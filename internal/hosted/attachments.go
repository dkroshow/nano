// attachments.go — durable attachments so hosted threads survive nano restarts
// (docs/decisions/supervised-app-server.md for codex,
// docs/decisions/claude-shim-daemon.md for claude). Boot re-attaches every
// surviving record automatically (boot_reattach.go); one that fails to resume
// is presented as a detached, one-click-resumable sidebar entry.
//
// Write-on-attach / remove-on-explicit-close is the whole design in one rule:
// a record that outlives a restart means the session was killed by the restart,
// not closed by the user, so it comes back as re-attachable. An explicit DELETE
// removes the record — user-closed stays closed, so the sidebar never fills
// with ghosts.
package hosted

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"

	filemodel "github.com/dkroshow/nano/internal/files"
	threadvalues "github.com/dkroshow/nano/internal/threads"
)

type attachmentRecord = Attachment
type attachmentKey = AttachmentKey

// AttachmentStore persists attachments to a JSON file beside the history DB.
// All exported operations are safe for concurrent use and persist the whole
// file atomically; the record count is bounded by open sessions, so rewriting
// the file per change is cheap.
type AttachmentStore struct {
	path  string
	mu    sync.Mutex
	byKey map[attachmentKey]attachmentRecord
}

// NewAttachmentStore loads durable attachments from path.
func NewAttachmentStore(path string) (*AttachmentStore, error) {
	s := &AttachmentStore{path: path, byKey: map[attachmentKey]attachmentRecord{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *AttachmentStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var records []attachmentRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return fmt.Errorf("parse attachments file %s: %w", s.path, err)
	}
	for _, r := range records {
		if r.NativeThreadID == "" {
			continue
		}
		r = r.normalized()
		s.byKey[r.Key()] = r
	}
	return nil
}

// Remember records or refreshes an attachment. Idempotent by (provider, native
// id), so a re-attach simply updates the session's cwd/model/time in place.
//
// ContinuedFrom, PendingName, and the turn anchor are carried forward rather
// than replaced: callers build the record from the live transport
// (claudeTransport.durableAttachment), which knows recovery handles but has no
// way to know what conversation it continued or what turn was in flight.
// A plain whole-record write would therefore erase both on every later
// re-attach — silently, and (for the anchor) at exactly the moment a re-attach
// replay is about to need it, since the replayed init line lands before the
// replay has finished.
func (s *AttachmentStore) Remember(r attachmentRecord) {
	if r.NativeThreadID == "" {
		return
	}
	r = r.normalized()
	// These retired metadata fields must not be inherited from an older record
	// during a transport-only refresh. control.sqlite is their sole authority.
	r.Cwd, r.Model, r.TurnServiceTier = "", "", ""
	s.mu.Lock()
	previous := s.byKey[r.Key()]
	if r.ContinuedFrom == "" {
		r.ContinuedFrom = previous.ContinuedFrom
	}
	if r.PendingName == "" {
		r.PendingName = previous.PendingName
	}
	if r.HostedThreadID == "" {
		r.HostedThreadID = previous.HostedThreadID
	}
	if r.PresentationMode == "" {
		r.PresentationMode = previous.PresentationMode
	}
	if r.TurnStartedAtMs == 0 {
		r.TurnStartedAtMs, r.TurnAnchorSeq = previous.TurnStartedAtMs, previous.TurnAnchorSeq
	}
	s.byKey[r.Key()] = r
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		log.Printf("persist attachment %s/%s: %v", r.Provider, r.NativeThreadID, err)
	}
}

// scrubRetiredMetadata removes posture facts that belonged to the old
// attachment authority. It persists one whole-file rewrite only when one of
// those fields is present. On write failure the in-memory map is restored, so
// callers never observe a scrub that did not become durable.
func (s *AttachmentStore) scrubRetiredMetadata() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	updated := make(map[attachmentKey]attachmentRecord, len(s.byKey))
	changed := false
	for key, record := range s.byKey {
		if record.Cwd != "" || record.Model != "" || record.TurnServiceTier != "" {
			record.Cwd, record.Model, record.TurnServiceTier = "", "", ""
			changed = true
		}
		updated[key] = record
	}
	if !changed {
		return nil
	}
	previous := s.byKey
	s.byKey = updated
	if err := s.persistLocked(); err != nil {
		s.byKey = previous
		return err
	}
	return nil
}

// SetTurnAnchor writes just the live-turn anchor onto an EXISTING record. It
// never creates one: a record means "this session survives a nano restart", and
// only a durable transport has earned that claim — a direct claude child dies
// with nano, so an anchor for it would offer a recovery that cannot happen.
// Passing 0/0 clears the anchor, which is what a turn ending means.
func (s *AttachmentStore) SetTurnAnchor(provider, nativeThreadID string, startedAtMs, anchorSeq int64) {
	key := attachmentKey{Provider: provider, NativeThreadID: nativeThreadID}
	s.mu.Lock()
	rec, ok := s.byKey[key]
	unchanged := ok && rec.TurnStartedAtMs == startedAtMs && rec.TurnAnchorSeq == anchorSeq
	var err error
	if ok && !unchanged {
		rec.TurnStartedAtMs = startedAtMs
		rec.TurnAnchorSeq = anchorSeq
		s.byKey[key] = rec
		err = s.persistLocked()
	}
	s.mu.Unlock()
	if err != nil {
		log.Printf("persist turn anchor %s/%s: %v", provider, nativeThreadID, err)
	}
}

// SetPendingName rewrites the staged title on an EXISTING provisional record —
// the pre-init rename path, where the ordinary names store still has no durable
// key to write. Like setTurnAnchor, it never creates a record: only a durable
// provisional attachment stages a name at all.
func (s *AttachmentStore) SetPendingName(provider, nativeThreadID, name string) {
	key := attachmentKey{Provider: provider, NativeThreadID: nativeThreadID}
	s.mu.Lock()
	rec, ok := s.byKey[key]
	var err error
	if ok && rec.PendingName != name {
		rec.PendingName = name
		s.byKey[key] = rec
		err = s.persistLocked()
	}
	s.mu.Unlock()
	if err != nil {
		log.Printf("persist pending name %s/%s: %v", provider, nativeThreadID, err)
	}
}

// SetShimSession writes just the shim session id onto an EXISTING record —
// the `--resume` respawn path: the record still names the predecessor's native
// id (the successor announces its own only with its first turn), but the child
// now hosting the thread is a new shim session, and a restart in that window
// must re-attach to it rather than spawn yet another `--resume` child and
// orphan this one. Like setTurnAnchor, it never creates a record.
func (s *AttachmentStore) SetShimSession(provider, nativeThreadID, shimSessionID string) {
	key := attachmentKey{Provider: provider, NativeThreadID: nativeThreadID}
	s.mu.Lock()
	rec, ok := s.byKey[key]
	var err error
	if ok && rec.ShimSessionID != shimSessionID {
		rec.ShimSessionID = shimSessionID
		s.byKey[key] = rec
		err = s.persistLocked()
	}
	s.mu.Unlock()
	if err != nil {
		log.Printf("persist shim session %s/%s: %v", provider, nativeThreadID, err)
	}
}

// Get returns one provider's record for a native id. The resume path's
// evidence source: it is what says where a detached thread lived (cwd) and with
// what posture (bin/args), neither of which may be guessed from New Window
// defaults.
func (s *AttachmentStore) Get(provider, nativeThreadID string) (attachmentRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byKey[attachmentKey{Provider: provider, NativeThreadID: nativeThreadID}]
	return rec, ok
}

// records returns all stored attachments.
func (s *AttachmentStore) records() []attachmentRecord {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]attachmentRecord, 0, len(s.byKey))
	for _, rec := range s.byKey {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HostedThreadID < out[j].HostedThreadID })
	return out
}

// Forget removes one provider's attachment for a native id — the
// remove-on-explicit-close half of the rule. A miss is a no-op that touches no
// disk.
func (s *AttachmentStore) Forget(provider, nativeThreadID string) {
	s.forgetWhere(nativeThreadID, func(k attachmentKey) bool {
		return k.Provider == provider && k.NativeThreadID == nativeThreadID
	})
}

// forgetClosed removes only the process generation we actually stopped.
func (s *AttachmentStore) forgetClosed(expected attachmentRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.byKey[expected.Key()]
	if !ok {
		return nil
	}
	if current.ShimSessionID != expected.ShimSessionID || current.HostedThreadID != expected.HostedThreadID || current.AttachedAt != expected.AttachedAt {
		return fmt.Errorf("attachment changed while closing; replacement was preserved")
	}
	delete(s.byKey, expected.Key())
	if err := s.persistLocked(); err != nil {
		s.byKey[expected.Key()] = current
		return err
	}
	return nil
}

// forgetEveryProvider removes a native id under every provider holding it — the
// dismiss path, whose route is keyed by native id alone. Native ids are UUIDs,
// so there is no realistic cross-provider collision this could over-delete.
func (s *AttachmentStore) forgetEveryProvider(nativeThreadID string) {
	s.forgetWhere(nativeThreadID, func(k attachmentKey) bool {
		return k.NativeThreadID == nativeThreadID
	})
}

// forgetWhere drops every matching record in one persist; label names the id
// in the failure log. Removing nothing touches no disk.
func (s *AttachmentStore) forgetWhere(label string, match func(attachmentKey) bool) {
	s.mu.Lock()
	removed := 0
	for k := range s.byKey {
		if match(k) {
			delete(s.byKey, k)
			removed++
		}
	}
	var err error
	if removed > 0 {
		err = s.persistLocked()
	}
	s.mu.Unlock()
	if err != nil {
		log.Printf("forget attachment %s: %v", label, err)
	}
}

// DetachedExcept returns remembered records whose thread is not currently in
// live, newest first. live is keyed like the store — attached sessions by
// (provider, native thread id); those are attached, not detached, and must not
// appear as ghosts.
func (s *AttachmentStore) DetachedExcept(live map[attachmentKey]bool) []attachmentRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]attachmentRecord, 0, len(s.byKey))
	for k, r := range s.byKey {
		if !live[k] {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return threadvalues.TimestampBefore(out[j].AttachedAt, out[i].AttachedAt) })
	return out
}

func (s *AttachmentStore) persistLocked() error {
	records := make([]attachmentRecord, 0, len(s.byKey))
	for _, r := range s.byKey {
		records = append(records, r)
	}
	// Stable on-disk order (provider, then id) keeps diffs readable and tests
	// deterministic.
	sort.Slice(records, func(i, j int) bool {
		if records[i].Provider != records[j].Provider {
			return records[i].Provider < records[j].Provider
		}
		return records[i].NativeThreadID < records[j].NativeThreadID
	})
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	// Use the shared atomic writer: temp + rename, so
	// a concurrent reader never sees a half-written file.
	return filemodel.WriteAtomic(s.path, data, 0o644)
}

// checkRetiredReferences rejects retirement of identities still referenced by attachments.
func (s *AttachmentStore) checkRetiredReferences(retired map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, attachment := range s.byKey {
		if retired[attachment.HostedThreadID] || retired[attachment.InvokerHostedThreadID] {
			return fmt.Errorf("retired identity still has a durable attachment reference")
		}
	}
	return nil
}

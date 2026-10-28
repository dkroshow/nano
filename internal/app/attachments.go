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
package app

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"

	filemodel "github.com/dkroshow/nano/internal/files"
	"github.com/dkroshow/nano/internal/hosted"
	threadvalues "github.com/dkroshow/nano/internal/threads"
)

type attachmentRecord = hosted.Attachment
type attachmentKey = hosted.AttachmentKey

// attachmentStore persists attachments to a JSON file beside the history DB.
// All exported operations are safe for concurrent use and persist the whole
// file atomically; the record count is bounded by open sessions, so rewriting
// the file per change is cheap.
type attachmentStore struct {
	path  string
	mu    sync.Mutex
	byKey map[attachmentKey]attachmentRecord
}

func newAttachmentStore(path string) (*attachmentStore, error) {
	s := &attachmentStore{path: path, byKey: map[attachmentKey]attachmentRecord{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *attachmentStore) load() error {
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
		r = r.Normalized()
		s.byKey[r.Key()] = r
	}
	return nil
}

// remember records or refreshes an attachment. Idempotent by (provider, native
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
func (s *attachmentStore) remember(r attachmentRecord) {
	if r.NativeThreadID == "" {
		return
	}
	r = r.Normalized()
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
func (s *attachmentStore) scrubRetiredMetadata() error {
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

// setTurnAnchor writes just the live-turn anchor onto an EXISTING record. It
// never creates one: a record means "this session survives a nano restart", and
// only a durable transport has earned that claim — a direct claude child dies
// with nano, so an anchor for it would offer a recovery that cannot happen.
// Passing 0/0 clears the anchor, which is what a turn ending means.
func (s *attachmentStore) setTurnAnchor(provider, nativeThreadID string, startedAtMs, anchorSeq int64) {
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

// setPendingName rewrites the staged title on an EXISTING provisional record —
// the pre-init rename path, where the ordinary names store still has no durable
// key to write. Like setTurnAnchor, it never creates a record: only a durable
// provisional attachment stages a name at all.
func (s *attachmentStore) setPendingName(provider, nativeThreadID, name string) {
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

// setShimSession writes just the shim session id onto an EXISTING record —
// the `--resume` respawn path: the record still names the predecessor's native
// id (the successor announces its own only with its first turn), but the child
// now hosting the thread is a new shim session, and a restart in that window
// must re-attach to it rather than spawn yet another `--resume` child and
// orphan this one. Like setTurnAnchor, it never creates a record.
func (s *attachmentStore) setShimSession(provider, nativeThreadID, shimSessionID string) {
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

// get returns one provider's record for a native id. The resume path's
// evidence source: it is what says where a detached thread lived (cwd) and with
// what posture (bin/args), neither of which may be guessed from New Window
// defaults.
func (s *attachmentStore) get(provider, nativeThreadID string) (attachmentRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byKey[attachmentKey{Provider: provider, NativeThreadID: nativeThreadID}]
	return rec, ok
}

func (s *attachmentStore) records() []attachmentRecord {
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

// forget removes one provider's attachment for a native id — the
// remove-on-explicit-close half of the rule. A miss is a no-op that touches no
// disk.
func (s *attachmentStore) forget(provider, nativeThreadID string) {
	s.forgetWhere(nativeThreadID, func(k attachmentKey) bool {
		return k.Provider == provider && k.NativeThreadID == nativeThreadID
	})
}

// forgetClosed removes only the process generation we actually stopped.
func (s *attachmentStore) forgetClosed(expected attachmentRecord) error {
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
func (s *attachmentStore) forgetEveryProvider(nativeThreadID string) {
	s.forgetWhere(nativeThreadID, func(k attachmentKey) bool {
		return k.NativeThreadID == nativeThreadID
	})
}

// forgetWhere drops every matching record in one persist; label names the id
// in the failure log. Removing nothing touches no disk.
func (s *attachmentStore) forgetWhere(label string, match func(attachmentKey) bool) {
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

// detachedExcept returns remembered records whose thread is not currently in
// live, newest first. live is keyed like the store — attached sessions by
// (provider, native thread id); those are attached, not detached, and must not
// appear as ghosts.
func (s *attachmentStore) detachedExcept(live map[attachmentKey]bool) []attachmentRecord {
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

func (s *attachmentStore) persistLocked() error {
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

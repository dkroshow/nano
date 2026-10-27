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
)

// attachmentProviderDefault is the provider assumed for records written before
// attachments were provider-aware: the store was codex-only by construction, so
// a missing provider can only mean codex.
const attachmentProviderDefault = "codex"

// A legacy Claude attachment has no spool/projection boundary. During its
// first re-attach, this sentinel suppresses the old spool transcript while the
// shim reports its current last sequence; the projection then rebases to that
// exact sequence before replay notifications are released.
// attachmentRecord is one durable transport attachment. Cwd, Model, and tier
// remain only so old files can be imported; live writes always scrub them.
//
// Bin and Args are claude-only: the resolved binary and the exact spawn args of
// the original child, stored so a `--resume` fallback can restore the session's
// posture flags (permission mode, allowed tools, model) instead of silently
// downgrading it. Codex records never set them — a codex resume gets its
// posture from the app-server's own thread state.
//
// ContinuedFrom names the native id this session succeeded, set only when a
// resume minted a new one (claude's `--resume` rung). It is provider-generic
// and write-only for now: the one durable trace that two native ids are the
// same conversation, kept so a later thread-lineage view can join them.
// Nothing reads it yet, so its absence on a record proves nothing.
// TurnStartedAtMs / TurnAnchorSeq are the claude live-turn anchor, persisted
// here because it is the only place nano can put it: the shim's spool stamps
// its own timestamps but never sends them, and its JSON-RPC surface is a frozen
// contract. Both are written when a turn opens and cleared when it closes, so a
// record carrying them describes a turn that was in flight when nano last
// looked. TurnAnchorSeq is the shim sequence number nano had already seen when
// it sent the turn's user line — sampled BEFORE the send, so every line the
// shim numbers from then on, the user line included, is strictly greater. That
// makes "the replayed user line's seq is greater than TurnAnchorSeq" hard
// identity evidence (D9/D11) rather than an assumption. A record written by an
// older nano simply has neither field, which degrades to no anchor — never to
// replay-arrival time.
// ShimSessionID is claude-only: the shim daemon's own handle for the child
// hosting this thread. It is what boot re-attach attaches by when the native
// id cannot answer — a child that has never been sent a turn announces no
// system.init, so its native id is unknown to nano AND to the shim's own
// byNativeID index. A record whose NativeThreadID equals its ShimSessionID is
// provisional (see provisional()): the thread has no native identity yet, and
// the shim child is the only thing the record can name.
type attachmentRecord struct {
	Provider              string `json:"provider"`
	NativeThreadID        string `json:"nativeThreadId"`
	HostedThreadID        string `json:"hostedThreadId,omitempty"`
	InvokerHostedThreadID string `json:"invokerHostedThreadId,omitempty"`
	InvokerNativeThreadID string `json:"invokerNativeThreadId,omitempty"`
	InvokerProvider       string `json:"invokerProvider,omitempty"`
	LegacyAgentSessionID  string `json:"agentSessionId,omitempty"`
	PresentationMode      string `json:"presentationMode,omitempty"`
	Cwd                   string `json:"cwd,omitempty"`
	Model                 string `json:"model,omitempty"`
	// PendingName exists only while a Claude child still has a provisional
	// shim identity. The real native id has not landed yet, so the ordinary
	// thread-name store has no durable key to write; this carries the operator's
	// New Window title across a Nano restart until captureInit can apply it.
	PendingName     string   `json:"pendingName,omitempty"`
	AttachedAt      string   `json:"attachedAt"` // RFC3339
	Bin             string   `json:"bin,omitempty"`
	Args            []string `json:"args,omitempty"`
	ShimSessionID   string   `json:"shimSessionId,omitempty"`
	ContinuedFrom   string   `json:"continuedFrom,omitempty"`
	TurnStartedAtMs int64    `json:"turnStartedAtMs,omitempty"`
	TurnAnchorSeq   int64    `json:"turnAnchorSeq,omitempty"`
	TurnServiceTier string   `json:"turnServiceTier,omitempty"`
}

// provisional reports a record for a thread that was started but never
// prompted: no system.init has landed, so the shim session id is standing in
// for the native id. Such a thread has no transcript on disk — the `--resume`
// fallback rung does not exist for it, and losing the shim child loses the
// (empty) conversation for good.
func (r attachmentRecord) provisional() bool {
	return r.ShimSessionID != "" && r.NativeThreadID == r.ShimSessionID
}

// attachmentKey is a record's identity: the native thread id survives a restart
// (the nano session id does not), but providers mint their ids independently,
// so the pair — not the id alone — addresses one attachment.
type attachmentKey struct {
	provider       string
	nativeThreadID string
}

// normalized fills in the provider a legacy record lacks, so every stored
// record is addressable and every rewrite of the file carries the field.
func (r attachmentRecord) normalized() attachmentRecord {
	if r.Provider == "" {
		r.Provider = attachmentProviderDefault
	}
	if r.HostedThreadID == "" {
		r.HostedThreadID = r.LegacyAgentSessionID
	}
	r.LegacyAgentSessionID = ""
	return r
}

func (r attachmentRecord) key() attachmentKey {
	return attachmentKey{provider: r.Provider, nativeThreadID: r.NativeThreadID}
}

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
		r = r.normalized()
		s.byKey[r.key()] = r
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
	r = r.normalized()
	// These retired metadata fields must not be inherited from an older record
	// during a transport-only refresh. control.sqlite is their sole authority.
	r.Cwd, r.Model, r.TurnServiceTier = "", "", ""
	s.mu.Lock()
	previous := s.byKey[r.key()]
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
	s.byKey[r.key()] = r
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
	key := attachmentKey{provider: provider, nativeThreadID: nativeThreadID}
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
	key := attachmentKey{provider: provider, nativeThreadID: nativeThreadID}
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
	key := attachmentKey{provider: provider, nativeThreadID: nativeThreadID}
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
	rec, ok := s.byKey[attachmentKey{provider: provider, nativeThreadID: nativeThreadID}]
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
		return k.provider == provider && k.nativeThreadID == nativeThreadID
	})
}

// forgetClosed removes only the process generation we actually stopped.
func (s *attachmentStore) forgetClosed(expected attachmentRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.byKey[expected.key()]
	if !ok {
		return nil
	}
	if current.ShimSessionID != expected.ShimSessionID || current.HostedThreadID != expected.HostedThreadID || current.AttachedAt != expected.AttachedAt {
		return fmt.Errorf("attachment changed while closing; replacement was preserved")
	}
	delete(s.byKey, expected.key())
	if err := s.persistLocked(); err != nil {
		s.byKey[expected.key()] = current
		return err
	}
	return nil
}

// forgetEveryProvider removes a native id under every provider holding it — the
// dismiss path, whose route is keyed by native id alone. Native ids are UUIDs,
// so there is no realistic cross-provider collision this could over-delete.
func (s *attachmentStore) forgetEveryProvider(nativeThreadID string) {
	s.forgetWhere(nativeThreadID, func(k attachmentKey) bool {
		return k.nativeThreadID == nativeThreadID
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
	sort.Slice(out, func(i, j int) bool { return timestampBefore(out[j].AttachedAt, out[i].AttachedAt) })
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
	// Reuse the shared atomic writer (instruction_files.go): temp + rename, so
	// a concurrent reader never sees a half-written file.
	return writeFileAtomic(s.path, data, 0o644)
}

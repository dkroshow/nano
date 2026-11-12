// attachments.go — every hosted thread Nano can resume, and its state, so
// hosted threads survive Nano restarts (docs/decisions/supervised-app-server.md
// for codex, docs/decisions/claude-shim-daemon.md for claude). Boot re-attaches
// every active row (boot_reattach.go); one that fails to resume is presented as
// a detached, one-click-resumable sidebar entry.
//
// Each hosted thread has one row in hosted_attachments, with an explicit state:
//
//   - attached: a durable session is live (written by its handshake), or was
//     live when Nano stopped;
//   - resumable: no session is live, and Nano can bring the thread back;
//   - finished: an explicit close, a cleanup agent, or Nano retiring a handle
//     that can never resume ended it. The row is kept with who and when.
//
// Nano never changes a state on a timer. Only an explicit resume reopens a
// finished thread; boot and the sidebar read active rows only.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	threadvalues "github.com/dkroshow/nano/internal/threads"
)

const (
	// AttachmentAttached marks a row whose durable session is live.
	AttachmentAttached = "attached"
	// AttachmentResumable marks a row with no live session that Nano can resume.
	AttachmentResumable = "resumable"
	// AttachmentFinished marks a row closed for good.
	AttachmentFinished = "finished"
	// FinishedByNano names Nano itself as the actor that finished a row.
	FinishedByNano = "nano"
)

// AttachmentStore keeps the active rows of hosted_attachments in memory, loaded
// by one indexed query, and writes every change through to the database
// before it is visible. The database wins: memory holds only what it last
// wrote there. All exported operations are safe for concurrent use.
type AttachmentStore struct {
	db    *sql.DB
	mu    sync.Mutex
	byKey map[AttachmentKey]Attachment
}

const attachmentColumns = `a.hosted_thread_id,a.provider,a.native_thread_id,a.state,a.bin,a.args_json,a.shim_session_id,a.pending_name,a.continued_from,a.turn_started_at_ms,a.turn_anchor_seq,a.attached_at,
	COALESCE(h.presentation_mode,''),COALESCE(h.invoker_hosted_id,''),COALESCE(h.invoker_native_id,''),COALESCE(h.invoker_provider,'')`

// NewAttachmentStore reads the active rows from the canonical threads
// database. Nothing is live when it opens, so a row still attached was live
// when Nano last stopped; it is counted for the log and becomes resumable
// until its session comes back.
func NewAttachmentStore(db *sql.DB) (*AttachmentStore, error) {
	if db == nil {
		return nil, errors.New("attachment store: threads database is required")
	}
	s := &AttachmentStore{db: db, byKey: map[AttachmentKey]Attachment{}}
	rows, err := db.Query(`SELECT ` + attachmentColumns + ` FROM hosted_attachments a LEFT JOIN hosted_threads h ON h.hosted_thread_id=a.hosted_thread_id WHERE a.state <> 'finished'`)
	if err != nil {
		return nil, fmt.Errorf("attachment store: read active rows: %w", err)
	}
	defer rows.Close()
	wasLive := 0
	for rows.Next() {
		var r Attachment
		var args string
		if err := rows.Scan(&r.HostedThreadID, &r.Provider, &r.NativeThreadID, &r.State, &r.Bin, &args, &r.ShimSessionID, &r.PendingName, &r.ContinuedFrom, &r.TurnStartedAtMs, &r.TurnAnchorSeq, &r.AttachedAt,
			&r.PresentationMode, &r.InvokerHostedThreadID, &r.InvokerNativeThreadID, &r.InvokerProvider); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(args), &r.Args); err != nil {
			return nil, fmt.Errorf("attachment store: arguments of %s: %w", r.HostedThreadID, err)
		}
		if len(r.Args) == 0 {
			r.Args = nil
		}
		if r.State == AttachmentAttached {
			wasLive++
			r.State = AttachmentResumable
		}
		s.byKey[r.Key()] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`UPDATE hosted_attachments SET state='resumable' WHERE state='attached'`); err != nil {
		return nil, fmt.Errorf("attachment store: mark rows resumable at start: %w", err)
	}
	log.Printf("hosted attachments: %d active, %d were live when Nano stopped", len(s.byKey), wasLive)
	return s, nil
}

// Remember records a live durable session as its hosted thread's attached
// row. One row per hosted thread: a re-key (a provisional Claude handle
// resolving, or a `--resume` successor) moves the row to the new native id.
//
// ContinuedFrom, PendingName, and the turn anchor are carried forward only
// from a row naming the same native id: callers build the record from the
// live transport (claudeTransport.durableAttachment), which knows recovery
// handles but has no way to know what conversation it continued or what turn
// was in flight. A plain whole-record write would therefore erase both on
// every later re-attach — silently, and (for the anchor) at exactly the moment
// a re-attach replay is about to need it, since the replayed init line lands
// before the replay has finished. A new native id starts clean, so a staged
// title never outlives its provisional handle.
func (s *AttachmentStore) Remember(r Attachment) {
	if r.NativeThreadID == "" {
		return
	}
	r = r.normalized()
	// Posture is canonical in hosted metadata, never in this row.
	r.Cwd, r.Model, r.TurnServiceTier = "", "", ""
	if r.HostedThreadID == "" {
		log.Printf("attachment %s/%s has no hosted thread; not stored", r.Provider, r.NativeThreadID)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.byKey[r.Key()]; ok && previous.HostedThreadID == r.HostedThreadID {
		if r.ContinuedFrom == "" {
			r.ContinuedFrom = previous.ContinuedFrom
		}
		if r.PendingName == "" {
			r.PendingName = previous.PendingName
		}
		if r.PresentationMode == "" {
			r.PresentationMode = previous.PresentationMode
		}
		if r.TurnStartedAtMs == 0 {
			r.TurnStartedAtMs, r.TurnAnchorSeq = previous.TurnStartedAtMs, previous.TurnAnchorSeq
		}
	}
	r.State = AttachmentAttached
	args, err := json.Marshal(r.Args)
	if err != nil || r.Args == nil {
		args = []byte("[]")
	}
	var reopened string
	err = s.db.QueryRow(`SELECT finished_by FROM hosted_attachments WHERE hosted_thread_id=? AND state='finished'`, r.HostedThreadID).Scan(&reopened)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("persist attachment %s: %v", r.HostedThreadID, err)
		return
	}
	if _, err := s.db.Exec(`INSERT INTO hosted_attachments(hosted_thread_id,provider,native_thread_id,state,bin,args_json,shim_session_id,pending_name,continued_from,turn_started_at_ms,turn_anchor_seq,attached_at)
		VALUES(?,?,?,'attached',?,?,?,?,?,?,?,?)
		ON CONFLICT(hosted_thread_id) DO UPDATE SET provider=excluded.provider,native_thread_id=excluded.native_thread_id,state='attached',bin=excluded.bin,args_json=excluded.args_json,
			shim_session_id=excluded.shim_session_id,pending_name=excluded.pending_name,continued_from=excluded.continued_from,turn_started_at_ms=excluded.turn_started_at_ms,
			turn_anchor_seq=excluded.turn_anchor_seq,attached_at=excluded.attached_at,finished_at='',finished_by=''`,
		r.HostedThreadID, r.Provider, r.NativeThreadID, r.Bin, string(args), r.ShimSessionID, r.PendingName, r.ContinuedFrom, r.TurnStartedAtMs, r.TurnAnchorSeq, r.AttachedAt); err != nil {
		log.Printf("persist attachment %s/%s: %v", r.Provider, r.NativeThreadID, err)
		return
	}
	if reopened != "" {
		log.Printf("attachment %s reopened by an explicit resume (finished by %s)", r.HostedThreadID, reopened)
	}
	for key, rec := range s.byKey {
		if rec.HostedThreadID == r.HostedThreadID && key != r.Key() {
			delete(s.byKey, key)
		}
	}
	s.byKey[r.Key()] = r
}

// SetLive records whether the session named by provider/native is live. It
// only moves an active row that still names that native id between attached
// and resumable; it never creates, finishes or reopens one.
func (s *AttachmentStore) SetLive(provider, nativeThreadID string, live bool) {
	state := AttachmentResumable
	if live {
		state = AttachmentAttached
	}
	s.update(provider, nativeThreadID, "state", func(rec *Attachment) any {
		if rec.State == state {
			return nil
		}
		rec.State = state
		return state
	})
}

// SetTurnAnchor writes just the live-turn anchor onto an EXISTING record. It
// never creates one: a record means "this session survives a nano restart", and
// only a durable transport has earned that claim — a direct claude child dies
// with nano, so an anchor for it would offer a recovery that cannot happen.
// Passing 0/0 clears the anchor, which is what a turn ending means.
func (s *AttachmentStore) SetTurnAnchor(provider, nativeThreadID string, startedAtMs, anchorSeq int64) {
	key := AttachmentKey{Provider: provider, NativeThreadID: nativeThreadID}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byKey[key]
	if !ok || (rec.TurnStartedAtMs == startedAtMs && rec.TurnAnchorSeq == anchorSeq) {
		return
	}
	if _, err := s.db.Exec(`UPDATE hosted_attachments SET turn_started_at_ms=?,turn_anchor_seq=? WHERE hosted_thread_id=?`, startedAtMs, anchorSeq, rec.HostedThreadID); err != nil {
		log.Printf("persist turn anchor %s/%s: %v", provider, nativeThreadID, err)
		return
	}
	rec.TurnStartedAtMs, rec.TurnAnchorSeq = startedAtMs, anchorSeq
	s.byKey[key] = rec
}

// SetPendingName rewrites the staged title on an EXISTING provisional record —
// the pre-init rename path, where the ordinary names store still has no durable
// key to write. Like setTurnAnchor, it never creates a record: only a durable
// provisional attachment stages a name at all.
func (s *AttachmentStore) SetPendingName(provider, nativeThreadID, name string) {
	s.update(provider, nativeThreadID, "pending_name", func(rec *Attachment) any {
		if rec.PendingName == name {
			return nil
		}
		rec.PendingName = name
		return name
	})
}

// SetShimSession writes just the shim session id onto an EXISTING record —
// the `--resume` respawn path: the record still names the predecessor's native
// id (the successor announces its own only with its first turn), but the child
// now hosting the thread is a new shim session, and a restart in that window
// must re-attach to it rather than spawn yet another `--resume` child and
// orphan this one. Like setTurnAnchor, it never creates a record.
func (s *AttachmentStore) SetShimSession(provider, nativeThreadID, shimSessionID string) {
	s.update(provider, nativeThreadID, "shim_session_id", func(rec *Attachment) any {
		if rec.ShimSessionID == shimSessionID {
			return nil
		}
		rec.ShimSessionID = shimSessionID
		return shimSessionID
	})
}

// update writes one column of an existing active row when change reports a
// new value (nil means unchanged).
func (s *AttachmentStore) update(provider, nativeThreadID, column string, change func(*Attachment) any) {
	key := AttachmentKey{Provider: provider, NativeThreadID: nativeThreadID}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byKey[key]
	if !ok {
		return
	}
	value := change(&rec)
	if value == nil {
		return
	}
	if _, err := s.db.Exec(`UPDATE hosted_attachments SET `+column+`=? WHERE hosted_thread_id=? AND state <> 'finished'`, value, rec.HostedThreadID); err != nil {
		log.Printf("persist attachment %s %s/%s: %v", column, provider, nativeThreadID, err)
		return
	}
	s.byKey[key] = rec
}

// Get returns the active row for a native id. The resume path's evidence
// source: it is what says with what posture (bin/args) a detached thread was
// started, which may not be guessed from New Window defaults.
func (s *AttachmentStore) Get(provider, nativeThreadID string) (Attachment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byKey[AttachmentKey{Provider: provider, NativeThreadID: nativeThreadID}]
	return rec, ok
}

// Records returns every active row, ordered by hosted thread.
func (s *AttachmentStore) Records() []Attachment {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Attachment, 0, len(s.byKey))
	for _, rec := range s.byKey {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HostedThreadID < out[j].HostedThreadID })
	return out
}

// Forget finishes the row still named by a Claude handle that can never
// resume: a never-prompted child whose process is gone, or a predecessor a
// non-durable successor replaced. A re-key has already moved the row to its
// new native id, so then nothing matches and nothing changes.
func (s *AttachmentStore) Forget(provider, nativeThreadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byKey[AttachmentKey{Provider: provider, NativeThreadID: nativeThreadID}]
	if !ok {
		return
	}
	if err := s.finishLocked(rec, FinishedByNano); err != nil {
		log.Printf("finish attachment %s/%s: %v", provider, nativeThreadID, err)
	}
}

// Finish ends the process generation that was actually stopped, recording
// who ended it. A row that changed meanwhile (a new shim child, another
// thread, a later attach) is left active.
func (s *AttachmentStore) Finish(expected Attachment, actor string) error {
	if actor == "" {
		return errors.New("finish attachment: actor is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.byKey[expected.Key()]
	if !ok {
		return nil
	}
	if current.ShimSessionID != expected.ShimSessionID || current.HostedThreadID != expected.HostedThreadID || current.AttachedAt != expected.AttachedAt {
		return fmt.Errorf("attachment changed while closing; replacement was preserved")
	}
	return s.finishLocked(current, actor)
}

func (s *AttachmentStore) finishLocked(rec Attachment, actor string) error {
	if _, err := s.db.Exec(`UPDATE hosted_attachments SET state='finished',finished_at=?,finished_by=? WHERE hosted_thread_id=? AND state <> 'finished'`,
		time.Now().UTC().Format(time.RFC3339Nano), actor, rec.HostedThreadID); err != nil {
		return err
	}
	delete(s.byKey, rec.Key())
	return nil
}

// DetachedExcept returns active rows whose thread is not currently in live,
// newest first. live is keyed like the store — attached sessions by
// (provider, native thread id); those are attached, not detached, and must not
// appear as ghosts.
func (s *AttachmentStore) DetachedExcept(live map[AttachmentKey]bool) []Attachment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Attachment, 0, len(s.byKey))
	for k, r := range s.byKey {
		if !live[k] {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return threadvalues.TimestampBefore(out[j].AttachedAt, out[i].AttachedAt) })
	return out
}

// CheckRetiredReferences rejects retirement of identities still referenced by active rows.
func (s *AttachmentStore) CheckRetiredReferences(retired map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, attachment := range s.byKey {
		if retired[attachment.HostedThreadID] || retired[attachment.InvokerHostedThreadID] {
			return fmt.Errorf("retired identity still has a durable attachment reference")
		}
	}
	return nil
}

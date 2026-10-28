package app

import (
	"encoding/json"
	"log"
	"maps"
	"reflect"
	goruntime "runtime"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Turn lifecycle wire vocabulary, verified against
// internal/wire/codex/schema/codex_app_server_protocol.v2.schemas.json (ServerNotification)
// and internal/wire/codex/captures/first-turn-annotated.md: the codex lifecycle is exactly
// turn/started and turn/completed — failure and interruption arrive as
// turn/completed with turn.status failed|interrupted, not as separate methods.
// On the Claude wire a turn opens with Nano's own sent user line OR received
// execution evidence (notification-started turns bypass Nano's send path), and
// closes with the received result line.
const (
	codexTurnStartedMethod   = "turn/started"
	codexTurnCompletedMethod = "turn/completed"
	claudeUserLineType       = "user"
	claudeResultLineType     = "result"
)

// sessionStatusExited is the terminal session status. An exited session lingers
// in s.sessions until someone closes it — its child is gone, so nothing that
// asks "is this thread alive?" may count it as live.
const sessionStatusExited = "exited"

// runtimeInfo is the volatile per-thread runtime overlay attached to catalog
// responses: `attached` when a nano session hosts the thread (with sessionId,
// status starting|ready, turnActive), `detached` when no session does but a
// durable attachment record survives — a thread nano can re-attach in one
// click. Absent field = neither. The detached shape carries no session fields,
// so they are omitempty rather than serialized as empty lies.
type runtimeInfo struct {
	Attached   bool   `json:"attached"`
	Detached   bool   `json:"detached"`
	SessionID  string `json:"sessionId,omitempty"`
	Status     string `json:"status,omitempty"`
	TurnActive bool   `json:"turnActive,omitempty"`
	// ThreadStatus is the session's turn state, the same value the pane
	// reads. It rides every runtime delta, so the row it patches stays in step.
	ThreadStatus string `json:"threadStatus,omitempty"`
	// TurnStartedAtMs is the working indicator's anchor — immutable, ms epoch,
	// absent when there is no turn or no evidence of when it began (D2/D4/D8).
	// It travels in the window refetch the payload-free runtime SSE nudge
	// triggers; the nudge itself stays payload-free.
	TurnStartedAtMs int64 `json:"turnStartedAtMs,omitempty"`
	// CompactingSinceMs is when the provider began compacting the turn's
	// context; absent while it is not. The row shows a marker meanwhile.
	CompactingSinceMs int64 `json:"compactingSinceMs,omitempty"`
	// LastTurnEndedAt is the server wall-clock instant of the most recent
	// hosted turn completion. It is process-local and absent for detached rows.
	LastTurnEndedAt string `json:"lastTurnEndedAt,omitempty"`
	// Waiting means a parked interaction is holding the turn: the row shows
	// waiting rather than a ticking elapsed (D15 — waiting is not working).
	// Without this field the rail has no path at all from pending interactions,
	// which is why park/claim/drain now nudge the runtime stream.
	Waiting bool `json:"waiting,omitempty"`
	// NeedsAttention is durable execution evidence that a background session's
	// latest turn failed. Compact clients promote it even when the group is shut.
	NeedsAttention bool `json:"needsAttention,omitempty"`
	// PresentationMode is UI-only session metadata. Background sessions remain
	// fully hosted and inspectable; compact clients may group or suppress them.
	PresentationMode string `json:"presentationMode,omitempty"`
	// ThreadDiagnostics is the thread's bounded list of delivery and
	// session facts (thread_diagnostics.go).
	ThreadDiagnostics []threadDiagnostic `json:"threadDiagnostics,omitempty"`
	// Error, MetadataDegraded and MetadataRevision let a browser keep its
	// session list current from runtime events alone: a spawn failure, an
	// ingestion warning, and a canonical metadata change it should re-read.
	// MetadataRevision is the latest revision Nano has published since boot,
	// absent when it has published none for the thread.
	Error            string `json:"error,omitempty"`
	MetadataDegraded bool   `json:"metadataDegraded,omitempty"`
	MetadataRevision int64  `json:"metadataRevision,omitempty"`
}

// runtimeSource is the narrow seam between the catalog API and session-land:
// the server implements it over its sessions map, and a nil source (tests,
// catalog-only wiring) simply leaves the overlay absent.
type runtimeSource interface {
	RuntimeFor(provider, nativeThreadID string) (runtimeInfo, bool)
}

// RuntimeFor reports the runtime overlay for (provider, nativeThreadID) — the
// catalog-facing half of sessionForThread, extended with the remembered feed:
// a live session makes the thread attached, otherwise a surviving attachment
// record makes it detached.
//
// An exited session never reaches this scan (sessionForThread skips corpses):
// its child is dead, and its attachment record (if any) still stands, so the
// thread reads detached — exactly the post-crash reality the sidebar must
// show, with re-attach as the offered action.
func (s *server) RuntimeFor(provider, nativeThreadID string) (runtimeInfo, bool) {
	if sess := s.sessionForThread(provider, nativeThreadID); sess != nil {
		return sessionRuntimeInfo(sess.summary()), true
	}
	if rec, remembered := s.attachmentFor(provider, nativeThreadID); remembered {
		return s.detachedRuntimeInfos([]attachmentRecord{rec})[rec.Key()], true
	}
	return runtimeInfo{}, false
}

// detachedRuntimeInfos projects detached threads' overlay rows. Presentation
// comes from the canonical hosted record when one exists — the mode is mutable
// now (setPresentation), and the attachment JSON copy may be stale between the
// flip and the record's next transport rewrite. The attachment value remains
// only as the fallback for legacy records that predate hosted identity. The
// modes are read in one batch so snapshots do not read once per thread behind
// each registration's write.
func (s *server) detachedRuntimeInfos(records []attachmentRecord) map[attachmentKey]runtimeInfo {
	ids := make([]string, 0, len(records))
	for _, rec := range records {
		ids = append(ids, rec.HostedThreadID)
	}
	modes := s.agentExecutions.presentationModes(ids)
	infos := make(map[attachmentKey]runtimeInfo, len(records))
	for _, rec := range records {
		mode := rec.PresentationMode
		if canonical, ok := modes[rec.HostedThreadID]; ok {
			mode = canonical
		}
		info := runtimeInfo{Detached: true, PresentationMode: mode}
		info.Status = s.boot.state(rec.Key())
		infos[rec.Key()] = info
	}
	return infos
}

// sessionRuntimeInfo projects one live session's summary into the attached
// overlay shape. Shared by RuntimeFor and the catalog's synthetic memory rows
// so the two paths cannot drift about what "attached" carries — the memory-row
// bug class the unified catalog already learned once.
func sessionRuntimeInfo(sum sessionSummary) runtimeInfo {
	return runtimeInfo{
		Attached:          true,
		SessionID:         sum.ID,
		Status:            sum.Status,
		TurnActive:        sum.TurnActive,
		ThreadStatus:      sum.ThreadStatus,
		TurnStartedAtMs:   sum.TurnStartedAtMs,
		CompactingSinceMs: sum.CompactingSinceMs,
		LastTurnEndedAt:   sum.LastTurnEndedAt,
		Waiting:           sum.Waiting,
		NeedsAttention:    sum.NeedsAttention,
		PresentationMode:  sum.PresentationMode,
		ThreadDiagnostics: sum.ThreadDiagnostics,
		Error:             sum.Error,
		MetadataDegraded:  sum.MetadataDegraded,
	}
}

type runtimeDeltaRow struct {
	Provider string       `json:"provider"`
	NativeID string       `json:"nativeId"`
	Runtime  *runtimeInfo `json:"runtime"`
}

// runtimeDelta carries the overlay of each thread whose runtime changed since
// the last snapshot, and a nil overlay for each thread Nano no longer holds;
// threads it does not name are unchanged. The first snapshot names every
// thread. The one-slot hub merges a pending delta into the next by thread, so
// a subscriber that misses an intermediate turnActive flip still receives the
// latest truth, including removals. Refresh is reserved for identity and
// attachment changes; ordinary turn-state changes patch rows in place. Rows is
// always present on the wire: a browser reads a missing rows field as "read
// the whole list", which an unchanged snapshot must never ask for.
type runtimeDelta struct {
	Rows []runtimeDeltaRow `json:"rows"`
	// Refresh asks browsers to re-read the session list: something changed
	// that this delta does not carry (a metadata or star change, a boot).
	Refresh bool `json:"refresh,omitempty"`
	// A change to the live session set or to an attachment travels here
	// instead of as Refresh: the list rows of the sessions that joined or
	// changed, the ids of those that left, and likewise the detached rows
	// that joined or changed and the keys ("provider:nativeThreadId") of those
	// that left. A browser on a slow link then never re-reads every session
	// for one, and a restart's re-attach, which drains the detached list one
	// thread at a time, costs a few rows per change instead of the list.
	Sessions        []listedSession       `json:"sessions,omitempty"`
	RemovedSessions []string              `json:"removedSessions,omitempty"`
	DetachedRows    []detachedSessionView `json:"detachedRows,omitempty"`
	RemovedDetached []string              `json:"removedDetached,omitempty"`
	// Seq is the delta's place in this process's runtime stream (the latest
	// one merged in), so a browser can name where it left off on reconnect.
	Seq uint64 `json:"seq,omitempty"`
	// rebuild tells the sidebar materializer the held set changed (Refresh
	// does too); sidebarCurrent says a star update already applied it.
	rebuild        bool
	sidebarCurrent bool
}

// runtimeOverlays is the runtime overlay of every thread Nano holds: the live
// sessions in summaries (the newest wins a shared native id), then the
// detached attachments. The runtime delta and the materialized sidebar both
// read it, so a sidebar rebuild agrees with the last delta about every row.
// When they disagreed on one field, each rebuild republished every row.
func (s *server) runtimeOverlays(summaries []sessionSummary) map[attachmentKey]runtimeInfo {
	overlays := make(map[attachmentKey]runtimeInfo)
	live := make(map[attachmentKey]bool)
	for _, sum := range summaries {
		if sum.ThreadID == "" {
			continue
		}
		key := attachmentKey{Provider: sum.Provider, NativeThreadID: sum.ThreadID}
		live[key] = true
		info := sessionRuntimeInfo(sum)
		if sum.HostedThreadID != "" {
			info.MetadataRevision = s.controlMetadata.latestRevision(sum.HostedThreadID)
		}
		overlays[key] = info
	}
	if s.attachments != nil {
		for key, info := range s.detachedRuntimeInfos(s.attachments.detachedExcept(live)) {
			overlays[key] = info
		}
	}
	return overlays
}

func (s *server) runtimeDeltaSnapshot() runtimeDelta {
	summaries := s.volatileSessionSummaries()
	current := s.runtimeOverlays(summaries)
	// The session list holds every registered session, an exited or failed
	// one included. Sessions without a native id have no row, so a browser
	// learns them only from list rows: any change to the listed set, to such a
	// session's status, or to a session leaving the live set travels as one.
	sessions := make(map[string]string)
	for _, sum := range summaries {
		if sum.ThreadID == "" {
			sessions[sum.ID] = "pending:" + sum.Status
			continue
		}
		sessions[sum.ID] = ""
	}
	for _, sess := range s.snapshotSessions() {
		if _, live := sessions[sess.id]; !live {
			sessions[sess.id] = sessionStatusExited
		}
	}

	s.runtimeSnapshotMu.Lock()
	previous, previousSessions := s.runtimeSnapshot, s.runtimeSessions
	refresh := previousSessions != nil && !maps.Equal(previousSessions, sessions)
	s.runtimeSessions = sessions
	for key, before := range previous {
		after, ok := current[key]
		if !ok || before.Attached != after.Attached || before.Detached != after.Detached {
			refresh = true
			break
		}
	}
	if !refresh {
		for key := range current {
			if _, ok := previous[key]; !ok {
				refresh = true
				break
			}
		}
	}
	s.runtimeSnapshot = current
	s.runtimeSnapshotMu.Unlock()

	rows := make([]runtimeDeltaRow, 0)
	for key, runtime := range current {
		if before, ok := previous[key]; ok && reflect.DeepEqual(before, runtime) {
			continue
		}
		runtimeCopy := runtime
		rows = append(rows, runtimeDeltaRow{
			Provider: key.Provider,
			NativeID: key.NativeThreadID,
			Runtime:  &runtimeCopy,
		})
	}
	for key := range previous {
		if _, ok := current[key]; !ok {
			rows = append(rows, runtimeDeltaRow{Provider: key.Provider, NativeID: key.NativeThreadID})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Provider != rows[j].Provider {
			return rows[i].Provider < rows[j].Provider
		}
		return rows[i].NativeID < rows[j].NativeID
	})
	delta := runtimeDelta{Rows: rows, rebuild: refresh}
	switch {
	case !refresh:
	case previousSessions == nil:
		delta.Refresh = true // the first snapshot has no change to carry
		s.detachedChanges(liveKeys(summaries))
	default:
		s.attachSessionChanges(&delta, previousSessions, sessions, summaries)
	}
	return delta
}

// runtimeChangeLimit bounds the rows of any kind one delta carries. A larger
// change is sent as one Refresh, a single list read, rather than as a burst
// of rows.
const runtimeChangeLimit = 64

// bounded is the delta a subscriber receives. Past runtimeChangeLimit rows,
// pending merges included, it is a refresh marker with no rows: one list
// read and one sidebar rebuild cover what the rows would have said.
func (d runtimeDelta) bounded() runtimeDelta {
	if len(d.Rows)+len(d.Sessions)+len(d.RemovedSessions)+len(d.DetachedRows)+len(d.RemovedDetached) <= runtimeChangeLimit {
		return d
	}
	return runtimeDelta{Rows: []runtimeDeltaRow{}, Refresh: true, rebuild: true, Seq: d.Seq}
}

// attachSessionChanges fills a delta's session fields from the change between
// two snapshots of the live session set and
// the detached set since the last change.
func (s *server) attachSessionChanges(delta *runtimeDelta, before, after map[string]string, summaries []sessionSummary) {
	var changed []*session
	for _, sess := range s.snapshotSessions() {
		if state, ok := after[sess.id]; ok {
			if previous, ok := before[sess.id]; !ok || previous != state {
				changed = append(changed, sess)
			}
		}
	}
	for id := range before {
		if _, ok := after[id]; !ok {
			delta.RemovedSessions = append(delta.RemovedSessions, id)
		}
	}
	sort.Strings(delta.RemovedSessions)
	changedDetached, removedDetached := s.detachedChanges(liveKeys(summaries))
	if len(changed)+len(delta.RemovedSessions)+len(changedDetached)+len(removedDetached) > runtimeChangeLimit {
		// Too large to send as rows: the same empty refresh marker bounded
		// sends, without building rows that would be dropped.
		*delta = runtimeDelta{Rows: []runtimeDeltaRow{}, Refresh: true, rebuild: true}
		return
	}
	if len(changed) > 0 {
		delta.Sessions = s.listedSessions(changed)
	}
	if len(changedDetached) > 0 {
		delta.DetachedRows = s.detachedViewsOf(changedDetached)
	}
	delta.RemovedDetached = removedDetached
}

// liveKeys names the threads of live sessions, which no detached row repeats.
func liveKeys(summaries []sessionSummary) map[attachmentKey]bool {
	live := make(map[attachmentKey]bool, len(summaries))
	for _, sum := range summaries {
		if sum.ThreadID != "" && sum.Status != sessionStatusExited {
			live[attachmentKey{Provider: sum.Provider, NativeThreadID: sum.ThreadID}] = true
		}
	}
	return live
}

// detachedChanges returns the detached records that joined or changed since
// the last call and the wire keys of those that left, and remembers the
// current set. The first call reports every record.
func (s *server) detachedChanges(live map[attachmentKey]bool) ([]attachmentRecord, []string) {
	var records []attachmentRecord
	if s.attachments != nil {
		records = s.attachments.detachedExcept(live)
	}
	current := make(map[attachmentKey]string, len(records))
	for _, record := range records {
		state, _ := json.Marshal(record)
		current[record.Key()] = string(state) + "|" + s.boot.state(record.Key())
	}
	s.runtimeSnapshotMu.Lock()
	previous := s.runtimeDetached
	s.runtimeDetached = current
	s.runtimeSnapshotMu.Unlock()
	var changed []attachmentRecord
	for _, record := range records {
		if before, ok := previous[record.Key()]; !ok || before != current[record.Key()] {
			changed = append(changed, record)
		}
	}
	var removed []string
	for key := range previous {
		if _, ok := current[key]; !ok {
			removed = append(removed, key.Provider+":"+key.NativeThreadID)
		}
	}
	sort.Strings(removed)
	return changed, removed
}

// notifyRuntime tells thread-index SSE subscribers that some runtime state
// changed (bind, unbind, turnActive flip). Nil-safe: a server wired without
// the hub simply has no subscribers to tell.
func (s *server) notifyRuntime() {
	if s.deferBootRuntime(false) {
		return
	}
	s.publishRuntime(false)
}

// notifyRuntimeRefresh publishes the latest runtime snapshot and also tells
// browsers to re-read catalog/sidebar structure. Use it for durable metadata
// changes that alter row placement but are not represented by runtimeInfo.
// Every open page reads the whole session list for it, so each call is logged
// with its caller: a burst of reads can then be traced to who asked.
func (s *server) notifyRuntimeRefresh() {
	if pc, _, _, ok := goruntime.Caller(1); ok {
		log.Printf("runtime refresh requested by %s", goruntime.FuncForPC(pc).Name())
	}
	if s.deferBootRuntime(true) {
		return
	}
	s.publishRuntime(true)
}

// Handshakes must not pay for fleet snapshots while the boot queues register.
func (s *server) deferBootRuntime(refresh bool) bool {
	boot := s.boot
	if boot == nil {
		return false
	}
	boot.mu.Lock()
	defer boot.mu.Unlock()
	if !boot.runtimeActive {
		return false
	}
	boot.runtimeDirty = true
	boot.runtimeRefresh = boot.runtimeRefresh || refresh
	return true
}

func (s *server) publishBootRuntime(final bool) {
	boot := s.boot
	boot.mu.Lock()
	dirty, refresh := boot.runtimeDirty, boot.runtimeRefresh
	boot.runtimeDirty, boot.runtimeRefresh = false, false
	boot.mu.Unlock()
	if dirty || final {
		s.publishRuntime(refresh)
	}
	if final {
		// Notices arriving during the final snapshot must survive the handoff
		// to immediate publishing, without holding handshakes behind its build.
		boot.mu.Lock()
		dirty, refresh = boot.runtimeDirty, boot.runtimeRefresh
		boot.runtimeActive, boot.runtimeDirty, boot.runtimeRefresh = false, false, false
		boot.mu.Unlock()
		if dirty {
			s.publishRuntime(refresh)
		}
	}
}

func (s *server) publishRuntime(refresh bool) {
	s.publishRuntimeDelta(refresh, false)
}

// publishRuntimeDelta takes one snapshot and hands it to the hub under one
// lock. Two publishers could otherwise capture old then new state and notify
// new then old; since a delta names only changed threads, no later unrelated
// change would correct the inversion.
func (s *server) publishRuntimeDelta(refresh, sidebarCurrent bool) {
	if s.runtimeEvents == nil {
		return
	}
	s.runtimePublishMu.Lock()
	defer s.runtimePublishMu.Unlock()
	delta := s.runtimeDeltaSnapshot()
	// A star update has already applied its own change to the sidebar; a
	// rebuild the snapshot needs for its own reasons still happens.
	delta.sidebarCurrent = sidebarCurrent && !delta.rebuild
	delta.Refresh = delta.Refresh || refresh
	delta.rebuild = delta.rebuild || refresh
	s.runtimeEvents.notify(delta)
}

// rememberAttachment persists a durable attachment; nil-safe so a provider
// handshake can call it unconditionally when the store is unavailable. Callers
// supply the record's identity and display fields; the attach time is stamped
// here so every provider orders detached entries newest-first identically.
func (s *server) rememberAttachment(rec attachmentRecord) {
	if s.attachments == nil {
		return
	}
	rec.AttachedAt = time.Now().Format(time.RFC3339)
	s.attachments.remember(rec)
}

// forgetAttachment drops a durable attachment record; nil-safe like
// rememberAttachment. Handed to sessions so a `--resume` successor can retire
// the record it supersedes (claude.go) without reaching for the store itself.
func (s *server) forgetAttachment(provider, nativeThreadID string) {
	if s.attachments == nil {
		return
	}
	s.attachments.forget(provider, nativeThreadID)
}

// setAttachmentTurnAnchor persists (or clears) the live-turn anchor on an
// existing attachment record; nil-safe like the rest. Handed to sessions so
// the claude transport can keep the record current without reaching for the
// store — it is the one piece of live state that has to outlive the process.
func (s *server) setAttachmentTurnAnchor(provider, nativeThreadID string, startedAtMs, anchorSeq int64) {
	if s.attachments == nil {
		return
	}
	s.attachments.setTurnAnchor(provider, nativeThreadID, startedAtMs, anchorSeq)
}

// setAttachmentPendingName persists a pre-init rename onto an existing
// provisional record; nil-safe like the rest (handleSessionName).
func (s *server) setAttachmentPendingName(provider, nativeThreadID, name string) {
	if s.attachments == nil {
		return
	}
	s.attachments.setPendingName(provider, nativeThreadID, name)
}

// setAttachmentShimSession ties an existing attachment record to the shim
// child now hosting its thread; nil-safe like the rest. Handed to sessions for
// the `--resume` respawn window (claude.go, openClaudeSession).
func (s *server) setAttachmentShimSession(provider, nativeThreadID, shimSessionID string) {
	if s.attachments == nil {
		return
	}
	s.attachments.setShimSession(provider, nativeThreadID, shimSessionID)
}

// attachmentFor looks up a durable attachment; nil store (feature off) simply
// reports none, which callers must treat as "no evidence", never as an error.
func (s *server) attachmentFor(provider, nativeThreadID string) (attachmentRecord, bool) {
	if s.attachments == nil {
		return attachmentRecord{}, false
	}
	return s.attachments.get(provider, nativeThreadID)
}

type codexTurn struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	StartedAt *int64 `json:"startedAt"`
}

// codexTurnInProgressStatus is the TurnStatus value that means the turn is
// still running (enum: completed | interrupted | failed | inProgress).
const codexTurnInProgressStatus = "inProgress"

// msPerSecond converts codex's unix-seconds timestamps at the ingest boundary.
const msPerSecond = 1000

// residualMsThreshold is the D1 residual defense: seconds-since-epoch is ~1.8e9
// while milliseconds-since-epoch is ~1.8e12, so anything above 1e10 is already
// milliseconds and must not be multiplied again. It has never fired against the
// current schema — it exists because the version that stops honoring the schema
// is the one nobody re-probes.
const residualMsThreshold = int64(1e10)

func (t codexTurn) startedAtMs() int64 {
	if t.StartedAt == nil || *t.StartedAt <= 0 {
		return 0
	}
	if *t.StartedAt > residualMsThreshold {
		return *t.StartedAt
	}
	return *t.StartedAt * msPerSecond
}

// codexTurnFromParams pulls the Turn out of a turn/started or turn/completed
// notification (TurnStartedNotification: {threadId, turn:{id,…}}).
func codexTurnFromParams(params json.RawMessage) codexTurn {
	var p struct {
		Turn codexTurn `json:"turn"`
	}
	if json.Unmarshal(params, &p) != nil {
		return codexTurn{}
	}
	return p.Turn
}

// runtimeHub fans the compact runtime delta out to every open thread-index SSE
// subscriber. Like catalogHub, each subscriber holds a single-slot buffer, so
// a slow subscriber can never block session-land.
//
// Each delta takes the next seq of this process's generation, and the hub
// keeps the last runtimeReplayLimit of them. A browser that read the session
// list at a position subscribes from it and receives what it missed as one
// merged delta, so a page load reads the list once instead of again when its
// stream opens, and a short drop needs no read at all.
type runtimeHub struct {
	mu         sync.Mutex
	subs       map[chan runtimeDelta]struct{}
	generation string
	seq        uint64
	recent     []runtimeDelta
}

const runtimeReplayLimit = 64

// runtimePosition names a point in the runtime stream: the session list read
// reports it, and the browser's stream connect echoes it back.
type runtimePosition struct {
	Generation string `json:"generation"`
	Seq        uint64 `json:"seq"`
}

func newRuntimeHub() *runtimeHub {
	return &runtimeHub{
		subs:       make(map[chan runtimeDelta]struct{}),
		generation: strconv.FormatInt(time.Now().UnixNano(), 36),
	}
}

func (h *runtimeHub) position() runtimePosition {
	h.mu.Lock()
	defer h.mu.Unlock()
	return runtimePosition{Generation: h.generation, Seq: h.seq}
}

func (h *runtimeHub) subscribe() chan runtimeDelta {
	ch := make(chan runtimeDelta, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// subscribeFrom subscribes a browser that last saw from. What it missed waits
// in the channel as one delta; a position this hub cannot replay (another
// process's, or older than the deltas kept) gets a refresh, which reads the
// list. A current position gets nothing.
func (h *runtimeHub) subscribeFrom(from runtimePosition) chan runtimeDelta {
	ch := make(chan runtimeDelta, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[ch] = struct{}{}
	missed := h.seq - from.Seq
	switch {
	case from.Generation != h.generation || from.Seq > h.seq || missed > uint64(len(h.recent)):
		ch <- runtimeDelta{Rows: []runtimeDeltaRow{}, Refresh: true, Seq: h.seq}
	case missed > 0:
		replay := h.recent[uint64(len(h.recent))-missed:]
		merged := replay[0]
		for _, delta := range replay[1:] {
			merged = mergeRuntimeDeltas(merged, delta)
		}
		ch <- merged.bounded()
	}
	return ch
}

func (h *runtimeHub) unsubscribe(ch chan runtimeDelta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

func (h *runtimeHub) notify(delta ...runtimeDelta) {
	next := runtimeDelta{}
	if len(delta) > 0 {
		next = delta[0]
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	next.Seq = h.seq
	h.recent = append(h.recent, next)
	if len(h.recent) > runtimeReplayLimit {
		h.recent = h.recent[1:]
	}
	for ch := range h.subs {
		// Merge a pending delta into this one rather than drop it: a thread the
		// pending delta names and this one does not keeps its pending overlay,
		// and a thread both name takes the later one.
		merged := next
		select {
		case pending := <-ch:
			merged = mergeRuntimeDeltas(pending, next)
		default:
		}
		select {
		case ch <- merged.bounded():
		default:
		}
	}
}

// mergeSessionChanges carries both deltas' session and detached changes,
// later over earlier: an entry that left and came back is present, and one
// that came and left is gone.
func mergeSessionChanges(merged *runtimeDelta, earlier, later runtimeDelta) {
	rows := map[string]listedSession{}
	removed := map[string]bool{}
	for _, delta := range []runtimeDelta{earlier, later} {
		for _, id := range delta.RemovedSessions {
			delete(rows, id)
			removed[id] = true
		}
		for _, row := range delta.Sessions {
			rows[row.ID] = row
			delete(removed, row.ID)
		}
	}
	for _, row := range rows {
		merged.Sessions = append(merged.Sessions, row)
	}
	sort.Slice(merged.Sessions, func(i, j int) bool {
		return timestampBefore(merged.Sessions[i].CreatedAt, merged.Sessions[j].CreatedAt)
	})
	for id := range removed {
		merged.RemovedSessions = append(merged.RemovedSessions, id)
	}
	sort.Strings(merged.RemovedSessions)
	detached := map[string]detachedSessionView{}
	removedDetached := map[string]bool{}
	for _, delta := range []runtimeDelta{earlier, later} {
		for _, key := range delta.RemovedDetached {
			delete(detached, key)
			removedDetached[key] = true
		}
		for _, row := range delta.DetachedRows {
			key := row.Provider + ":" + row.NativeThreadID
			detached[key] = row
			delete(removedDetached, key)
		}
	}
	for _, row := range detached {
		merged.DetachedRows = append(merged.DetachedRows, row)
	}
	sort.Slice(merged.DetachedRows, func(i, j int) bool {
		return merged.DetachedRows[i].Provider+":"+merged.DetachedRows[i].NativeThreadID < merged.DetachedRows[j].Provider+":"+merged.DetachedRows[j].NativeThreadID
	})
	for key := range removedDetached {
		merged.RemovedDetached = append(merged.RemovedDetached, key)
	}
	sort.Strings(merged.RemovedDetached)
}

// mergeRuntimeDeltas is the delta a subscriber reads in place of two: later's
// rows over earlier's by thread, in the sorted order the snapshot uses.
func mergeRuntimeDeltas(earlier, later runtimeDelta) runtimeDelta {
	byKey := make(map[attachmentKey]runtimeDeltaRow, len(earlier.Rows)+len(later.Rows))
	for _, row := range earlier.Rows {
		byKey[attachmentKey{Provider: row.Provider, NativeThreadID: row.NativeID}] = row
	}
	for _, row := range later.Rows {
		byKey[attachmentKey{Provider: row.Provider, NativeThreadID: row.NativeID}] = row
	}
	// A rebuild still needed keeps that need through the merge; a later star
	// update that applied cleanly cannot mask it.
	needsRebuild := (earlier.rebuild || earlier.Refresh) && !earlier.sidebarCurrent || (later.rebuild || later.Refresh) && !later.sidebarCurrent
	merged := runtimeDelta{
		Refresh: earlier.Refresh || later.Refresh,
		rebuild: earlier.rebuild || later.rebuild, Seq: max(earlier.Seq, later.Seq),
	}
	merged.sidebarCurrent = (merged.rebuild || merged.Refresh) && !needsRebuild
	mergeSessionChanges(&merged, earlier, later)
	if earlier.Rows != nil || later.Rows != nil {
		merged.Rows = make([]runtimeDeltaRow, 0, len(byKey))
		for _, row := range byKey {
			merged.Rows = append(merged.Rows, row)
		}
		sort.Slice(merged.Rows, func(i, j int) bool {
			if merged.Rows[i].Provider != merged.Rows[j].Provider {
				return merged.Rows[i].Provider < merged.Rows[j].Provider
			}
			return merged.Rows[i].NativeID < merged.Rows[j].NativeID
		})
	}
	return merged
}

package app

import (
	"encoding/json"
	"log"
	"maps"
	"reflect"
	goruntime "runtime"
	"sort"
	"time"

	sessionruntime "github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/sidebar"
)

// runtimeSource is the narrow seam between the catalog API and session-land:
// the server implements it over its sessions map, and a nil source (tests,
// catalog-only wiring) simply leaves the overlay absent.
type runtimeSource interface {
	RuntimeFor(provider, nativeThreadID string) (sidebar.RuntimeInfo, bool)
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
func (s *server) RuntimeFor(provider, nativeThreadID string) (sidebar.RuntimeInfo, bool) {
	if sess := s.SessionForThread(provider, nativeThreadID); sess != nil {
		return sessionRuntimeInfo(sess.Summary()), true
	}
	if rec, remembered := s.attachmentFor(provider, nativeThreadID); remembered {
		return s.detachedRuntimeInfos([]attachmentRecord{rec})[rec.Key()], true
	}
	return sidebar.RuntimeInfo{}, false
}

// detachedRuntimeInfos projects detached threads' overlay rows. Presentation
// comes from the canonical hosted record when one exists — the mode is mutable
// now (setPresentation), and the attachment JSON copy may be stale between the
// flip and the record's next transport rewrite. The attachment value remains
// only as the fallback for legacy records that predate hosted identity. The
// modes are read in one batch so snapshots do not read once per thread behind
// each registration's write.
func (s *server) detachedRuntimeInfos(records []attachmentRecord) map[attachmentKey]sidebar.RuntimeInfo {
	ids := make([]string, 0, len(records))
	for _, rec := range records {
		ids = append(ids, rec.HostedThreadID)
	}
	modes := s.agentExecutions.PresentationModes(ids)
	infos := make(map[attachmentKey]sidebar.RuntimeInfo, len(records))
	for _, rec := range records {
		mode := rec.PresentationMode
		if canonical, ok := modes[rec.HostedThreadID]; ok {
			mode = canonical
		}
		info := sidebar.RuntimeInfo{Detached: true, PresentationMode: mode}
		info.Status = s.bootAttachments.State(rec.Key())
		infos[rec.Key()] = info
	}
	return infos
}

// sessionRuntimeInfo projects one live session's summary into the attached
// overlay shape. Shared by RuntimeFor and the catalog's synthetic memory rows
// so the two paths cannot drift about what "attached" carries — the memory-row
// bug class the unified catalog already learned once.
func sessionRuntimeInfo(sum sessionruntime.Summary) sidebar.RuntimeInfo {
	return sidebar.RuntimeInfo{
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

// runtimeOverlays is the runtime overlay of every thread Nano holds: the live
// sessions in summaries (the newest wins a shared native id), then the
// detached attachments. The runtime delta and the materialized sidebar both
// read it, so a sidebar rebuild agrees with the last delta about every row.
// When they disagreed on one field, each rebuild republished every row.
func (s *server) runtimeOverlays(summaries []sessionruntime.Summary) map[attachmentKey]sidebar.RuntimeInfo {
	overlays := make(map[attachmentKey]sidebar.RuntimeInfo)
	live := make(map[attachmentKey]bool)
	for _, sum := range summaries {
		if sum.ThreadID == "" {
			continue
		}
		key := attachmentKey{Provider: sum.Provider, NativeThreadID: sum.ThreadID}
		live[key] = true
		info := sessionRuntimeInfo(sum)
		if sum.HostedThreadID != "" {
			info.MetadataRevision = s.controlMetadata.LatestRevision(sum.HostedThreadID)
		}
		overlays[key] = info
	}
	if s.attachments != nil {
		for key, info := range s.detachedRuntimeInfos(s.attachments.DetachedExcept(live)) {
			overlays[key] = info
		}
	}
	return overlays
}

func (s *server) runtimeDeltaSnapshot() sidebar.RuntimeDelta {
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
	for _, sess := range s.SnapshotSessions() {
		if _, live := sessions[sess.ID()]; !live {
			sessions[sess.ID()] = sessionruntime.StatusExited
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

	rows := make([]sidebar.RuntimeRow, 0)
	for key, runtime := range current {
		if before, ok := previous[key]; ok && reflect.DeepEqual(before, runtime) {
			continue
		}
		runtimeCopy := runtime
		rows = append(rows, sidebar.RuntimeRow{
			Provider: key.Provider,
			NativeID: key.NativeThreadID,
			Runtime:  &runtimeCopy,
		})
	}
	for key := range previous {
		if _, ok := current[key]; !ok {
			rows = append(rows, sidebar.RuntimeRow{Provider: key.Provider, NativeID: key.NativeThreadID})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Provider != rows[j].Provider {
			return rows[i].Provider < rows[j].Provider
		}
		return rows[i].NativeID < rows[j].NativeID
	})
	delta := sidebar.RuntimeDelta{Rows: rows, Rebuild: refresh}
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

// attachSessionChanges fills a delta's session fields from the change between
// two snapshots of the live session set and
// the detached set since the last change.
func (s *server) attachSessionChanges(delta *sidebar.RuntimeDelta, before, after map[string]string, summaries []sessionruntime.Summary) {
	var changed []*sessionruntime.Session
	for _, sess := range s.SnapshotSessions() {
		if state, ok := after[sess.ID()]; ok {
			if previous, ok := before[sess.ID()]; !ok || previous != state {
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
	if len(changed)+len(delta.RemovedSessions)+len(changedDetached)+len(removedDetached) > sidebar.RuntimeChangeLimit {
		// Too large to send as rows: the same empty refresh marker bounded
		// sends, without building rows that would be dropped.
		*delta = sidebar.RuntimeDelta{Rows: []sidebar.RuntimeRow{}, Refresh: true, Rebuild: true}
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
func liveKeys(summaries []sessionruntime.Summary) map[attachmentKey]bool {
	live := make(map[attachmentKey]bool, len(summaries))
	for _, sum := range summaries {
		if sum.ThreadID != "" && sum.Status != sessionruntime.StatusExited {
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
		records = s.attachments.DetachedExcept(live)
	}
	current := make(map[attachmentKey]string, len(records))
	for _, record := range records {
		state, _ := json.Marshal(record)
		current[record.Key()] = string(state) + "|" + s.bootAttachments.State(record.Key())
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
// changes that alter row placement but are not represented by sidebar.RuntimeInfo.
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
	delta.SidebarCurrent = sidebarCurrent && !delta.Rebuild
	delta.Refresh = delta.Refresh || refresh
	delta.Rebuild = delta.Rebuild || refresh
	s.runtimeEvents.Notify(delta)
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
	s.attachments.Remember(rec)
}

// forgetAttachment drops a durable attachment record; nil-safe like
// rememberAttachment. Handed to sessions so a `--resume` successor can retire
// the record it supersedes (claude.go) without reaching for the store itself.
func (s *server) forgetAttachment(provider, nativeThreadID string) {
	if s.attachments == nil {
		return
	}
	s.attachments.Forget(provider, nativeThreadID)
}

// setAttachmentTurnAnchor persists (or clears) the live-turn anchor on an
// existing attachment record; nil-safe like the rest. Handed to sessions so
// the claude transport can keep the record current without reaching for the
// store — it is the one piece of live state that has to outlive the process.
func (s *server) setAttachmentTurnAnchor(provider, nativeThreadID string, startedAtMs, anchorSeq int64) {
	if s.attachments == nil {
		return
	}
	s.attachments.SetTurnAnchor(provider, nativeThreadID, startedAtMs, anchorSeq)
}

// setAttachmentPendingName persists a pre-init rename onto an existing
// provisional record; nil-safe like the rest (handleSessionName).
func (s *server) setAttachmentPendingName(provider, nativeThreadID, name string) {
	if s.attachments == nil {
		return
	}
	s.attachments.SetPendingName(provider, nativeThreadID, name)
}

// setAttachmentShimSession ties an existing attachment record to the shim
// child now hosting its thread; nil-safe like the rest. Handed to sessions for
// the `--resume` respawn window (claude.go, openClaudeSession).
func (s *server) setAttachmentShimSession(provider, nativeThreadID, shimSessionID string) {
	if s.attachments == nil {
		return
	}
	s.attachments.SetShimSession(provider, nativeThreadID, shimSessionID)
}

// attachmentFor looks up a durable attachment; nil store (feature off) simply
// reports none, which callers must treat as "no evidence", never as an error.
func (s *server) attachmentFor(provider, nativeThreadID string) (attachmentRecord, bool) {
	if s.attachments == nil {
		return attachmentRecord{}, false
	}
	return s.attachments.Get(provider, nativeThreadID)
}

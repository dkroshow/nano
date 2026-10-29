package sidebar

import (
	"encoding/json"
	"maps"
	"reflect"
	"sort"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	sessionruntime "github.com/dkroshow/nano/internal/sessions"
)

// RuntimeFor reports the runtime overlay for (provider, nativeThreadID) — the
// catalog-facing half of sessionForThread, extended with the remembered feed:
// a live session makes the thread attached, otherwise a surviving attachment
// record makes it detached.
//
// An exited session never reaches this scan (sessionForThread skips corpses):
// its child is dead, and its attachment record (if any) still stands, so the
// thread reads detached — exactly the post-crash reality the sidebar must
// show, with re-attach as the offered action.
func (s *Controller) runtimeFor(provider, nativeThreadID string) (RuntimeInfo, bool) {
	if s.registry != nil {
		if sess := s.registry.SessionForThread(provider, nativeThreadID); sess != nil {
			return sessionRuntimeInfo(sess.Summary()), true
		}
	}
	if s.attachments != nil {
		if rec, remembered := s.attachments.Get(provider, nativeThreadID); remembered {
			return s.detachedRuntimeInfos([]hostedstore.Attachment{rec})[rec.Key()], true
		}
	}
	return RuntimeInfo{}, false
}

// detachedRuntimeInfos projects detached threads' overlay rows. Presentation
// comes from the canonical hosted record when one exists — the mode is mutable
// now (setPresentation), and the attachment JSON copy may be stale between the
// flip and the record's next transport rewrite. The attachment value remains
// only as the fallback for legacy records that predate hosted identity. The
// modes are read in one batch so snapshots do not read once per thread behind
// each registration's write.
func (s *Controller) detachedRuntimeInfos(records []hostedstore.Attachment) map[hostedstore.AttachmentKey]RuntimeInfo {
	ids := make([]string, 0, len(records))
	for _, rec := range records {
		ids = append(ids, rec.HostedThreadID)
	}
	modes := s.agentExecutions.PresentationModes(ids)
	infos := make(map[hostedstore.AttachmentKey]RuntimeInfo, len(records))
	for _, rec := range records {
		mode := rec.PresentationMode
		if canonical, ok := modes[rec.HostedThreadID]; ok {
			mode = canonical
		}
		info := RuntimeInfo{Detached: true, PresentationMode: mode}
		info.Status = s.boot.State(rec.Key())
		infos[rec.Key()] = info
	}
	return infos
}

// sessionRuntimeInfo projects one live session's summary into the attached
// overlay shape. Shared by RuntimeFor and the catalog's synthetic memory rows
// so the two paths cannot drift about what "attached" carries — the memory-row
// bug class the unified catalog already learned once.
func sessionRuntimeInfo(sum sessionruntime.Summary) RuntimeInfo {
	return RuntimeInfo{
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
func (s *Controller) runtimeOverlays(summaries []sessionruntime.Summary) map[hostedstore.AttachmentKey]RuntimeInfo {
	overlays := make(map[hostedstore.AttachmentKey]RuntimeInfo)
	live := make(map[hostedstore.AttachmentKey]bool)
	for _, sum := range summaries {
		if sum.ThreadID == "" {
			continue
		}
		key := hostedstore.AttachmentKey{Provider: sum.Provider, NativeThreadID: sum.ThreadID}
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

func (s *Controller) runtimeDeltaSnapshot() RuntimeDelta {
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
	for _, sess := range s.registry.SnapshotSessions() {
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

	rows := make([]RuntimeRow, 0)
	for key, runtime := range current {
		if before, ok := previous[key]; ok && reflect.DeepEqual(before, runtime) {
			continue
		}
		runtimeCopy := runtime
		rows = append(rows, RuntimeRow{
			Provider: key.Provider,
			NativeID: key.NativeThreadID,
			Runtime:  &runtimeCopy,
		})
	}
	for key := range previous {
		if _, ok := current[key]; !ok {
			rows = append(rows, RuntimeRow{Provider: key.Provider, NativeID: key.NativeThreadID})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Provider != rows[j].Provider {
			return rows[i].Provider < rows[j].Provider
		}
		return rows[i].NativeID < rows[j].NativeID
	})
	delta := RuntimeDelta{Rows: rows, Rebuild: refresh}
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
func (s *Controller) attachSessionChanges(delta *RuntimeDelta, before, after map[string]string, summaries []sessionruntime.Summary) {
	var changed []*sessionruntime.Session
	for _, sess := range s.registry.SnapshotSessions() {
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
	if len(changed)+len(delta.RemovedSessions)+len(changedDetached)+len(removedDetached) > RuntimeChangeLimit {
		// Too large to send as rows: the same empty refresh Marker bounded
		// sends, without building rows that would be dropped.
		*delta = RuntimeDelta{Rows: []RuntimeRow{}, Refresh: true, Rebuild: true}
		return
	}
	if len(changed) > 0 {
		delta.Sessions = s.ListedSessions(changed)
	}
	if len(changedDetached) > 0 {
		delta.DetachedRows = s.detachedViewsOf(changedDetached)
	}
	delta.RemovedDetached = removedDetached
}

// liveKeys names the threads of live sessions, which no detached row repeats.
func liveKeys(summaries []sessionruntime.Summary) map[hostedstore.AttachmentKey]bool {
	live := make(map[hostedstore.AttachmentKey]bool, len(summaries))
	for _, sum := range summaries {
		if sum.ThreadID != "" && sum.Status != sessionruntime.StatusExited {
			live[hostedstore.AttachmentKey{Provider: sum.Provider, NativeThreadID: sum.ThreadID}] = true
		}
	}
	return live
}

// detachedChanges returns the detached records that joined or changed since
// the last call and the wire keys of those that left, and remembers the
// current set. The first call reports every record.
func (s *Controller) detachedChanges(live map[hostedstore.AttachmentKey]bool) ([]hostedstore.Attachment, []string) {
	var records []hostedstore.Attachment
	if s.attachments != nil {
		records = s.attachments.DetachedExcept(live)
	}
	current := make(map[hostedstore.AttachmentKey]string, len(records))
	for _, record := range records {
		state, _ := json.Marshal(record)
		current[record.Key()] = string(state) + "|" + s.boot.State(record.Key())
	}
	s.runtimeSnapshotMu.Lock()
	previous := s.runtimeDetached
	s.runtimeDetached = current
	s.runtimeSnapshotMu.Unlock()
	var changed []hostedstore.Attachment
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

// PublishRuntimeDelta takes one snapshot and hands it to the hub under one
// lock. Two publishers could otherwise capture old then new state and notify
// new then old; since a delta names only changed threads, no later unrelated
// change would correct the inversion.
// PublishRuntimeDelta serializes a runtime snapshot and publishes its change.
func (s *Controller) PublishRuntimeDelta(refresh, sidebarCurrent bool) {
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

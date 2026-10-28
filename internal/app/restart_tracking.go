// restart_tracking.go — one content-free record per Nano process start: when
// HTTP opened and, for each stored thread the boot re-attaches, when each
// reconnect step happened. It exists so restart speed is measured rather than
// guessed (docs/restart-tracking.md). Ids, fixed codes and millisecond
// offsets only: no prompt text, titles, paths or error text.
package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/observability"
)

const (
	restartsFileName = "restarts.jsonl"
	// A boot that has not settled every thread by then is written as it
	// stands, with the rest marked not_live_at_cap.
	restartRecordCap = 15 * time.Minute
	// A live thread's attached mark may still be on its way: a Claude respawn
	// goes live before its resume returns. The record waits at most this long
	// after the thread went live.
	restartSettleGrace = 30 * time.Second
	// Browsers ask for the resting threads they show right after a restart.
	// With resting threads, the record waits until no attach has settled for
	// this long, so those attaches count.
	restartRestQuiet = time.Minute
	// Restart lines carry every thread (~100 KB for 400 threads).
	restartsReadTail     = 4 << 20
	restartsDefaultLimit = 5
	restartsMaxLimit     = 50
)

// processStarted is the earliest moment this process can name; restart
// offsets count from it.
var processStarted = time.Now()

type restartStep = observability.RestartStep

const (
	restartStatePending = "pending"
	restartStateLive    = "live"
	restartStateFailed  = "failed"
	// resting: left by the boot loop until something needs it; settled for
	// the record unless it is attached before the record is written.
	restartStateResting = "resting"
)

const (
	restartPicked     = observability.RestartPicked
	restartLock       = observability.RestartLock
	restartRegistered = observability.RestartRegistered
	restartAttached   = observability.RestartAttached
	restartLive       = observability.RestartLive
)

type restartRecord struct {
	V                int                               `json:"v"`
	RestartID        string                            `json:"restart_id"`
	Stamp            string                            `json:"stamp"`
	StartedAt        time.Time                         `json:"started_at"`
	HTTPOpenMs       *int64                            `json:"http_open_ms"`
	BootLoopMs       *int64                            `json:"boot_loop_ms"`
	VisibleLiveMs    *int64                            `json:"visible_live_ms"`
	VisibleSettledMs *int64                            `json:"visible_settled_ms"`
	AllLiveMs        *int64                            `json:"all_live_ms"`
	AllSettledMs     *int64                            `json:"all_settled_ms"`
	Complete         bool                              `json:"complete"`
	Interrupted      bool                              `json:"interrupted,omitempty"`
	ThreadsTotal     int                               `json:"threads_total"`
	Live             int                               `json:"live"`
	Failed           int                               `json:"failed"`
	Pending          int                               `json:"pending"`
	Resting          int                               `json:"resting"`
	Visible          int                               `json:"visible"`
	ByProvider       map[string]restartProviderSummary `json:"by_provider"`
	Threads          []restartThreadRecord             `json:"threads,omitempty"`
}

type restartProviderSummary struct {
	Threads int                    `json:"threads"`
	Live    int                    `json:"live"`
	Failed  int                    `json:"failed"`
	Pending int                    `json:"pending"`
	Resting int                    `json:"resting"`
	Ms      map[string]restartStat `json:"ms"`
}

type restartStat struct {
	N   int   `json:"n"`
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	Max int64 `json:"max"`
}

type restartThreadRecord struct {
	Provider       string `json:"provider"`
	NativeThreadID string `json:"native_thread_id"`
	HostedThreadID string `json:"hosted_thread_id,omitempty"`
	Visible        bool   `json:"visible"`
	Tier           string `json:"tier"`
	Order          int    `json:"order"`
	// State is pending, resting, live or failed: the last of live and failed
	// wins.
	State        string `json:"state"`
	PickedMs     *int64 `json:"picked_ms"`
	LockMs       *int64 `json:"lock_ms"`
	RegisteredMs *int64 `json:"registered_ms"`
	AttachedMs   *int64 `json:"attached_ms"`
	LiveMs       *int64 `json:"live_ms"`
	FailedMs     *int64 `json:"failed_ms"`
	Reason       string `json:"reason,omitempty"`
}

// restartTracker holds this process's record until it is written.
type restartTracker struct {
	path    string
	stamp   string
	bootID  string
	started time.Time

	mu          sync.Mutex
	httpOpen    *int64
	bootLoop    *int64
	threads     []*restartThread
	byKey       map[attachmentKey]*restartThread
	wake        *time.Timer // the next time the record may be written
	lastMark    time.Time   // when a mark last changed the record
	written     bool        // final: no later mark changes the record
	interrupted bool
	saved       bool // the line is in the file
}

// restartThread is one stored thread's entry, changed only under the
// tracker's lock. Each timestamp is set at most once; state follows the last
// of live and failed.
type restartThread struct {
	tracker *restartTracker
	restartThreadRecord
}

func newRestartTracker(path, stamp, bootID string, started time.Time) *restartTracker {
	t := &restartTracker{path: path, stamp: stamp, bootID: bootID, started: started, byKey: map[attachmentKey]*restartThread{}}
	time.AfterFunc(time.Until(started.Add(restartRecordCap)), func() { t.writeUnsettled("not_live_at_cap", false) })
	return t
}

func (t *restartTracker) sinceStart() *int64 {
	ms := time.Since(t.started).Milliseconds()
	return &ms
}

func (t *restartTracker) markHTTPOpen() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.httpOpen == nil {
		t.httpOpen = t.sinceStart()
	}
}

// addThreads registers the boot's stored threads, before HTTP serves, so a
// request that attaches one early is tracked too.
func (t *restartTracker) addThreads(records []attachmentRecord) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, rec := range records {
		if t.byKey[rec.key()] != nil {
			continue
		}
		th := &restartThread{tracker: t, restartThreadRecord: restartThreadRecord{
			Provider: rec.Provider, NativeThreadID: rec.NativeThreadID, HostedThreadID: rec.HostedThreadID,
			Tier: bootTierNames[bootTierUnseen], Order: i, State: restartStatePending,
		}}
		t.threads = append(t.threads, th)
		t.byKey[rec.key()] = th
	}
}

// rank fixes each thread's sidebar place and the order the boot loop takes,
// before the loop starts, so requests that jump the queue do not change them.
func (t *restartTracker) rank(ranks map[attachmentKey]bootRank, ordered []attachmentRecord) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, rec := range ordered {
		if th := t.byKey[rec.key()]; th != nil {
			r, known := ranks[rec.key()]
			if !known {
				r.tier = bootTierUnseen
			}
			th.Tier, th.Visible, th.Order = bootTierNames[r.tier], r.visible, i
		}
	}
}

// rest marks threads the boot loop leaves resting.
func (t *restartTracker) rest(records []attachmentRecord) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, rec := range records {
		if th := t.byKey[rec.key()]; th != nil && th.State == restartStatePending && th.PickedMs == nil {
			th.State = restartStateResting // a thread a request already picked stays pending
		}
	}
}

func (t *restartTracker) thread(key attachmentKey) *restartThread {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byKey[key]
}

func (t *restartTracker) markBootLoopDone() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.bootLoop == nil {
		t.bootLoop = t.sinceStart()
	}
	t.markedLocked()
}

// hiddenAtBoot reports a boot thread the sidebar did not show at first sight.
// Nil-safe: a session outside a boot is never hidden.
func (th *restartThread) hiddenAtBoot() bool {
	if th == nil {
		return false
	}
	th.tracker.mu.Lock()
	defer th.tracker.mu.Unlock()
	return !th.Visible
}

// at records when this thread reached step. Nil-safe: sessions outside a
// boot have no entry.
func (th *restartThread) at(step restartStep) {
	if th == nil {
		return
	}
	t := th.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	field := map[restartStep]**int64{
		restartPicked: &th.PickedMs, restartLock: &th.LockMs, restartRegistered: &th.RegisteredMs,
		restartAttached: &th.AttachedMs, restartLive: &th.LiveMs,
	}[step]
	if t.written {
		return
	}
	switch {
	case step == restartLive:
		th.State = restartStateLive
	case step == restartPicked && th.State == restartStateResting:
		th.State = restartStatePending // attaching on demand: settled only once live or failed
	}
	if *field == nil {
		*field = t.sinceStart()
	}
	t.markedLocked()
}

// fail records a failure; failed_ms and reason keep the first one. A thread
// that was live and then fails counts as failed, and one that fails and then
// becomes live counts as live.
func (th *restartThread) fail(reason string) {
	if th == nil {
		return
	}
	t := th.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.written {
		return
	}
	th.State = restartStateFailed
	if th.FailedMs == nil {
		th.FailedMs, th.Reason = t.sinceStart(), reason
	}
	t.markedLocked()
}

// writeIfSettledLocked writes the record once the boot loop is done, no
// thread is pending, every live thread has its attached mark or has waited
// restartSettleGrace since it went live, and, while threads rest, no mark has
// changed the record for restartRestQuiet.
// markedLocked notes that a mark just changed the record, then weighs it.
func (t *restartTracker) markedLocked() {
	t.lastMark = time.Now()
	t.writeIfSettledLocked()
}

func (t *restartTracker) writeIfSettledLocked() {
	if t.written || t.bootLoop == nil {
		return
	}
	now, wait := time.Now(), time.Duration(0)
	resting := false
	for _, th := range t.threads {
		switch {
		case th.State == restartStatePending:
			return // the mark that settles it weighs the record again
		case th.State == restartStateResting:
			resting = true
		case th.State == restartStateLive && th.AttachedMs == nil && th.LiveMs != nil:
			due := t.started.Add(time.Duration(*th.LiveMs)*time.Millisecond + restartSettleGrace)
			wait = max(wait, due.Sub(now))
		}
	}
	if resting {
		wait = max(wait, restartRestQuiet-now.Sub(t.lastMark))
	}
	if wait > 0 {
		// One timer for every wait; its callback weighs the record afresh, so
		// a deadline set before a later mark cannot skip that mark.
		if t.wake != nil {
			t.wake.Stop()
		}
		t.wake = time.AfterFunc(wait, func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.writeIfSettledLocked()
		})
		return
	}
	t.writeLocked(false)
}

// writeUnsettled writes the record as it stands: at the cap, or when Nano
// stops before the boot settles (interrupted). Threads still pending carry
// reason.
func (t *restartTracker) writeUnsettled(reason string, interrupted bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.written {
		return
	}
	for _, th := range t.threads {
		if th.State == restartStatePending {
			th.Reason = reason
		}
	}
	t.writeLocked(interrupted)
}

func (t *restartTracker) writeLocked(interrupted bool) {
	t.written, t.interrupted = true, interrupted
	record := t.recordLocked(true)
	line, err := json.Marshal(record)
	if err == nil {
		err = appendLine(t.path, line)
	}
	if err != nil {
		// The API keeps serving it from memory.
		log.Printf("restart record %s: %v", t.path, err)
		return
	}
	t.saved = true
	log.Printf("restart record: http open %s, visible live %s, all live %s, all settled %s (%d threads: %d live, %d failed, %d pending)",
		restartMsText(record.HTTPOpenMs), restartMsText(record.VisibleLiveMs), restartMsText(record.AllLiveMs),
		restartMsText(record.AllSettledMs), record.ThreadsTotal, record.Live, record.Failed, record.Pending)
}

func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return errors.Join(err, f.Close())
}

func restartMsText(ms *int64) string {
	if ms == nil {
		return "unknown"
	}
	return (time.Duration(*ms) * time.Millisecond).String()
}

// restartStages are the per-thread intervals summarized by provider. Each is
// measured between its own two marks.
var restartStages = []struct {
	name     string
	from, to func(*restartThreadRecord) *int64
}{
	{"lock_wait", func(r *restartThreadRecord) *int64 { return r.PickedMs }, func(r *restartThreadRecord) *int64 { return r.LockMs }},
	{"record", func(r *restartThreadRecord) *int64 { return r.LockMs }, func(r *restartThreadRecord) *int64 { return r.RegisteredMs }},
	{"reconnect", func(r *restartThreadRecord) *int64 { return r.RegisteredMs }, func(r *restartThreadRecord) *int64 { return r.AttachedMs }},
	{"registered_to_live", func(r *restartThreadRecord) *int64 { return r.RegisteredMs }, func(r *restartThreadRecord) *int64 { return r.LiveMs }},
	{"to_live", func(*restartThreadRecord) *int64 { return new(int64) }, func(r *restartThreadRecord) *int64 { return r.LiveMs }},
}

// recordLocked summarizes the tracker as it stands. It is complete only once
// written with every thread settled. *_live_ms stays null while any counted
// thread is pending or failed; *_settled_ms is when the last of them became
// live or failed.
func (t *restartTracker) recordLocked(withThreads bool) restartRecord {
	record := restartRecord{
		V: 1, RestartID: t.bootID, Stamp: t.stamp, StartedAt: t.started.UTC(),
		HTTPOpenMs: t.httpOpen, BootLoopMs: t.bootLoop, Interrupted: t.interrupted,
		Complete:     t.written && !t.interrupted && t.bootLoop != nil,
		ThreadsTotal: len(t.threads), ByProvider: map[string]restartProviderSummary{},
	}
	type span struct {
		last            *int64
		failed, pending bool
	}
	var all, visible span
	note := func(s *span, th *restartThread) {
		switch th.State {
		case restartStateLive:
			s.last = laterMs(s.last, th.LiveMs)
		case restartStateFailed:
			s.last, s.failed = laterMs(s.last, th.FailedMs), true
		case restartStateResting:
		default:
			s.pending = true
		}
	}
	durations := map[string]map[string][]int64{}
	for _, th := range t.threads {
		p := record.ByProvider[th.Provider]
		p.Threads++
		switch th.State {
		case restartStateLive:
			record.Live++
			p.Live++
		case restartStateFailed:
			record.Failed++
			p.Failed++
		case restartStateResting:
			record.Resting++
			p.Resting++
		default:
			record.Pending++
			p.Pending++
			record.Complete = false
		}
		note(&all, th)
		if th.Visible {
			record.Visible++
			note(&visible, th)
		}
		record.ByProvider[th.Provider] = p
		if durations[th.Provider] == nil {
			durations[th.Provider] = map[string][]int64{}
		}
		for _, stage := range restartStages {
			if from, to := stage.from(&th.restartThreadRecord), stage.to(&th.restartThreadRecord); from != nil && to != nil && *to >= *from {
				durations[th.Provider][stage.name] = append(durations[th.Provider][stage.name], *to-*from)
			}
		}
		if withThreads {
			record.Threads = append(record.Threads, th.restartThreadRecord)
		}
	}
	for _, s := range []struct {
		span          span
		live, settled **int64
	}{{all, &record.AllLiveMs, &record.AllSettledMs}, {visible, &record.VisibleLiveMs, &record.VisibleSettledMs}} {
		if s.span.pending || s.span.last == nil {
			continue
		}
		*s.settled = s.span.last
		if !s.span.failed {
			*s.live = s.span.last
		}
	}
	for provider, byStage := range durations {
		p := record.ByProvider[provider]
		p.Ms = map[string]restartStat{}
		for name, values := range byStage {
			slices.Sort(values)
			p.Ms[name] = restartStat{
				N: len(values), P50: values[(len(values)-1)/2],
				P95: values[(len(values)-1)*95/100], Max: values[len(values)-1],
			}
		}
		record.ByProvider[provider] = p
	}
	return record
}

func laterMs(current, candidate *int64) *int64 {
	if current == nil || *candidate > *current {
		return candidate
	}
	return current
}

// handleRestarts answers GET /api/v1/restarts: the newest restarts first,
// this process's own while it is still unwritten.
func (s *server) handleRestarts(w http.ResponseWriter, r *http.Request) {
	limit := restartsDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > restartsMaxLimit {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", fmt.Sprintf("limit must be 1-%d", restartsMaxLimit))
			return
		}
		limit = n
	}
	restarts, err := s.restart.recent(limit, r.URL.Query().Get("threads") == "1")
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "restarts_unreadable", err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	httpapi.WriteJSON(w, map[string]any{"restarts": restarts})
}

func (t *restartTracker) recent(limit int, withThreads bool) ([]restartRecord, error) {
	out := []restartRecord{}
	if t == nil {
		return out, nil
	}
	t.mu.Lock()
	if !t.saved {
		out = append(out, t.recordLocked(withThreads))
	}
	t.mu.Unlock()
	written, err := readRestartTail(t.path)
	if err != nil {
		return nil, err
	}
	// Saved between the two reads: the file's copy is the final one.
	if len(out) == 1 && len(written) > 0 && written[len(written)-1].RestartID == out[0].RestartID {
		out = out[:0]
	}
	for i := len(written) - 1; i >= 0 && len(out) < limit; i-- {
		if !withThreads {
			written[i].Threads = nil
		}
		out = append(out, written[i])
	}
	return out, nil
}

func readRestartTail(path string) ([]restartRecord, error) {
	lines, err := tailJSONLines(path, restartsReadTail)
	if err != nil {
		return nil, err
	}
	var records []restartRecord
	for _, line := range lines {
		var record restartRecord
		if json.Unmarshal(line, &record) == nil && record.V == 1 {
			records = append(records, record)
		}
	}
	return records, nil
}

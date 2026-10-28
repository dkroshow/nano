// boot_reattach.go — the eager half of the durability rule. The decision docs
// framed the slice as "persist attachments, re-attach on boot"
// (docs/decisions/supervised-app-server.md, docs/decisions/claude-shim-daemon.md);
// what shipped first was the lazy half (detached rows, one-click re-attach).
// This closes the gap: a nano restart brings back by itself every stored
// thread with work not yet settled, and a browser brings back every thread it
// shows, because the point of the daemons is that a deploy costs the operator
// nothing — not a click per thread. The rest rest: each comes back the moment
// something needs it (a pane or sidebar row shows it, a prompt or request for
// it), so a restart does not spend minutes on hundreds of threads nobody looks
// at.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/sessions"
)

// bootReattach is one boot's set of stored attachments that are not live yet.
// Registration takes minutes for hundreds of threads, so HTTP opens first and
// the set reports each waiting thread as "starting" while the background loop
// registers it in sidebar priority (bootOrder), or as "resting" when the loop
// leaves it for later. A request that needs one waiting thread live registers
// that thread at once through the same code.
type bootReattach struct {
	order    []attachmentRecord
	controls map[string]*sessions.BootControl

	mu       sync.Mutex
	pending  map[attachmentKey]bool
	resting  map[attachmentKey]bool // pending, but not queued by the boot loop
	byHosted map[string]attachmentRecord

	runtimeActive  bool
	runtimeDirty   bool
	runtimeRefresh bool
}

// prepareBootReattach records every stored attachment as starting. Nothing is
// live at boot. It runs once, before HTTP serves.
func (s *server) prepareBootReattach() {
	if s.boot != nil || s.attachments == nil {
		return
	}
	records := s.attachments.DetachedExcept(nil)
	boot := &bootReattach{
		controls: map[string]*sessions.BootControl{},
		pending:  make(map[attachmentKey]bool, len(records)),
		resting:  map[attachmentKey]bool{},
		byHosted: make(map[string]attachmentRecord, len(records)),
	}
	for i := len(records) - 1; i >= 0; i-- { // oldest attach first: bootOrder's order for ties
		rec := records[i]
		boot.order = append(boot.order, rec)
		boot.pending[rec.Key()] = true
		if rec.HostedThreadID != "" {
			boot.byHosted[rec.HostedThreadID] = rec
		}
		if boot.controls[rec.Provider] == nil {
			control := sessions.NewBootControl(sessions.ProviderLifecycleCapabilities(rec.Provider).BootHandshakeLimit)
			boot.controls[rec.Provider] = control
		}
	}
	s.boot = boot
	s.restart.addThreads(boot.order)
}

// state reports a stored thread that still waits for its registration:
// "starting" while the boot loop will register it, "resting" while it waits
// for something to need it, and "" once settled. Nil-safe: without a boot set
// nothing waits.
func (b *bootReattach) state(key attachmentKey) string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stateLocked(key)
}

func (b *bootReattach) stateLocked(key attachmentKey) string {
	switch {
	case !b.pending[key]:
		return ""
	case b.resting[key]:
		return "resting"
	default:
		return "starting"
	}
}

func (b *bootReattach) waiting(key attachmentKey) bool { return b.state(key) != "" }

// waitingHosted returns a waiting thread's record and state by hosted id.
func (b *bootReattach) waitingHosted(hostedThreadID string) (attachmentRecord, string) {
	if b == nil || hostedThreadID == "" {
		return attachmentRecord{}, ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, ok := b.byHosted[hostedThreadID]
	if !ok {
		return attachmentRecord{}, ""
	}
	return rec, b.stateLocked(rec.Key())
}

// rest leaves records for something to need them.
func (b *bootReattach) rest(records []attachmentRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, rec := range records {
		b.resting[rec.Key()] = true
	}
}

// wake ends a thread's rest as its attach begins, so it reports starting.
func (b *bootReattach) wake(rec attachmentRecord) {
	b.mu.Lock()
	delete(b.resting, rec.Key())
	b.mu.Unlock()
}

func (b *bootReattach) settle(rec attachmentRecord) {
	b.mu.Lock()
	delete(b.pending, rec.Key())
	delete(b.resting, rec.Key())
	b.mu.Unlock()
}

type bootReattachOutcome int

const (
	bootReattachSettled bootReattachOutcome = iota // registered or settled by another caller
	bootReattachRegistered
	bootReattachFailed
	bootReattachForgotten
)

// bootRuntimePublishInterval bounds sidebar rebuilds while keeping boot progress visible.
const bootRuntimePublishInterval = 250 * time.Millisecond

// bootHiddenWaitLimit bounds how long hidden threads wait for every
// provider's visible ones to register, so a stuck provider delays the other
// providers' hidden threads by at most this.
const bootHiddenWaitLimit = 30 * time.Second

// reattachOnBoot registers every stored attachment through spawnSession — the
// same path a sidebar re-attach click takes, dual-attachment guards and
// posture restoration included. Codex re-binds to the app-server daemon's
// thread; claude walks the resume ladder (live shim child first). A record
// refused before its shell registers remains detached. Later handshake
// failures stay visible on the shell through the provider-neutral lifecycle.
// Registrations run one at a time per provider, in one queue per provider;
// provider handshakes run behind their shells under per-provider limits.
func (s *server) reattachOnBoot() {
	s.prepareBootReattach()
	boot := s.boot
	if boot == nil {
		return
	}
	boot.mu.Lock()
	boot.runtimeActive = true
	boot.mu.Unlock()
	started := time.Now()
	ranks, ok := s.sidebarBootRanks(boot.order)
	if !ok {
		log.Printf("boot re-attach: sidebar unavailable; attaching in stored order")
	}
	ordered := bootOrder(boot.order, ranks)
	s.restart.rank(ranks, ordered)
	order, resting := bootQueue(ordered, func(rec attachmentRecord) bool {
		return ranks[rec.Key()].visible || s.bootWorkOpen(rec)
	})
	boot.rest(resting)
	s.restart.rest(resting)
	log.Printf("boot re-attach: %d threads queued, %d resting until needed", len(order), len(resting))
	// One queue per provider, each in sidebar priority, so a provider whose
	// daemon hangs holds only its own threads. The restart record's order is
	// the planned priority; within each provider it is also the actual order.
	queues := map[string][]attachmentRecord{}
	for _, rec := range order {
		queues[rec.Provider] = append(queues[rec.Provider], rec)
	}
	// A queue's hidden threads wait until every queue's visible threads are
	// registered: once registered, a hidden thread's replay competes for the
	// CPU and the stores with the registrations of the threads the operator
	// sees first. A stuck provider holds the others' hidden threads only for
	// bootHiddenWaitLimit.
	hidden, release := context.WithTimeout(context.Background(), bootHiddenWaitLimit)
	defer release()
	var visiblePending sync.WaitGroup
	var countsMu sync.Mutex
	counts := map[bootReattachOutcome]int{}
	var queued sync.WaitGroup
	for _, records := range queues {
		queued.Add(1)
		visiblePending.Add(1)
		go func() {
			defer queued.Done()
			waiting := true
			for _, rec := range records {
				if waiting && !ranks[rec.Key()].visible {
					waiting = false
					visiblePending.Done()
					<-hidden.Done()
				}
				outcome := s.reattachStored(rec, false)
				countsMu.Lock()
				counts[outcome]++
				countsMu.Unlock()
			}
			if waiting {
				visiblePending.Done()
			}
		}()
	}
	go func() {
		visiblePending.Wait()
		release()
	}()
	registered := make(chan struct{})
	go func() {
		queued.Wait()
		close(registered)
	}()
	timer := time.NewTimer(bootRuntimePublishInterval)
	defer timer.Stop()
registering:
	for {
		select {
		case <-registered:
			break registering
		case <-timer.C:
			s.publishBootRuntime(false)
			timer.Reset(bootRuntimePublishInterval)
		}
	}
	s.publishBootRuntime(true)
	log.Printf("boot re-attach finished: %d threads in %s (%d registered, %d settled by a request, %d failed, %d no longer stored)",
		len(boot.order), time.Since(started).Round(time.Millisecond), counts[bootReattachRegistered],
		counts[bootReattachSettled], counts[bootReattachFailed], counts[bootReattachForgotten])
	s.restart.markBootLoopDone()
}

const (
	bootTierStarred = iota
	bootTierTopLevel
	bootTierNested
	bootTierUnseen
)

var bootTierNames = [...]string{"starred", "top_level", "nested", "unseen"}

// bootOrder sets restart priority from the sidebar, so the threads near its
// top come back first: starred threads, then top-level rows, then subagents
// nested under them, each visible first and then most recently updated first.
// A thread the sidebar does not know goes last. The sort is stable, so ties
// keep the stored order, and without ranks the stored order stands. Every
// visible thread precedes every hidden one, which reattachOnBoot's hold
// relies on.
func bootOrder(records []attachmentRecord, ranks map[attachmentKey]bootRank) []attachmentRecord {
	rankOf := func(rec attachmentRecord) bootRank {
		if r, ok := ranks[rec.Key()]; ok {
			return r
		}
		return bootRank{tier: bootTierUnseen}
	}
	ordered := append([]attachmentRecord(nil), records...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := rankOf(ordered[i]), rankOf(ordered[j])
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.visible != b.visible {
			return a.visible
		}
		return a.updated.After(b.updated)
	})
	return ordered
}

// bootQueue splits ordered records into the ones the boot loop registers, the
// threads the sidebar shows first and the threads with work not yet settled,
// and the ones left resting. The sidebar estimate brings back what a person
// sees without waiting for a browser; a browser still asks for any resting
// thread it shows that the estimate missed, such as an expanded subagent.
func bootQueue(ordered []attachmentRecord, needed func(attachmentRecord) bool) (queue, resting []attachmentRecord) {
	for _, rec := range ordered {
		if needed(rec) {
			queue = append(queue, rec)
		} else {
			resting = append(resting, rec)
		}
	}
	return queue, resting
}

// bootWorkOpen reports a stored thread with work not yet settled, from each
// record of it: a hosted operation, a prompt in flight (queued, delivering,
// uncertain, or accepted into a turn not known to have ended), or a provider
// turn that started and has no outcome. Such a thread cannot wait for someone
// to look at it. A store it cannot read counts as work, so the thread attaches.
func (s *server) bootWorkOpen(rec attachmentRecord) bool {
	if s.agentExecutions.HasNonTerminalTurns(rec.HostedThreadID) || s.completedTurns.Open(rec.HostedThreadID) {
		return true
	}
	if s.turns == nil {
		return false
	}
	inFlight, err := s.turns.InFlight(context.Background(), rec.HostedThreadID)
	return inFlight || err != nil
}

// sidebarVisibleTopLevel is how many top-level rows count as visible: about
// one screen of the sidebar under its starred rows.
const sidebarVisibleTopLevel = 20

// bootRank is a stored thread's place in the sidebar at boot.
type bootRank struct {
	tier    int
	updated time.Time
	visible bool
}

// sidebarBootRanks reads each stored thread's place in the sidebar's default
// view: starred rows, top-level rows and nested rows, with the catalog's
// updated time. Visible rows are the ones a browser shows first once the
// restart is done: every starred row, and the first sidebarVisibleTopLevel
// top-level rows by recency among the rows the active sidebar will show (rows
// it shows now, plus stored threads, which show once attached). Ordinary
// history rows do not count. A thread the sidebar does not know has no entry
// and ranks as unseen.
func (s *server) sidebarBootRanks(stored []attachmentRecord) (map[attachmentKey]bootRank, bool) {
	nodes, ready := s.sidebar.defaultNodes()
	if !ready {
		return nil, false
	}
	isStored := make(map[attachmentKey]bool, len(stored))
	for _, rec := range stored {
		isStored[rec.Key()] = true
	}
	ranks := make(map[attachmentKey]bootRank, len(nodes))
	var topLevel []attachmentKey
	for _, node := range nodes {
		key := attachmentKey{Provider: node.thread.Provider, NativeThreadID: node.thread.NativeID}
		r := bootRank{tier: bootTierNested}
		if updated := parseCatalogTimePtr(node.thread.UpdatedAt); updated != nil {
			r.updated = *updated
		}
		switch {
		case node.thread.Starred:
			r.tier, r.visible = bootTierStarred, true
		case node.parentID == "":
			r.tier = bootTierTopLevel
			if node.included || isStored[key] {
				topLevel = append(topLevel, key)
			}
		}
		ranks[key] = r
	}
	sort.SliceStable(topLevel, func(i, j int) bool { return ranks[topLevel[i]].updated.After(ranks[topLevel[j]].updated) })
	for _, key := range topLevel[:min(len(topLevel), sidebarVisibleTopLevel)] {
		r := ranks[key]
		r.visible = true
		ranks[key] = r
	}
	return ranks, true
}

// resumeSpawnLock returns provider's resume lock.
func (s *server) resumeSpawnLock(provider string) *sync.Mutex {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return &s.resumeSpawnClaude
	case "codex":
		return &s.resumeSpawnCodex
	case "grok":
		return &s.resumeSpawnGrok
	default:
		return &s.resumeSpawnMu
	}
}

// reattachWaitingHosted registers one waiting thread (starting or resting)
// now, for a request that needs it live. Any other thread is left as it is.
func (s *server) reattachWaitingHosted(hostedThreadID string) {
	if rec, state := s.boot.waitingHosted(hostedThreadID); state != "" {
		s.reattachStored(rec, true)
	}
}

// handleHostedThreadPrioritize registers a thread that still waits after a
// restart now, ahead of the boot queue or out of rest, because the operator
// sees it (an open pane, a sidebar row). A thread not waiting is left as it
// is. The registration runs behind the answer, so a provider stuck in its
// handshake holds no request.
func (s *server) handleHostedThreadPrioritize(w http.ResponseWriter, _ *http.Request, record agentSessionRecord) {
	_, state := s.boot.waitingHosted(record.ID)
	starting := state != ""
	if starting {
		go s.reattachWaitingHosted(record.ID)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]bool{"starting": starting})
}

// reattachStored registers one boot record unless another caller already
// settled it. Two callers may race here; spawnSession's resume guard lets only
// the first register, and the second sees the live session.
func (s *server) reattachStored(rec attachmentRecord, immediateRuntimePublish bool) bootReattachOutcome {
	boot := s.boot
	if !boot.waiting(rec.Key()) {
		return bootReattachSettled
	}
	// Settle only after the attempt, so no view sees the thread as neither
	// starting nor live.
	defer func() {
		boot.settle(rec)
		if immediateRuntimePublish {
			s.publishRuntime(false)
		} else {
			s.notifyRuntime()
		}
	}()
	boot.wake(rec)
	restart := s.restart.thread(rec.Key())
	restart.at(restartPicked)
	if _, stored := s.attachments.Get(rec.Provider, rec.NativeThreadID); !stored {
		restart.fail("no_longer_stored")
		return bootReattachForgotten // detached or closed while it waited
	}
	// Presentation is mutable and control.sqlite is its authority; the
	// attachment copy may be stale from a flip while nano was down or the
	// thread detached. The attachment value covers only legacy records that
	// predate hosted identity.
	presentationMode := rec.PresentationMode
	if rec.HostedThreadID != "" && s.agentExecutions != nil {
		if canonical, ok := s.agentExecutions.PresentationModeFor(rec.HostedThreadID); ok {
			presentationMode = canonical
		}
	}
	sess, err := s.spawnSession(spawnRequest{
		ProviderRequest: sessions.ProviderRequest{
			Provider:            rec.Provider,
			Mode:                "resume",
			ThreadID:            rec.NativeThreadID,
			HostedThreadID:      rec.HostedThreadID,
			CodexZeroTurnResume: sessions.ProviderLifecycleCapabilities(rec.Provider).ResumeWithoutTranscript,
			BootReattach:        boot.controls[rec.Provider],
		},
		InvokerHostedThreadID:   rec.InvokerHostedThreadID,
		InvokerNativeThreadID:   rec.InvokerNativeThreadID,
		InvokerProvider:         rec.InvokerProvider,
		PresentationMode:        presentationMode,
		PendingName:             rec.PendingName,
		immediateRuntimePublish: immediateRuntimePublish,
		restart:                 restart,
		// A thread the sidebar does not show catches up after the visible
		// ones; one the operator opened before the boot loop reached it
		// (immediateRuntimePublish) does not wait.
		catalogBackground: !immediateRuntimePublish && restart.hiddenAtBoot(),
	})
	var attached *sessions.ThreadAttachedError
	switch {
	case errors.As(err, &attached):
		return bootReattachSettled // the winning resume carries the entry
	case errors.Is(err, errHostedControlUnavailable):
		restart.fail("control_unavailable")
		log.Printf("boot re-attach %s/%s failed (left detached): %v", rec.Provider, rec.NativeThreadID, err)
		return bootReattachFailed
	case err != nil:
		restart.fail("spawn_error")
		log.Printf("boot re-attach %s/%s failed (left detached): %v", rec.Provider, rec.NativeThreadID, err)
		return bootReattachFailed
	}
	// Codex turn overrides (service tier included) are re-seeded by the
	// shared resume seam — seedCodexTurnOverrides via startCodexSession —
	// so boot and click-resume converge; no boot-only patching here.
	log.Printf("boot re-attach %s/%s registered session %s", rec.Provider, rec.NativeThreadID, sess.ID())
	return bootReattachRegistered
}

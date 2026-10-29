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
	"github.com/dkroshow/nano/internal/sidebar"
)

// bootReattach holds one boot's registration order, per-provider controls and
// runtime-publication gate. The pending/resting record set belongs to sessions.
type bootReattach struct {
	order    []attachmentRecord
	controls map[string]*sessions.BootControl

	mu sync.Mutex

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
	s.bootAttachments = sessions.NewBootAttachments(records)
	boot := &bootReattach{
		controls: map[string]*sessions.BootControl{},
	}
	for i := len(records) - 1; i >= 0; i-- { // oldest attach first: bootOrder's order for ties
		rec := records[i]
		boot.order = append(boot.order, rec)
		if boot.controls[rec.Provider] == nil {
			control := sessions.NewBootControl(sessions.ProviderLifecycleCapabilities(rec.Provider).BootHandshakeLimit)
			boot.controls[rec.Provider] = control
		}
	}
	s.boot = boot
	s.restart.addThreads(boot.order)
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
	ranks, ok := s.sidebarOwner().BootRanks(boot.order)
	if !ok {
		log.Printf("boot re-attach: sidebar unavailable; attaching in stored order")
	}
	ordered := bootOrder(boot.order, ranks)
	s.restart.rank(ranks, ordered)
	order, resting := bootQueue(ordered, func(rec attachmentRecord) bool {
		return ranks[rec.Key()].Visible || s.bootWorkOpen(rec)
	})
	s.bootAttachments.Rest(resting)
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
				if waiting && !ranks[rec.Key()].Visible {
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

var bootTierNames = [...]string{"starred", "top_level", "nested", "unseen"}

// bootOrder sets restart priority from the sidebar, so the threads near its
// top come back first: starred threads, then top-level rows, then subagents
// nested under them, each visible first and then most recently updated first.
// A thread the sidebar does not know goes last. The sort is stable, so ties
// keep the stored order, and without ranks the stored order stands. Every
// visible thread precedes every hidden one, which reattachOnBoot's hold
// relies on.
func bootOrder(records []attachmentRecord, ranks map[attachmentKey]sidebar.BootRank) []attachmentRecord {
	rankOf := func(rec attachmentRecord) sidebar.BootRank {
		if r, ok := ranks[rec.Key()]; ok {
			return r
		}
		return sidebar.BootRank{Tier: sidebar.BootTierUnseen}
	}
	ordered := append([]attachmentRecord(nil), records...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := rankOf(ordered[i]), rankOf(ordered[j])
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		if a.Visible != b.Visible {
			return a.Visible
		}
		return a.Updated.After(b.Updated)
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
	if rec, state := s.bootAttachments.WaitingHosted(hostedThreadID); state != "" {
		s.reattachStored(rec, true)
	}
}

// handleHostedThreadPrioritize registers a thread that still waits after a
// restart now, ahead of the boot queue or out of rest, because the operator
// sees it (an open pane, a sidebar row). A thread not waiting is left as it
// is. The registration runs behind the answer, so a provider stuck in its
// handshake holds no request.
func (s *server) handleHostedThreadPrioritize(w http.ResponseWriter, _ *http.Request, record agentSessionRecord) {
	_, state := s.bootAttachments.WaitingHosted(record.ID)
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
	if !s.bootAttachments.Waiting(rec.Key()) {
		return bootReattachSettled
	}
	// Settle only after the attempt, so no view sees the thread as neither
	// starting nor live.
	defer func() {
		s.bootAttachments.Settle(rec)
		if immediateRuntimePublish {
			s.publishRuntime(false)
		} else {
			s.notifyRuntime()
		}
	}()
	s.bootAttachments.Wake(rec)
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
			BootReattach:        s.boot.controls[rec.Provider],
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

package sessions

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/diagnostics"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/threadindex"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// A thread's rows come only from the provider's saved file (the catalog).
// The live stream adds the reply being streamed and outbox prompts the file
// does not have yet. One loop per thread orders live text, outbox changes and
// catalog applies, matches saved rows to live text by exact identity, and
// pushes each pane one ordered stream in which a saved row and removal of
// its live text travel in the same message. The browser matches nothing.
// Live text is never stored, merged into rows, or served to agents.

const (
	// liveTextIdleStop is how long a loop with no pane and no activity keeps
	// its state before it stops and forgets it.
	liveTextIdleStop = 60 * time.Second
	// liveTextPaneBacklog closes a pane stream whose queued changes, beyond
	// its last tail, grow this large; the browser reconnects to a fresh tail.
	liveTextPaneBacklog = 1 << 20
	// liveTextRetiredIDs bounds the retired entry ids remembered, so a late
	// chunk or outbox change of a retired entry is ignored, not shown again.
	liveTextRetiredIDs = 256
	// liveTextSaveWindow is how long a finished reply has to reach the saved
	// file before Nano records it for investigation.
	liveTextSaveWindow = 20 * time.Second
	// liveTextWarmDelay is how long a warm pane waits after a publish before
	// it reads; a further publish restarts the wait, so a burst costs one read.
	liveTextWarmDelay = time.Second
)

// liveTextEntry is one piece of live text as panes see it.
type liveTextEntry struct {
	ID                string `json:"id"`
	Kind              string `json:"kind"` // reply | prompt
	Text              string `json:"text"`
	State             string `json:"state,omitempty"`    // a prompt's delivery state; "done" for a finished reply
	ActionID          string `json:"actionId,omitempty"` // a prompt's client action id
	SenderSubmittedAt string `json:"senderSubmittedAt,omitempty"`
	// A prompt's allowed actions and label (promptPresentation).
	AllowedActions []string `json:"allowedActions,omitempty"`
	Label          string   `json:"label,omitempty"`
	// outbox is the stored prompt record last presented, presented again
	// when its delivery turns slow.
	outbox prompts.OutboxEntry
	// key is the exact identity a saved row carries. Grok replies update it
	// to each chunk's eventId while their public entry ID stays stable.
	key string
	// order is when a reply began or a prompt was delivered, and endOrder when
	// a reply ended (its block finished or its turn ended), in the loop's
	// event order.
	order, endOrder int64
	queued          bool
	// rank and sequence order queued prompts as delivery claims them; rank is
	// -1 until read.
	rank      int
	sequence  uint64
	turn      string
	delivered bool
	// finished records that the provider reported the reply complete.
	finished bool
}

type liveReplyUpdate struct {
	key, text, turn string
	done            bool
	// started records that the live stream showed the reply start; a Codex
	// reply first seen mid-stream, after a restart, waits for its saved row.
	started bool
}

// liveTextEvent is one producer change. The loop applies them in arrival
// order, which decides display order.
type liveTextEvent struct {
	outbox    *prompts.OutboxEntry
	reply     *liveReplyUpdate
	grok      *grokLiveUpdate
	turnEnded string
	// unsaved is the key of a finished reply whose save window has passed.
	unsaved string
	// slow is the id of a prompt entry whose delivery may have turned slow.
	slow string
}

// liveTextInbox holds producer changes until the loop drains them, so a
// producer never blocks. A reply's newer full text replaces its pending
// update in place, keeping the position of its first arrival.
type liveTextInbox struct {
	events      []liveTextEvent
	replyAt     map[string]int
	applied     bool
	subscribe   []*liveTextPane
	unsubscribe []*liveTextPane
	// warm is set when warm panes are due their coalesced read.
	warm bool
	// texts asks for the text of the live entries, answered once drained.
	texts []chan<- []string
}

func (in *liveTextInbox) addReply(update liveReplyUpdate) {
	if index, ok := in.replyAt[update.key]; ok {
		update.done = update.done || in.events[index].reply.done
		update.started = update.started || in.events[index].reply.started
		in.events[index].reply = &update
		return
	}
	if in.replyAt == nil {
		in.replyAt = make(map[string]int)
	}
	in.replyAt[update.key] = len(in.events)
	in.events = append(in.events, liveTextEvent{reply: &update})
}

// LiveTextLoop owns ordered live evidence and its handoff to saved transcript rows.
type LiveTextLoop struct {
	sources LiveTextSources
	sess    *Session

	mu      sync.Mutex
	inbox   liveTextInbox
	running bool
	wake    chan struct{}

	// Owned by the loop goroutine.
	entries     []*liveTextEntry
	byID        map[string]*liveTextEntry
	retired     map[string]bool
	retiredFIFO []string
	panes       map[*liveTextPane]bool
	nextOrder   int64
	// awaiting holds when each finished reply finished, and its turn, by key,
	// until its save window's check, whatever the panes show by then.
	awaiting map[string]awaitedReply
	grok     grokLiveResponse
	// warmTimer posts the warm panes' read once publishes pause.
	warmTimer *time.Timer
}

type awaitedReply struct {
	finishedAt time.Time
	turn       string
}

// ObserveBatch takes the live adapter's parsed changes for one frame. Only
// top-level assistant text with an exact native id becomes live text; a
// provisional block index never matches a saved row.
func (l *LiveTextLoop) observeBatch(batch model.Batch, turn string) {
	if l == nil {
		return
	}
	if l.sess != nil && l.sess.Provider() == "grok" {
		l.observeGrokBatch(batch)
		return
	}
	codex := l.sess != nil && l.sess.Provider() == "codex"
	var updates []liveReplyUpdate
	var ended []string
	for _, change := range batch.Changes {
		// A Codex turn end is the parsed terminal turn the frame carried.
		if turn := change.Turn; codex && turn != nil && turn.Scope.NativeTurnID != nil && len(turn.Completion) > 0 && turn.Status != model.TurnInProgress {
			ended = append(ended, *turn.Scope.NativeTurnID)
		}
		item := change.Item
		if item == nil || item.Type != model.ItemAgentMessage || item.Role != model.RoleAssistant ||
			item.Sidechain || item.Scope.ParentNativeID != nil || item.Scope.NativeParentCallID != nil || item.Text == "" {
			continue
		}
		update := liveReplyUpdate{key: item.ExactNativeItemID(), text: item.Text, turn: turn, done: item.Complete, started: !codex}
		if codex {
			if item.Scope.NativeTurnID != nil {
				update.turn = *item.Scope.NativeTurnID
			}
			for _, observation := range item.Observations {
				// item/started is a snapshot that is not complete.
				if observation.Kind == model.ObservationSnapshot && observation.Complete != nil && !*observation.Complete {
					update.started = true
					break
				}
			}
		}
		if update.key != "" {
			updates = append(updates, update)
		}
	}
	if len(updates) == 0 && len(ended) == 0 {
		return
	}
	l.post(func(in *liveTextInbox) {
		for _, update := range updates {
			in.addReply(update)
		}
		for _, turn := range ended {
			in.events = append(in.events, liveTextEvent{turnEnded: turn})
		}
	})
}

// TurnEnded observes completion of a provider turn.
func (l *LiveTextLoop) turnEnded(turn string) {
	if l == nil || turn == "" {
		return
	}
	l.post(func(in *liveTextInbox) { in.events = append(in.events, liveTextEvent{turnEnded: turn}) })
}

// OutboxChanged posts outbox changes together, so a pane sees them in one
// change: a resent prompt replaces the one it retires with no moment showing
// neither.
func (l *LiveTextLoop) outboxChanged(entries ...prompts.OutboxEntry) {
	if l == nil || len(entries) == 0 {
		return
	}
	l.post(func(in *liveTextInbox) {
		for i := range entries {
			in.events = append(in.events, liveTextEvent{outbox: &entries[i]})
		}
	})
}

// Applied notifies the view that saved transcript evidence changed.
func (l *LiveTextLoop) applied() {
	if l == nil {
		return
	}
	l.post(func(in *liveTextInbox) { in.applied = true })
}

func (l *LiveTextLoop) subscribe(pane *liveTextPane) {
	l.post(func(in *liveTextInbox) { in.subscribe = append(in.subscribe, pane) })
}

func (l *LiveTextLoop) unsubscribe(pane *liveTextPane) {
	pane.close()
	l.post(func(in *liveTextInbox) { in.unsubscribe = append(in.unsubscribe, pane) })
}

func (l *LiveTextLoop) post(add func(*liveTextInbox)) {
	l.mu.Lock()
	add(&l.inbox)
	start := !l.running
	l.running = true
	l.mu.Unlock()
	if start {
		go l.run()
	}
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *LiveTextLoop) run() {
	l.reset()
	l.seedOutbox()
	timer := time.NewTimer(liveTextIdleStop)
	defer timer.Stop()
	for {
		select {
		case <-l.wake:
			l.mu.Lock()
			in := l.inbox
			l.inbox = liveTextInbox{}
			l.mu.Unlock()
			l.process(in)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(liveTextIdleStop)
		case <-timer.C:
			l.mu.Lock()
			idle := len(l.panes) == 0 && len(l.awaiting) == 0 && inboxEmpty(l.inbox) && !l.sessionWorking()
			if idle {
				l.running = false
				l.mu.Unlock()
				return
			}
			l.mu.Unlock()
			timer.Reset(liveTextIdleStop)
		}
	}
}

func inboxEmpty(in liveTextInbox) bool {
	return len(in.events) == 0 && !in.applied && len(in.subscribe) == 0 && len(in.unsubscribe) == 0 && !in.warm && len(in.texts) == 0
}

// CurrentTexts is the text of the prompts and replies this thread shows as
// live text now, for links in text the file has not saved. A stopped loop
// holds none.
func (l *LiveTextLoop) CurrentTexts(ctx context.Context) []string {
	answer := make(chan []string, 1)
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return nil
	}
	l.inbox.texts = append(l.inbox.texts, answer)
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
	select {
	case texts := <-answer:
		return texts
	case <-ctx.Done():
		return nil
	}
}

func (l *LiveTextLoop) sessionWorking() bool {
	if l.sess == nil {
		return false
	}
	return l.sess.view.LiveTurnActive()
}

func (l *LiveTextLoop) reset() {
	l.entries, l.byID = nil, make(map[string]*liveTextEntry)
	l.retired, l.retiredFIFO = make(map[string]bool), nil
	l.panes = make(map[*liveTextPane]bool)
	l.awaiting = make(map[string]awaitedReply)
	l.grok = grokLiveResponse{}
	if l.warmTimer != nil {
		l.warmTimer.Stop()
		l.warmTimer = nil
	}
}

func (l *LiveTextLoop) process(in liveTextInbox) {
	for _, pane := range in.unsubscribe {
		delete(l.panes, pane)
	}
	created := false
	var unsaved []string
	for _, event := range in.events {
		switch {
		case event.outbox != nil:
			created = l.applyOutbox(*event.outbox, -1) || created
		case event.grok != nil:
			created = l.applyGrokUpdate(*event.grok) || created
		case event.reply != nil:
			created = l.applyReply(*event.reply) || created
		case event.unsaved != "":
			unsaved = append(unsaved, event.unsaved)
		case event.slow != "":
			if current := l.byID[event.slow]; current != nil {
				l.presentPrompt(current, current.outbox)
			}
		default:
			l.endTurn(event.turnEnded)
		}
	}
	joining := make(map[*liveTextPane]bool)
	for _, pane := range in.subscribe {
		if !pane.isClosed() {
			l.panes[pane], joining[pane] = true, true
		}
	}
	// No pane sees new live text before a read has checked it: a prompt or a
	// block the provider resends may already be saved. A joining pane gets
	// its tail from the same read as every other pane.
	if in.applied {
		l.armWarm()
	}
	if in.applied || created || len(joining) > 0 {
		l.tick(joining, false)
	}
	if in.warm {
		l.tick(nil, true)
	}
	l.sendLiveChanges()
	for _, key := range unsaved {
		l.checkUnsaved(key)
	}
	if len(in.texts) > 0 {
		texts := make([]string, 0, len(l.entries))
		for _, entry := range l.entries {
			texts = append(texts, entry.Text)
		}
		for _, answer := range in.texts {
			answer <- texts
		}
	}
}

// applyReply updates or creates a reply entry and reports a new one.
func (l *LiveTextLoop) applyReply(update liveReplyUpdate) bool {
	id := "reply:" + update.key
	if l.retired[id] {
		return false // a late chunk of a saved or superseded reply
	}
	entry, existing := l.byID[id]
	if !existing && !update.started {
		// Not shown, but still checked: the save window holds whatever the
		// panes show.
		if update.done {
			l.awaitSave(update.key, update.turn)
		}
		return false
	}
	if !existing {
		entry = &liveTextEntry{ID: id, Kind: "reply", key: update.key, turn: update.turn, order: l.next(), delivered: true}
		l.byID[id] = entry
		l.entries = append(l.entries, entry)
	}
	entry.Text = update.text
	if update.done && !entry.finished {
		entry.finished = true
		l.end(entry)
		l.awaitSave(entry.key, entry.turn)
	}
	return !existing
}

func (l *LiveTextLoop) endTurn(turn string) {
	for _, entry := range l.entries {
		if entry.Kind == "reply" && entry.turn == turn {
			l.end(entry)
		}
	}
}

// end marks a reply ended once: its block finished or its turn ended.
func (l *LiveTextLoop) end(entry *liveTextEntry) {
	entry.State = "done"
	if entry.endOrder == 0 {
		entry.endOrder = l.next()
	}
}

// awaitSave notes a reply the provider reported finished and checks, a save
// window later, that the saved file holds it.
func (l *LiveTextLoop) awaitSave(key, turn string) {
	if _, waiting := l.awaiting[key]; waiting {
		return
	}
	l.awaiting[key] = awaitedReply{finishedAt: time.Now(), turn: turn}
	time.AfterFunc(liveTextSaveWindow, func() {
		l.post(func(in *liveTextInbox) { in.events = append(in.events, liveTextEvent{unsaved: key}) })
	})
}

// checkUnsaved records, content-free and for investigation, a finished reply
// whose saved row is not in the file a save window after it finished. It
// reads the file itself, so it holds whatever the panes show by then.
func (l *LiveTextLoop) checkUnsaved(key string) {
	awaited, ok := l.awaiting[key]
	if !ok {
		return
	}
	finishedAt := awaited.finishedAt
	delete(l.awaiting, key)
	threadID, err := l.catalogThreadID()
	saved := false
	if err == nil {
		var matched map[string]string
		_, matched, err = l.read(threadID, nil, []history.TranscriptMatch{l.replyMatch(map[string]string{key: awaited.turn})})
		saved = matched[key] != ""
	}
	if err != nil && !notIndexed(err) {
		log.Printf("live text save check %s: %v", l.hostedID(), err)
		return
	}
	if saved {
		return
	}
	deferred := l.sources.Catalog != nil && l.sources.Catalog.DeferredTranscriptReread(threadID)
	l.reportUnsaved(key, finishedAt, deferred)
}

// reportUnsaved publishes the bounded diagnostic from the observed file facts.
func (l *LiveTextLoop) reportUnsaved(key string, finishedAt time.Time, deferred bool) {
	elapsed := time.Since(finishedAt)
	label := ""
	if deferred {
		label = "deferred re-read"
	}
	log.Printf("live text reply %s of %s finished %s ago with no saved row label=%q", key, l.hostedID(), elapsed.Round(time.Second), label)
	if l.sources.Diagnostics == nil {
		return
	}
	l.sources.Diagnostics.Add([]diagnostics.Event{{
		EventID: "file.reply.unsaved:" + l.hostedID() + ":" + key, Kind: "file.reply.unsaved",
		ClientAt: finishedAt.UTC().Format(time.RFC3339Nano), Provider: l.sess.Provider(),
		HostedThreadID: l.hostedID(), ThreadID: l.sess.stateOwner.ThreadID(), Code: key,
		DurationMS: int(elapsed / time.Millisecond),
		Status:     strings.ReplaceAll(label, " ", "-"),
	}})
}

func (l *LiveTextLoop) next() int64 {
	l.nextOrder++
	return l.nextOrder
}

// drop removes an entry from every pane and remembers it as retired.
func (l *LiveTextLoop) drop(entry *liveTextEntry) {
	if entry == nil {
		return
	}
	delete(l.byID, entry.ID)
	for index, candidate := range l.entries {
		if candidate == entry {
			l.entries = append(l.entries[:index], l.entries[index+1:]...)
			break
		}
	}
	l.retired[entry.ID] = true
	l.retiredFIFO = append(l.retiredFIFO, entry.ID)
	if len(l.retiredFIFO) > liveTextRetiredIDs {
		delete(l.retired, l.retiredFIFO[0])
		l.retiredFIFO = l.retiredFIFO[1:]
	}
}

// visible is the live-text region in its display order: delivered prompts
// and replies in the order they began, then queued prompts in the order
// delivery will claim them. Every pane shows the same region.
func (l *LiveTextLoop) visible() []liveTextEntry {
	out := make([]liveTextEntry, 0, len(l.entries))
	for _, entry := range l.entries {
		out = append(out, *entry)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].queued != out[j].queued {
			return !out[i].queued
		}
		if out[i].queued {
			if out[i].rank != out[j].rank {
				return out[i].rank < out[j].rank
			}
			return out[i].sequence < out[j].sequence
		}
		return out[i].order < out[j].order
	})
	return out
}

// liveTextStatus is a thread's runtime and pending questions at a projection
// revision. Only the stream handler writes it, and it never carries rows.

// shownOnScreen reports a pane on screen: one that shows live text.
func (l *LiveTextLoop) shownOnScreen() bool {
	for pane := range l.panes {
		if !pane.rowsOnly {
			return true
		}
	}
	return false
}

// armWarm starts, or restarts, the wait before the warm panes' read. Warm
// panes riding a pane on screen's reads need none.
func (l *LiveTextLoop) armWarm() {
	if l.shownOnScreen() {
		return
	}
	for pane := range l.panes {
		if !pane.warm {
			continue
		}
		if l.warmTimer == nil {
			l.warmTimer = time.AfterFunc(liveTextWarmDelay, func() { l.post(func(in *liveTextInbox) { in.warm = true }) })
		} else {
			l.warmTimer.Reset(liveTextWarmDelay)
		}
		return
	}
}

func (l *LiveTextLoop) hostedID() string {
	if l.sess == nil {
		return ""
	}
	return l.sess.HostedThreadID()
}

// LiveTextSources identifies the existing saved-read and diagnostic owners.
type LiveTextSources struct {
	Catalog     *threadindex.Service
	Diagnostics *diagnostics.Store
}

// NewLiveTextLoop constructs the ordered saved/live view for one attachment.
func NewLiveTextLoop(sources LiveTextSources, sess *Session) *LiveTextLoop {
	return &LiveTextLoop{sources: sources, sess: sess, wake: make(chan struct{}, 1)}
}

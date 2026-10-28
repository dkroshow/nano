package app

// live_text.go — file-only transcripts. A thread's rows come only from the
// provider's saved file (the catalog). The live stream adds live text: the
// reply being streamed and the prompts the outbox holds that the file does
// not have yet. One loop per thread orders live text, outbox changes and
// catalog applies, matches saved rows to live text by exact identity, and
// pushes each pane one ordered stream in which a saved row and the removal of
// its live text always travel in the same message. The browser matches
// nothing. Live text is never stored, merged into rows, or served to agents.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/liveview"
	threadvalues "github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

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

// startLiveText builds the file-only loop for every session constructor.
func (s *server) startLiveText(sess *session) {
	sess.attachLiveText(newLiveTextLoop(s, sess))
}

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

type liveTextLoop struct {
	srv  *server
	sess liveTextAttachment

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
func (l *liveTextLoop) ObserveBatch(batch model.Batch, turn string) {
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
func (l *liveTextLoop) TurnEnded(turn string) {
	if l == nil || turn == "" {
		return
	}
	l.post(func(in *liveTextInbox) { in.events = append(in.events, liveTextEvent{turnEnded: turn}) })
}

// OutboxChanged posts outbox changes together, so a pane sees them in one
// change: a resent prompt replaces the one it retires with no moment showing
// neither.
func (l *liveTextLoop) OutboxChanged(entries ...prompts.OutboxEntry) {
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
func (l *liveTextLoop) Applied() {
	if l == nil {
		return
	}
	l.post(func(in *liveTextInbox) { in.applied = true })
}

func (l *liveTextLoop) subscribe(pane *liveTextPane) {
	l.post(func(in *liveTextInbox) { in.subscribe = append(in.subscribe, pane) })
}

func (l *liveTextLoop) unsubscribe(pane *liveTextPane) {
	pane.close()
	l.post(func(in *liveTextInbox) { in.unsubscribe = append(in.unsubscribe, pane) })
}

func (l *liveTextLoop) post(add func(*liveTextInbox)) {
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

func (l *liveTextLoop) run() {
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
func (l *liveTextLoop) CurrentTexts(ctx context.Context) []string {
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

func (l *liveTextLoop) sessionWorking() bool {
	if l.sess == nil {
		return false
	}
	return l.sess.LiveTurnActive()
}

func (l *liveTextLoop) reset() {
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

// seedOutbox starts a new loop from a read-only snapshot of the outbox, so a
// queued or parked prompt shows on reopening. Delivery state is never changed.
// No pane exists yet; each pane joins through a tick that checks them first.
func (l *liveTextLoop) seedOutbox() {
	if l.sess == nil || l.sess.PromptStore() == nil {
		return
	}
	records, err := l.sess.PromptStore().List(context.Background(), l.sess.HostedThreadID())
	if err != nil {
		log.Printf("live text outbox snapshot %s: %v", l.sess.HostedThreadID(), err)
		return
	}
	for _, record := range records {
		l.applyOutbox(record.Outbox, record.DeliveryRank)
	}
}

func (l *liveTextLoop) process(in liveTextInbox) {
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

// applyOutbox applies one outbox change and reports whether a read must check
// it: a new entry, or a newly learned key. rank is the prompt's delivery rank
// when known, else -1.
func (l *liveTextLoop) applyOutbox(entry prompts.OutboxEntry, rank int) bool {
	if entry.ActionID == "" {
		return false
	}
	id := "prompt:" + entry.ActionID
	current := l.byID[id]
	switch entry.State {
	case prompts.Refused, prompts.Expired:
		// Refused, cancelled or discarded: no saved row will follow. Expired:
		// an old delivered prompt exact identity never settled, no longer shown.
		l.drop(current)
		return false
	case prompts.Confirmed:
		// The file holds its row now, but the prompt leaves only with its row
		// in a tick's read, so the row and the removal arrive together.
		if current != nil {
			l.presentPrompt(current, entry)
			return l.learnKey(current, entry)
		}
		return false
	}
	created := false
	if current == nil {
		if l.retired[id] {
			return false
		}
		current = &liveTextEntry{ID: id, Kind: "prompt", ActionID: entry.ActionID, key: l.promptKey(entry), rank: rank}
		if l.sess != nil {
			turn, found, err := l.sess.PersistedOperation(context.Background(), entry.ActionID)
			if err != nil {
				log.Printf("live text submission metadata %s: %v", l.hostedID(), err)
			} else if found && turn.Origin == turnOriginAgent && turn.SenderSubmittedAt != "" {
				current.SenderSubmittedAt = turn.SenderSubmittedAt
			}
		}
		l.byID[id] = current
		l.entries = append(l.entries, current)
		created = true
	}
	current.Text = entry.PresentationText
	l.presentPrompt(current, entry)
	learned := l.learnKey(current, entry)
	current.sequence = entry.Sequence
	current.queued = entry.State == prompts.Queued || entry.State == prompts.Parked
	if current.queued && current.rank < 0 {
		current.rank = l.deliveryRank(entry.ActionID)
	}
	if !current.queued && !current.delivered {
		current.delivered, current.order = true, l.next()
	}
	return created || learned
}

// presentPrompt shows a prompt's stored state as the delivery journal does.
// A delivery that turns slow changes its label with no store change, so the
// loop presents it again then, once per delivery claim.
func (l *liveTextLoop) presentPrompt(current *liveTextEntry, entry prompts.OutboxEntry) {
	provider := ""
	if l.sess != nil {
		provider = l.sess.Provider()
	}
	newClaim := entry.UpdatedAt != current.outbox.UpdatedAt || current.outbox.State != prompts.Delivering
	current.outbox = entry
	current.State = string(entry.State)
	current.AllowedActions, current.Label = promptPresentation(provider, entry)
	if slow, ok := slowDeliveryAt(entry); ok && newClaim && current.Label == "" {
		id := current.ID
		time.AfterFunc(time.Until(slow), func() {
			l.post(func(in *liveTextInbox) { in.events = append(in.events, liveTextEvent{slow: id}) })
		})
	}
}

// learnKey gives a prompt its exact key once the outbox knows it, and reports
// that a read must check it: a Codex or Grok prompt learns its turn on delivery, and
// its saved row may already be in the file.
func (l *liveTextLoop) learnKey(current *liveTextEntry, entry prompts.OutboxEntry) bool {
	learned := false
	if l.sess != nil && (l.sess.Provider() == "grok" || l.sess.Provider() == "codex") && entry.Binding != nil && entry.Binding.Scope.NativeTurnID != nil && current.turn != *entry.Binding.Scope.NativeTurnID {
		current.turn = *entry.Binding.Scope.NativeTurnID
		learned = true
	}
	if current.key == "" {
		current.key = l.promptKey(entry)
		learned = learned || current.key != ""
	}
	return learned
}

// deliveryRank reads the rank delivery claims a queued prompt by, so queued
// prompts show in the order they will be delivered.
func (l *liveTextLoop) deliveryRank(actionID string) int {
	if l.sess == nil || l.sess.PromptStore() == nil {
		return 0
	}
	record, _, err := l.sess.PromptStore().Get(context.Background(), actionID)
	if err != nil {
		log.Printf("live text queue rank %s: %v", actionID, err)
		return 0
	}
	return record.DeliveryRank
}

// promptKey is the exact native identity a prompt's saved user row carries:
// the UUID Nano stamps on Claude and Grok submissions and sends Codex as its
// client message id, or, for a Codex prompt delivered without one, the native
// turn it started.
func (l *liveTextLoop) promptKey(entry prompts.OutboxEntry) string {
	if l.sess != nil && l.sess.Provider() == "codex" {
		switch {
		case entry.Binding == nil:
			return ""
		case entry.Binding.EventID != "":
			return entry.Binding.EventID
		case entry.Binding.Scope.NativeTurnID != nil:
			return *entry.Binding.Scope.NativeTurnID
		}
		return ""
	}
	if entry.Binding != nil && entry.Binding.EventID != "" {
		return entry.Binding.EventID
	}
	if l.sess != nil && (l.sess.Provider() == "claude" || l.sess.Provider() == "grok") {
		return submissionEventID(l.sess.HostedThreadID(), entry.ActionID)
	}
	return ""
}

// applyReply updates or creates a reply entry and reports a new one.
func (l *liveTextLoop) applyReply(update liveReplyUpdate) bool {
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

func (l *liveTextLoop) endTurn(turn string) {
	for _, entry := range l.entries {
		if entry.Kind == "reply" && entry.turn == turn {
			l.end(entry)
		}
	}
}

// end marks a reply ended once: its block finished or its turn ended.
func (l *liveTextLoop) end(entry *liveTextEntry) {
	entry.State = "done"
	if entry.endOrder == 0 {
		entry.endOrder = l.next()
	}
}

// awaitSave notes a reply the provider reported finished and checks, a save
// window later, that the saved file holds it.
func (l *liveTextLoop) awaitSave(key, turn string) {
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
func (l *liveTextLoop) checkUnsaved(key string) {
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
	elapsed := time.Since(finishedAt)
	label := ""
	if l.srv != nil {
		if catalog, ok := l.srv.catalog.(*threadCatalogService); ok && catalog.deferredTranscriptReread(threadID) {
			label = "deferred re-read"
		}
	}
	log.Printf("live text reply %s of %s finished %s ago with no saved row label=%q", key, l.hostedID(), elapsed.Round(time.Second), label)
	if l.srv == nil || l.srv.browserDiagnostics == nil {
		return
	}
	l.srv.browserDiagnostics.add([]browserDiagnosticEvent{{
		EventID: "file.reply.unsaved:" + l.hostedID() + ":" + key, Kind: "file.reply.unsaved",
		ClientAt: finishedAt.UTC().Format(time.RFC3339Nano), Provider: l.sess.Provider(),
		HostedThreadID: l.hostedID(), ThreadID: l.sess.ThreadID(), Code: key,
		DurationMS: int(elapsed / time.Millisecond),
		Status:     strings.ReplaceAll(label, " ", "-"),
	}})
}

func (l *liveTextLoop) next() int64 {
	l.nextOrder++
	return l.nextOrder
}

// drop removes an entry from every pane and remembers it as retired.
func (l *liveTextLoop) drop(entry *liveTextEntry) {
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
func (l *liveTextLoop) visible() []liveTextEntry {
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

// proofs is what one read proves: saved names the entries whose saved rows
// it holds; gone adds the ended replies the saved row of a prompt delivered
// after them supersedes, since an unsaved reply's text stays until the next
// prompt. Only a prompt proves a later turn: a reply may be a replay. For
// Claude this is exact, because the file is written and indexed in order.
func (l *liveTextLoop) proofs(matched map[string]string, ended map[string]bool) (saved, gone map[string]bool) {
	saved, gone = make(map[string]bool), make(map[string]bool)
	var latest int64
	for _, entry := range l.entries {
		if entry.key != "" && matched[entry.key] != "" || l.sess.Provider() == "grok" && entry.Kind == "prompt" && entry.turn != "" && matched[entry.turn] != "" {
			saved[entry.ID], gone[entry.ID] = true, true
			if entry.Kind == "prompt" {
				latest = max(latest, entry.order)
			}
		}
	}
	for _, entry := range l.entries {
		if entry.Kind == "reply" && (entry.endOrder > 0 && entry.endOrder < latest || ended[entry.turn]) {
			gone[entry.ID] = true
		}
	}
	return saved, gone
}

// reloadEnds names the Codex turns whose end the file records, when this
// read reloads a browser pane: a new pane, or a fresh tail after a rebuild. A
// Codex Stop leaves no saved reply, so a reload clears the stopped reply's
// text once the file proves its turn ended; Claude saves a stopped reply and
// needs none. An agent stream never reloads what the operator sees.
func (l *liveTextLoop) reloadEnds(panes []*liveTextPane, joining map[*liveTextPane]bool, responses []transcriptResponse) map[string]bool {
	ended := map[string]bool{}
	if l.sess.Provider() != "codex" {
		return ended
	}
	reloading := false
	for i, pane := range panes {
		// A pane's first tail once the thread is indexed replaces nothing.
		reloading = reloading || !pane.rowsOnly && (joining[pane] || pane.key != "" && pane.needsTail(responses[i]))
	}
	if !reloading {
		return ended
	}
	for _, response := range responses {
		for _, turn := range response.Turns {
			// A page reads a turn in progress until the file records its end.
			if turn.NativeTurnID != "" && turn.Status != string(history.TurnInProgress) {
				ended[turn.NativeTurnID] = true
			}
		}
	}
	return ended
}

// liveTextPane is one open pane: one profile of one thread on one stream.
type liveTextPane struct {
	query transcriptListQuery
	// rowsOnly marks a stream of rows, never live text: an agent's, or a warm
	// stream's.
	rowsOnly bool
	// warm marks a warm-stream pane, a thread whose saved rows the browser
	// keeps ready (warm_transcripts.go). It rides a pane on screen's reads,
	// otherwise reads on its own coalesced schedule, and tags its messages
	// with ref. reads counts its stream's page reads.
	warm  bool
	ref   string
	reads *atomic.Int64
	// A warm pane's window: the fill rule its rows are chosen by, and the
	// rows the browser already holds, used by its first read and then
	// dropped (warm_transcripts.go).
	fill   warmFill
	marker *warmMarker

	// The stream this pane writes to; a warm stream's panes share one.
	*liveTextQueue
	closed bool // guarded by the queue's mutex

	// Owned by the loop goroutine.
	key    string
	rows   map[string]liveRowState
	maxSeq int64
	sent   liveSent
}

// liveTextQueue is one stream's unsent messages and its one backlog budget.
type liveTextQueue struct {
	mu     sync.Mutex
	queue  []liveTextQueued
	bytes  int
	limit  int // counted bytes that end the stream
	closed bool
	ready  chan struct{}
	done   chan struct{}
}

type liveTextQueued struct {
	pane *liveTextPane
	raw  []byte
	// counted marks bytes the backlog budget holds: everything but a pane
	// stream's tail.
	counted bool
}

// liveSent is what a pane last received of the live-text region.
type liveSent struct {
	shape string
	texts map[string]string
}

type liveRowState struct {
	seq    int64
	digest [sha256.Size]byte
}

func newLiveTextQueue() *liveTextQueue {
	return newLiveTextQueueWithBacklog(liveTextPaneBacklog)
}

func newLiveTextQueueWithBacklog(limit int) *liveTextQueue {
	return &liveTextQueue{limit: limit, ready: make(chan struct{}, 1), done: make(chan struct{})}
}

func newLiveTextPane(query transcriptListQuery) *liveTextPane {
	query.Before, query.BeforeNativeTurn, query.BeforeTurn, query.TurnLimit = "", "", 0, 0
	return &liveTextPane{query: query, liveTextQueue: newLiveTextQueue()}
}

func (p *liveTextPane) send(message liveTextMessage) {
	message.Ref = p.ref
	if p.query.View == defaultThreadTranscriptView {
		message.Rows, message.Turns = enhancedRows(message.Rows), enhancedTurns(message.Turns)
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return
	}
	q := p.liveTextQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	if p.closed || q.closed {
		return
	}
	tail := message.Type == "tail"
	if tail {
		// A tail replaces everything its pane queued before it.
		p.dropQueuedLocked()
	}
	// A pane stream's tail must always land, so only the changes queued after
	// it count as backlog. A warm stream counts its tails too: they are
	// bounded, and the stream can drop them.
	q.push(liveTextQueued{pane: p, raw: raw, counted: !tail || p.warm})
}

// push queues one message, or ends the stream once the counted backlog
// would pass its budget; the browser reconnects to fresh tails.
func (q *liveTextQueue) push(m liveTextQueued) {
	if m.counted && q.bytes+len(m.raw) > q.limit {
		q.queue = nil
		q.closeLocked()
		return
	}
	if m.counted {
		q.bytes += len(m.raw)
	}
	q.queue = append(q.queue, m)
	q.signalLocked()
}

// fail ends a pane's stream with one error message, which the browser shows
// instead of reconnecting. A warm stream carries on without that thread.
func (p *liveTextPane) fail(message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed && !p.liveTextQueue.closed {
		p.failLocked(message)
	}
}

func (p *liveTextPane) failLocked(message string) {
	raw, _ := json.Marshal(liveTextMessage{Type: "error", Ref: p.ref, Error: message})
	q := p.liveTextQueue
	if p.warm {
		p.dropQueuedLocked()
		p.closed = true
		q.push(liveTextQueued{pane: p, raw: raw, counted: true})
		return
	}
	q.queue = []liveTextQueued{{pane: p, raw: raw}}
	q.closeLocked()
}

// close ends the pane: its queued messages go unsent, and a pane's own
// stream ends with it.
func (p *liveTextPane) close() {
	q := p.liveTextQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	p.dropQueuedLocked()
	p.closed = true
	if !p.warm {
		q.closeLocked()
	}
}

func (p *liveTextPane) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed || p.liveTextQueue.closed
}

func (p *liveTextPane) dropQueuedLocked() {
	q := p.liveTextQueue
	q.queue = slices.DeleteFunc(q.queue, func(m liveTextQueued) bool {
		if m.pane == p && m.counted {
			q.bytes -= len(m.raw)
		}
		return m.pane == p
	})
}

func (q *liveTextQueue) take() [][]byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([][]byte, 0, len(q.queue))
	for _, m := range q.queue {
		out = append(out, m.raw)
	}
	q.queue, q.bytes = nil, 0
	return out
}

func (q *liveTextQueue) signalLocked() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *liveTextQueue) closeLocked() {
	if !q.closed {
		q.closed = true
		close(q.done)
	}
}

type liveTextMessage struct {
	Type string `json:"type"`
	// Ref names the thread of a warm stream's message; Refs, on a warm
	// stream's set message, every thread it now carries.
	Ref        string                `json:"ref,omitempty"`
	Refs       []string              `json:"refs,omitempty"`
	ThreadID   string                `json:"threadId,omitempty"`
	Key        string                `json:"key,omitempty"`
	Rows       []transcriptItem      `json:"rows,omitempty"`
	Removed    []string              `json:"removed,omitempty"`
	Turns      []transcriptTurn      `json:"turns,omitempty"`
	Page       *transcriptPageInfo   `json:"page,omitempty"`
	Projection *transcriptProjection `json:"projection,omitempty"`
	Live       *[]liveTextEntry      `json:"live,omitempty"`
	// Saved names the live text this message removes because its rows arrived.
	Saved  []string        `json:"saved,omitempty"`
	ID     string          `json:"id,omitempty"`
	Append string          `json:"append,omitempty"`
	Error  string          `json:"error,omitempty"`
	Status *liveTextStatus `json:"status,omitempty"`
}

// enhancedRows and enhancedTurns are what an Enhanced pane receives: the
// fields the Enhanced view draws. The developer view's raw provider copy,
// tags, provenance and turn config stay on dev panes and the HTTP API; on
// an Enhanced stream they were over a quarter of every tail.
func enhancedRows(rows []transcriptItem) []transcriptItem {
	if rows == nil {
		return nil
	}
	out := make([]transcriptItem, len(rows))
	for i, row := range rows {
		row.Detail, row.FilterTags, row.Synthetic, row.ToolName, row.CallID = nil, nil, false, "", ""
		out[i] = row
	}
	return out
}

func enhancedTurns(turns []transcriptTurn) []transcriptTurn {
	if turns == nil {
		return nil
	}
	out := make([]transcriptTurn, len(turns))
	for i, turn := range turns {
		turn.Config, turn.NativeTurnID, turn.FinalitySource, turn.FinalityConfidence, turn.CompletionEvidence = nil, "", "", "", ""
		out[i] = turn
	}
	return out
}

// liveTextStatus is a thread's runtime and pending questions at a projection
// revision. Only the stream handler writes it, and it never carries rows.
type liveTextStatus = liveview.Status

// replyMatch and promptMatch look up the exact identities a provider's saved
// rows carry: a Claude reply's message block and prompt's stamped UUID; a
// Codex reply's item id in its turn, and prompt's turn; Grok's newest chunk
// event id and the Nano submission id. Grok prompts also prove their saved turn
// in tick. replyMatch takes each reply's key with its turn.
func (l *liveTextLoop) replyMatch(turns map[string]string) history.TranscriptMatch {
	keys := make([]string, 0, len(turns))
	for key := range turns {
		keys = append(keys, key)
	}
	if l.sess.Provider() == "grok" {
		return history.TranscriptMatch{Namespace: "native.eventId", ItemType: "agent_message", Keys: keys}
	}
	if l.sess.Provider() == "codex" {
		return history.TranscriptMatch{Namespace: history.MatchTurnNativeItem, ItemType: "agent_message", Keys: keys, Turns: turns}
	}
	return history.TranscriptMatch{Namespace: l.sess.Provider() + ".messageBlock", ItemType: "agent_message", Keys: keys}
}

// claimedTurnEvents names, for each turn a Codex prompt without a client id
// waits on, the client ids of the other prompts bound to that turn. Their
// rows are theirs, so the turn's remaining row is this prompt's. It reads the
// durable bindings, not live entries: a prompt that already left, or was
// confirmed before a restart, still claims its row. A turn whose claims cannot
// be read is left out of this tick's keys: without them its row is unproven.
func (l *liveTextLoop) claimedTurnEvents(turns []string) (map[string][]string, []string) {
	if l.sess.PromptStore() == nil {
		return nil, nil
	}
	claimed := make(map[string][]string)
	readable := make(map[string]bool) // one read per distinct turn
	var proven []string
	for _, turn := range turns {
		ok, read := readable[turn]
		if !read {
			events, err := l.sess.PromptStore().TurnEvents(context.Background(), l.sess.HostedThreadID(), model.ProviderCodex, l.sess.ThreadID(), turn)
			if ok = err == nil; !ok {
				log.Printf("live text turn events %s: %v", turn, err)
			} else if len(events) > 0 {
				claimed[turn] = events
			}
			readable[turn] = ok
		}
		if ok {
			proven = append(proven, turn)
		}
	}
	return claimed, proven
}

func (l *liveTextLoop) promptMatch(keys []string) history.TranscriptMatch {
	if l.sess.Provider() == "grok" {
		return history.TranscriptMatch{Namespace: "nano.submissionId", ItemType: "user_message", Keys: keys}
	}
	if l.sess.Provider() == "codex" {
		return history.TranscriptMatch{Namespace: history.MatchTurnItem, ItemType: "user_message", Keys: keys}
	}
	return history.TranscriptMatch{Namespace: "native.eventId", ItemType: "user_message", Keys: keys}
}

// liveTextCatalog reads pages of a thread and checks exact keys in one
// catalog transaction.
type liveTextCatalog interface {
	LoadTranscripts(context.Context, string, []transcriptListQuery, []history.TranscriptMatch) ([]transcriptResponse, map[string]string, error)
}

var _ liveTextCatalog = (*threadCatalogService)(nil)

func (l *liveTextLoop) read(threadID string, queries []transcriptListQuery, match []history.TranscriptMatch) ([]transcriptResponse, map[string]string, error) {
	catalog, ok := l.srv.catalog.(liveTextCatalog)
	if !ok {
		return nil, nil, errors.New("catalog unavailable")
	}
	return catalog.LoadTranscripts(context.Background(), threadID, queries, match)
}

var errLiveTextNotIndexed = errors.New("thread not in the catalog yet")

func notIndexed(err error) bool {
	return errors.Is(err, errLiveTextNotIndexed) || errors.Is(err, errCatalogThreadNotFound)
}

func (l *liveTextLoop) catalogThreadID() (string, error) {
	if l.sess == nil {
		return "", errLiveTextNotIndexed
	}
	native := l.sess.ThreadID()
	resolver, ok := l.srv.catalog.(threadReferenceResolver)
	if native == "" || !ok {
		return "", errLiveTextNotIndexed
	}
	id, err := resolver.ResolveThreadReference(context.Background(), native)
	if errors.Is(err, errCatalogThreadNotFound) {
		return "", errLiveTextNotIndexed
	}
	return id, err
}

// tick reads every due pane's newest page and checks every held key in one
// catalog transaction, so one answer holds for every pane. Text the read
// proves saved leaves every pane in the message that brings its row, and
// retires. Joining panes get their tail from the same read, and panes asking
// for the same page share one read. Warm panes are due when they join and
// when their coalesced read comes (warm), never while a pane on screen shows
// the thread.
func (l *liveTextLoop) tick(joining map[*liveTextPane]bool, warm bool) {
	shown := l.shownOnScreen()
	var panes []*liveTextPane
	for pane := range l.panes {
		switch {
		case !pane.warm:
			if !warm {
				panes = append(panes, pane)
			}
		case warm || joining[pane] || shown:
			// A warm pane rides a pane on screen's reads (they ask for the
			// same page), so the browser's cached rows stay current while
			// the thread is shown, and no second read is made.
			panes = append(panes, pane)
		}
	}
	if len(panes) == 0 {
		return // held text is checked when a pane opens
	}
	var queries []transcriptListQuery
	page := make([]int, len(panes)) // each pane's index in queries
	asked := make(map[string]int)
	for i, pane := range panes {
		key := pageQueryKey(pane.query)
		index, ok := asked[key]
		if !ok {
			index, asked[key] = len(queries), len(queries)
			queries = append(queries, pane.query)
		}
		page[i] = index
	}
	var promptKeys, promptTurns []string
	replyTurns := make(map[string]string)
	// A Codex prompt with a client id shares its turn with others, so it is
	// found by that id within its turn; one without keeps its turn key.
	eventTurns := make(map[string]string)
	for _, entry := range l.entries {
		if l.sess.Provider() == "grok" && entry.Kind == "prompt" && entry.turn != "" {
			promptTurns = append(promptTurns, entry.turn)
		}
		switch {
		case entry.key == "":
		case entry.Kind == "prompt" && l.sess.Provider() == "codex" && entry.turn != "" && entry.key != entry.turn:
			eventTurns[entry.key] = entry.turn
		case entry.Kind == "prompt":
			promptKeys = append(promptKeys, entry.key)
		default:
			replyTurns[entry.key] = entry.turn
		}
	}
	var match []history.TranscriptMatch
	if len(promptTurns) > 0 {
		match = append(match, history.TranscriptMatch{Namespace: history.MatchTurnItem, ItemType: "user_message", Keys: promptTurns})
	}
	if l.sess.Provider() == "codex" && len(promptKeys) > 0 {
		var claimed map[string][]string
		claimed, promptKeys = l.claimedTurnEvents(promptKeys)
		if len(promptKeys) > 0 {
			prompt := l.promptMatch(promptKeys)
			prompt.Exclude = claimed
			match = append(match, prompt)
		}
	} else if len(promptKeys) > 0 {
		match = append(match, l.promptMatch(promptKeys))
	}
	if len(eventTurns) > 0 {
		keys := make([]string, 0, len(eventTurns))
		for key := range eventTurns {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		match = append(match, history.TranscriptMatch{Namespace: history.MatchTurnEvent, ItemType: "user_message", Keys: keys, Turns: eventTurns})
	}
	if len(replyTurns) > 0 {
		match = append(match, l.replyMatch(replyTurns))
	}

	threadID, err := l.catalogThreadID()
	var responses []transcriptResponse
	var matched map[string]string
	if err == nil {
		responses, matched, err = l.read(threadID, queries, match)
	}
	if notIndexed(err) {
		// Nothing is saved yet: joining panes start empty, the rest wait.
		for pane := range joining {
			if !pane.warm {
				l.tail(pane, livePage{}, nil)
			}
		}
		return
	}
	if err != nil {
		// A failed read is a bug: every pane shows it instead of retrying.
		log.Printf("live text read %s: %v", l.hostedID(), err)
		for _, pane := range panes {
			pane.fail("the saved transcript could not be read")
			delete(l.panes, pane)
		}
		return
	}
	pages := make([]livePage, len(responses))
	for i, response := range responses {
		pages[i] = newLivePage(response)
	}
	paneResponses := make([]transcriptResponse, len(panes))
	for i := range panes {
		paneResponses[i] = responses[page[i]]
	}
	saved, gone := l.proofs(matched, l.reloadEnds(panes, joining, paneResponses))
	for _, entry := range append([]*liveTextEntry(nil), l.entries...) {
		if gone[entry.ID] {
			l.drop(entry)
		}
	}
	for i, pane := range panes {
		read := pages[page[i]]
		if pane.reads != nil {
			pane.reads.Add(1)
		}
		switch {
		case pane.warm && !settledProjection(read.Projection.State):
			// A warm pane sends nothing until the projection settles, so a
			// rebuild reaches it as one fresh tail.
		case joining[pane]:
			l.tail(pane, read, saved)
		default:
			l.updatePane(pane, read, saved)
		}
	}
}

// livePage is one page read and its rows' digests, computed once per read and
// shared by every pane that asked for that page.
type livePage struct {
	transcriptResponse
	digests [][sha256.Size]byte
}

func newLivePage(response transcriptResponse) livePage {
	page := livePage{transcriptResponse: response, digests: make([][sha256.Size]byte, len(response.Items))}
	for i, item := range response.Items {
		raw, _ := json.Marshal(item)
		page.digests[i] = sha256.Sum256(raw)
	}
	return page
}

// pageQueryKey names one page query: panes with the same key read the same
// page.
func pageQueryKey(query transcriptListQuery) string {
	return fmt.Sprintf("%s\x00%s\x00%t\x00%d", query.View, strings.Join(query.HiddenCategories, ","), query.HiddenSpecified, query.Limit)
}

func settledProjection(state string) bool {
	return state == string(history.TranscriptReady) || state == string(history.TranscriptEmpty) || state == string(history.TranscriptRetained)
}

// shownOnScreen reports a pane on screen: one that shows live text.
func (l *liveTextLoop) shownOnScreen() bool {
	for pane := range l.panes {
		if !pane.rowsOnly {
			return true
		}
	}
	return false
}

// armWarm starts, or restarts, the wait before the warm panes' read. Warm
// panes riding a pane on screen's reads need none.
func (l *liveTextLoop) armWarm() {
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

// tail sends a pane its newest page and the live text, as one message the
// browser swaps in one render.
func (l *liveTextLoop) tail(pane *liveTextPane, response livePage, saved map[string]bool) {
	// Named before the pane's record of shown text is replaced below. A tail
	// also names every prompt whose saved row is in its page: a browser that
	// was away while a send of its own was saved gets the row and that word in
	// one message, so it never shows both, nor neither.
	shownSaved := pane.shown(saved)
	inPage, err := l.promptsSavedIn(response.Items)
	if err != nil {
		log.Printf("live text saved prompts %s: %v", l.hostedID(), err)
		pane.fail("the saved transcript could not be read")
		delete(l.panes, pane)
		return
	}
	for _, id := range inPage {
		if !slices.Contains(shownSaved, id) {
			shownSaved = append(shownSaved, id)
		}
	}
	sort.Strings(shownSaved)
	live := l.view(pane)
	if pane.warm {
		response = pane.window(response)
	}
	marker := pane.marker
	pane.marker = nil
	pane.key = response.Projection.Key
	pane.rows = make(map[string]liveRowState, len(response.Items))
	pane.maxSeq = 0
	for i, item := range response.Items {
		pane.rows[item.ID] = liveRowState{seq: item.ProjectedSeq, digest: response.digests[i]}
		pane.maxSeq = max(pane.maxSeq, item.ProjectedSeq)
	}
	pane.sent = sentFor(live)
	page, projection := response.Page, response.Projection
	if marker != nil && marker.key == pane.key {
		// The browser holds this projection's rows first..last and says what
		// they are (a digest of their ids, sequences, lengths and truncation).
		// When the window's rows in that range digest the same, it gets only
		// the window's rows outside the range, as an update, and trims to the
		// window; a row changed or removed in place while it was away makes
		// the digests differ, and it gets a tail.
		var held, rows []transcriptItem
		for _, item := range response.Items {
			if item.ProjectedSeq < marker.first || item.ProjectedSeq > marker.last {
				rows = append(rows, item)
			} else {
				held = append(held, item)
			}
		}
		if warmHeldDigest(held) == marker.digest {
			pane.send(liveTextMessage{Type: "update", ThreadID: response.ThreadID, Key: pane.key, Rows: nonNilItems(rows),
				Turns: response.Turns, Page: &page, Projection: &projection})
			return
		}
	}
	message := liveTextMessage{Type: "tail", ThreadID: response.ThreadID, Key: pane.key, Rows: nonNilItems(response.Items),
		Turns: response.Turns, Page: &page, Projection: &projection}
	if !pane.rowsOnly {
		message.Live, message.Saved = &live, shownSaved
	}
	pane.send(message)
}

// window is the page a warm pane sends: the newest rows that fill the
// browser's window (warm_fill.go) and the turns they are in. The page is
// shared with other panes, so nothing is changed in place.
func (p *liveTextPane) window(response livePage) livePage {
	keep := p.fill.count(response.Items)
	if keep >= len(response.Items) {
		return response
	}
	cut := len(response.Items) - keep
	response.Items, response.digests = response.Items[cut:], response.digests[cut:]
	response.Page.StartSeq, response.Page.HasMoreBefore = response.Items[0].ProjectedSeq, true
	inTurns := make(map[string]bool, keep)
	for _, item := range response.Items {
		inTurns[item.TurnID] = true
	}
	turns := make([]transcriptTurn, 0, len(response.Turns))
	for _, turn := range response.Turns {
		if inTurns[turn.ID] {
			turns = append(turns, turn)
		}
	}
	response.Turns = turns
	return response
}

// view is the live text a pane shows: all of it, or none on an agent stream.
func (l *liveTextLoop) view(pane *liveTextPane) []liveTextEntry {
	if pane.rowsOnly {
		return nil
	}
	return l.visible()
}

// needsTail reports that a read replaces the pane rather than patching it: a
// rebuild, or more new rows than one page holds.
func (p *liveTextPane) needsTail(response transcriptResponse) bool {
	gap := len(response.Items) > 0 && response.Page.HasMoreBefore && p.maxSeq > 0 &&
		response.Items[0].ProjectedSeq > p.maxSeq
	return response.Projection.Key != p.key || gap
}

// promptsSavedIn names, as live-text ids, the prompts whose saved rows are
// among these page rows: the store lists the candidates (TailCandidates, read
// after the page, so every prompt whose row the page holds is among them) and
// one catalog read matches their exact keys to rows. Only a row id in the page
// names a prompt, so a row saved after the page was read names nothing. A
// Codex prompt with only a turn key is left out: its turn may hold several
// rows, and the browser's record read covers it.
func (l *liveTextLoop) promptsSavedIn(rows []transcriptItem) ([]string, error) {
	if l.sess == nil || l.sess.PromptStore() == nil {
		return nil, nil
	}
	var oldest time.Time
	page := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.Type != "user_message" {
			continue
		}
		page[row.ID] = true
		if row.CreatedAt == nil {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, *row.CreatedAt); err == nil && (oldest.IsZero() || at.Before(oldest)) {
			oldest = at
		}
	}
	if len(page) == 0 || oldest.IsZero() {
		return nil, nil
	}
	candidates, err := l.sess.PromptStore().TailCandidates(context.Background(), l.hostedID(), oldest)
	if err != nil {
		return nil, err
	}
	actions := make(map[string][]string) // key -> action ids
	eventTurns := make(map[string]string)
	var keys []string
	for _, entry := range candidates {
		key := l.promptKey(entry)
		if key == "" || l.sess.Provider() == "codex" && (entry.Binding == nil || entry.Binding.EventID == "") {
			continue
		}
		if l.sess.Provider() == "codex" && entry.Binding.Scope.NativeTurnID != nil {
			eventTurns[key] = *entry.Binding.Scope.NativeTurnID
		} else if len(actions[key]) == 0 {
			keys = append(keys, key)
		}
		actions[key] = append(actions[key], entry.ActionID)
	}
	var match []history.TranscriptMatch
	if len(keys) > 0 {
		slices.Sort(keys)
		match = append(match, l.promptMatch(keys))
	}
	if len(eventTurns) > 0 {
		events := make([]string, 0, len(eventTurns))
		for key := range eventTurns {
			events = append(events, key)
		}
		slices.Sort(events)
		match = append(match, history.TranscriptMatch{Namespace: history.MatchTurnEvent, ItemType: "user_message", Keys: events, Turns: eventTurns})
	}
	if len(match) == 0 {
		return nil, nil
	}
	threadID, err := l.catalogThreadID()
	if err != nil {
		return nil, err
	}
	_, matched, err := l.read(threadID, nil, match)
	if err != nil {
		return nil, err
	}
	var ids []string
	for key, itemID := range matched {
		if page[itemID] {
			for _, action := range actions[key] {
				ids = append(ids, "prompt:"+action)
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// shown names the live text this pane was showing that a read found saved,
// and every prompt a read found saved: the browser shows its own send until
// the stream lists it or names it saved, even one Nano never listed live.
func (p *liveTextPane) shown(saved map[string]bool) []string {
	var ids []string
	for id := range saved {
		if _, sent := p.sent.texts[id]; sent || strings.HasPrefix(id, "prompt:") {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (l *liveTextLoop) updatePane(pane *liveTextPane, response livePage, saved map[string]bool) {
	if pane.isClosed() {
		delete(l.panes, pane)
		return
	}
	if pane.warm {
		response = pane.window(response)
	}
	if pane.needsTail(response.transcriptResponse) {
		l.tail(pane, response, saved)
		return
	}
	live := l.view(pane)
	var rows []transcriptItem
	start := int64(0)
	if len(response.Items) > 0 {
		start = response.Items[0].ProjectedSeq
	}
	present := make(map[string]bool, len(response.Items))
	for i, item := range response.Items {
		present[item.ID] = true
		digest := response.digests[i]
		if state, ok := pane.rows[item.ID]; !ok || state.digest != digest {
			rows = append(rows, item)
		}
		pane.rows[item.ID] = liveRowState{seq: item.ProjectedSeq, digest: digest}
		pane.maxSeq = max(pane.maxSeq, item.ProjectedSeq)
	}
	var removed []string
	for id, state := range pane.rows {
		if present[id] {
			continue
		}
		if state.seq >= start {
			removed = append(removed, id) // it left this pane's profile
		}
		delete(pane.rows, id)
	}
	sort.Strings(removed)
	liveChanged := !pane.rowsOnly && !pane.sent.same(live, true)
	var shownSaved []string
	if !pane.rowsOnly {
		shownSaved = pane.shown(saved)
	}
	if len(rows) == 0 && len(removed) == 0 && !liveChanged && len(shownSaved) == 0 {
		return
	}
	page := response.Page
	message := liveTextMessage{Type: "update", Key: pane.key, Rows: rows, Removed: removed, Turns: response.Turns, Page: &page}
	if liveChanged || len(shownSaved) > 0 {
		message.Live, message.Saved = &live, shownSaved
		pane.sent = sentFor(live)
	}
	pane.send(message)
}

// sendLiveChanges streams text growth as appends, and any other live-text
// change as the whole region.
func (l *liveTextLoop) sendLiveChanges() {
	for pane := range l.panes {
		if pane.isClosed() {
			delete(l.panes, pane)
			continue
		}
		if pane.rowsOnly {
			continue
		}
		live := l.view(pane)
		if !pane.sent.same(live, false) {
			pane.send(liveTextMessage{Type: "live", Live: &live})
			pane.sent = sentFor(live)
			continue
		}
		for _, entry := range live {
			before := pane.sent.texts[entry.ID]
			if entry.Text == before {
				continue
			}
			if !strings.HasPrefix(entry.Text, before) {
				pane.send(liveTextMessage{Type: "live", Live: &live})
				pane.sent = sentFor(live)
				break
			}
			pane.send(liveTextMessage{Type: "append", ID: entry.ID, Append: entry.Text[len(before):]})
			pane.sent.texts[entry.ID] = entry.Text
		}
	}
}

func (l *liveTextLoop) hostedID() string {
	if l.sess == nil {
		return ""
	}
	return l.sess.HostedThreadID()
}

func sentFor(live []liveTextEntry) liveSent {
	sent := liveSent{texts: make(map[string]string, len(live))}
	var shape strings.Builder
	for _, entry := range live {
		sent.texts[entry.ID] = entry.Text
		shape.WriteString(entry.ID + "\x00" + entry.State + "\x00" + entry.Label + "\x00")
	}
	sent.shape = shape.String()
	return sent
}

// same reports the same entries, order, states and labels; withText also compares
// every entry's text.
func (s liveSent) same(live []liveTextEntry, withText bool) bool {
	if s.texts == nil || sentFor(live).shape != s.shape {
		return false
	}
	if withText {
		for _, entry := range live {
			if s.texts[entry.ID] != entry.Text {
				return false
			}
		}
	}
	return true
}

func nonNilItems(items []transcriptItem) []transcriptItem {
	if items == nil {
		return []transcriptItem{}
	}
	return items
}

// handleHostedThreadLive streams one pane of a file-only thread: its status
// and pending questions, its rows and its live text, in one ordered event
// stream. Each pane opens its own.
func (s *server) handleHostedThreadLive(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	sess := s.hostedSessionForRequest(record.ID)
	if sess == nil {
		httpapi.WriteHTTPError(w, "no live session is attached to this thread", http.StatusConflict)
		return
	}
	sess.handleLiveStream(w, r)
}

func savedConversationItem(row transcriptItem) liveProjectionItem {
	itemType := row.Type
	switch row.Type {
	case "user_message":
		itemType = "userMessage"
	case "agent_message":
		itemType = "agentMessage"
	}
	createdAt := ""
	if row.CreatedAt != nil {
		createdAt = *row.CreatedAt
	}
	return liveProjectionItem{ID: row.ID, DurableItemID: row.ID, NativeItemID: row.NativeItemID, ProjectedSeq: row.ProjectedSeq,
		TurnID: row.TurnID, Type: itemType, Category: row.Category, Role: row.Role, Phase: row.Phase, Text: row.Text,
		Status: threadvalues.FirstNonEmpty(row.Status, "completed"), Done: true, CreatedAt: createdAt, ToolName: row.ToolName, CallID: row.CallID,
		ContentRef: row.ContentRef, ContentTruncated: row.ContentTruncated, OriginalBytes: int(row.OriginalBytes)}
}

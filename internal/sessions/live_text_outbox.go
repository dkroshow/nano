package sessions

import (
	"context"
	"log"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
)

// seedOutbox starts a new loop from a read-only snapshot of the outbox, so a
// queued or parked prompt shows on reopening. Delivery state is never changed.
// No pane exists yet; each pane joins through a tick that checks them first.
func (l *LiveTextLoop) seedOutbox() {
	if l.sess == nil || l.sess.delivery.PromptStore() == nil {
		return
	}
	records, err := l.sess.delivery.PromptStore().List(context.Background(), l.sess.identity.hostedThreadID)
	if err != nil {
		log.Printf("live text outbox snapshot %s: %v", l.sess.identity.hostedThreadID, err)
		return
	}
	for _, record := range records {
		l.applyOutbox(record.Outbox, record.DeliveryRank)
	}
}

// applyOutbox applies one outbox change and reports whether a read must check
// it: a new entry, or a newly learned key. rank is the prompt's delivery rank
// when known, else -1.
func (l *LiveTextLoop) applyOutbox(entry prompts.OutboxEntry, rank int) bool {
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
			turn, found, err := l.sess.delivery.PersistedOperation(context.Background(), entry.ActionID)
			if err != nil {
				log.Printf("live text submission metadata %s: %v", l.hostedID(), err)
			} else if found && turn.Origin == string(prompts.AgentOrigin) && turn.SenderSubmittedAt != "" {
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
func (l *LiveTextLoop) presentPrompt(current *liveTextEntry, entry prompts.OutboxEntry) {
	provider := ""
	if l.sess != nil {
		provider = l.sess.identity.provider
	}
	newClaim := entry.UpdatedAt != current.outbox.UpdatedAt || current.outbox.State != prompts.Delivering
	current.outbox = entry
	current.State = string(entry.State)
	current.AllowedActions, current.Label = PromptPresentation(provider, entry)
	if slow, ok := SlowDeliveryAt(entry); ok && newClaim && current.Label == "" {
		id := current.ID
		time.AfterFunc(time.Until(slow), func() {
			l.post(func(in *liveTextInbox) { in.events = append(in.events, liveTextEvent{slow: id}) })
		})
	}
}

// learnKey gives a prompt its exact key once the outbox knows it, and reports
// that a read must check it: a Codex or Grok prompt learns its turn on delivery, and
// its saved row may already be in the file.
func (l *LiveTextLoop) learnKey(current *liveTextEntry, entry prompts.OutboxEntry) bool {
	learned := false
	if l.sess != nil && (l.sess.identity.provider == "grok" || l.sess.identity.provider == "codex") && entry.Binding != nil && entry.Binding.Scope.NativeTurnID != nil && current.turn != *entry.Binding.Scope.NativeTurnID {
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
func (l *LiveTextLoop) deliveryRank(actionID string) int {
	if l.sess == nil || l.sess.delivery.PromptStore() == nil {
		return 0
	}
	record, _, err := l.sess.delivery.PromptStore().Get(context.Background(), actionID)
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
func (l *LiveTextLoop) promptKey(entry prompts.OutboxEntry) string {
	if l.sess != nil && l.sess.identity.provider == "codex" {
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
	if l.sess != nil && (l.sess.identity.provider == "claude" || l.sess.identity.provider == "grok") {
		return SubmissionEventID(l.sess.identity.hostedThreadID, entry.ActionID)
	}
	return ""
}

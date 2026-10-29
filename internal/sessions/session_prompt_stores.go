package sessions

import (
	"context"
	"database/sql"

	"github.com/dkroshow/nano/internal/core/prompts"
)

// Queue consumes the hosted owner's compatibility view of durable prompts.
type Queue interface {
	// Get reads compatibility evidence for an action.
	Get(string) (prompts.QueuedTurn, bool)
	// First reads the next queued prompt for an attachment.
	First(string, string, string) (prompts.QueuedTurn, bool)
	// ByProviderTurn reads a prompt bound to the named provider turn.
	ByProviderTurn(string, string, string, string) (prompts.QueuedTurn, bool)
	// ListThread reads compatibility records for a thread.
	ListThread(string, string, string) []prompts.QueuedTurn
	// ListThreadFrom reads compatibility records through the supplied journal.
	ListThreadFrom(prompts.JournalReader, string, string, string) []prompts.QueuedTurn
	// BootParksFor reads prompts parked during boot for the attachment.
	BootParksFor(string, string, string) []prompts.BootPark
	// QueuedCount counts queued prompts for the attachment.
	QueuedCount(string, string, string) int
	// Position reads an action’s one-based delivery position.
	Position(string) int
	// TransactionStore returns the canonical transactional prompt store.
	TransactionStore() *prompts.Store
	// BindTransactionStore sets the canonical transactional prompt store.
	BindTransactionStore(*prompts.Store)
	// Database returns the prompt view’s database connection.
	Database() *sql.DB
}

// Journal reads delivery evidence and its restart identity.
type Journal interface {
	// Get reads compatibility evidence for an action.
	Get(string) (prompts.SendAttempt, bool)
	// ListThread reads compatibility records for a thread.
	ListThread(string, string) []prompts.SendAttempt
	// TransactionStore returns the canonical transactional prompt store.
	TransactionStore() *prompts.Store
	// BindTransactionStore sets the canonical transactional prompt store.
	BindTransactionStore(*prompts.Store)
	// RunIdentity returns the process identity attached to delivery evidence.
	RunIdentity() string
}

// QueuedPrompt reads one queued prompt from the attachment store.
func (sess *Delivery) QueuedPrompt(actionID string) (prompts.QueuedTurn, bool) {
	if sess.queuedTurns == nil {
		return prompts.QueuedTurn{}, false
	}
	return sess.queuedTurns.Get(actionID)
}

func (sess *Delivery) sendAttempt(actionID string) (prompts.SendAttempt, bool) {
	if sess.sendAttempts == nil {
		return prompts.SendAttempt{}, false
	}
	return sess.sendAttempts.Get(actionID)
}

func (sess *Delivery) queuedPrompts(nativeID string) []prompts.QueuedTurn {
	if sess.queuedTurns == nil {
		return nil
	}
	return sess.queuedTurns.ListThread(sess.identity.provider, nativeID, sess.identity.hostedThreadID)
}

// HasDeliveryEvidence reads this attachment's journal and queue, using the
// application's stores only when that source is absent on the attachment.
func (sess *Delivery) HasDeliveryEvidence(actionID string, queue Queue, journal Journal) bool {
	if sess != nil {
		if sess.queuedTurns != nil {
			queue = sess.queuedTurns
		}
		if sess.sendAttempts != nil {
			journal = sess.sendAttempts
		}
	}
	if queue != nil {
		if _, found := queue.Get(actionID); found {
			return true
		}
	}
	if journal != nil {
		if _, found := journal.Get(actionID); found {
			return true
		}
	}
	return false
}

// QueuePositionOr reads this attachment's queue or the application fallback.
func (sess *Delivery) QueuePositionOr(actionID string, fallback Queue) int {
	if sess != nil && sess.queuedTurns != nil {
		fallback = sess.queuedTurns
	}
	return fallback.Position(actionID)
}

// DeliveryWasAttempted reads acceptance/sending evidence from the attachment's journal.
func (sess *Delivery) DeliveryWasAttempted(actionID string, fallback Journal) bool {
	if sess != nil && sess.sendAttempts != nil {
		fallback = sess.sendAttempts
	}
	attempt, ok := fallback.Get(actionID)
	return ok && (attempt.Status == sendAttemptSending || attempt.Status == sendAttemptAccepted)
}

// initializePromptStore runs once during Runtime coordinator construction.
func (sess *Delivery) initializePromptStore(canonicalDB *sql.DB) error {
	if sess.turns == nil && canonicalDB != nil {
		promptStore, err := prompts.OpenStore(context.Background(), canonicalDB)
		if err != nil {
			return err
		}
		if _, err := promptStore.AdoptLegacy(context.Background()); err != nil {
			return err
		}
		sess.turns = promptStore
		if sess.queuedTurns != nil {
			sess.queuedTurns.BindTransactionStore(promptStore)
		}
		if sess.sendAttempts != nil {
			sess.sendAttempts.BindTransactionStore(promptStore)
		}
	}
	return nil
}

func (sess *Delivery) queuedPromptStore() Queue         { return sess.queuedTurns }
func (sess *Delivery) sendJournal() Journal             { return sess.sendAttempts }
func (sess *Delivery) expiryCounter() *DeliveryExpiries { return sess.deliveryExpiries }
func (sess *Delivery) memberWorkPending() bool          { return sess.owedHint.Load() }

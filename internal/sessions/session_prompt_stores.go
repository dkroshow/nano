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
func (delivery *Delivery) QueuedPrompt(actionID string) (prompts.QueuedTurn, bool) {
	if delivery.queuedTurns == nil {
		return prompts.QueuedTurn{}, false
	}
	return delivery.queuedTurns.Get(actionID)
}

func (delivery *Delivery) sendAttempt(actionID string) (prompts.SendAttempt, bool) {
	if delivery.sendAttempts == nil {
		return prompts.SendAttempt{}, false
	}
	return delivery.sendAttempts.Get(actionID)
}

func (delivery *Delivery) queuedPrompts(nativeID string) []prompts.QueuedTurn {
	if delivery.queuedTurns == nil {
		return nil
	}
	return delivery.queuedTurns.ListThread(delivery.identity.provider, nativeID, delivery.identity.hostedThreadID)
}

// HasDeliveryEvidence reads this attachment's journal and queue, using the
// application's stores only when that source is absent on the attachment.
func (delivery *Delivery) HasDeliveryEvidence(actionID string, queue Queue, journal Journal) bool {
	if delivery != nil {
		if delivery.queuedTurns != nil {
			queue = delivery.queuedTurns
		}
		if delivery.sendAttempts != nil {
			journal = delivery.sendAttempts
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
func (delivery *Delivery) QueuePositionOr(actionID string, fallback Queue) int {
	if delivery != nil && delivery.queuedTurns != nil {
		fallback = delivery.queuedTurns
	}
	return fallback.Position(actionID)
}

// DeliveryWasAttempted reads acceptance/sending evidence from the attachment's journal.
func (delivery *Delivery) DeliveryWasAttempted(actionID string, fallback Journal) bool {
	if delivery != nil && delivery.sendAttempts != nil {
		fallback = delivery.sendAttempts
	}
	attempt, ok := fallback.Get(actionID)
	return ok && (attempt.Status == sendAttemptSending || attempt.Status == sendAttemptAccepted)
}

// initializePromptStore runs once during Runtime coordinator construction.
func (delivery *Delivery) initializePromptStore(canonicalDB *sql.DB) error {
	if delivery.turns == nil && canonicalDB != nil {
		promptStore, err := prompts.OpenStore(context.Background(), canonicalDB)
		if err != nil {
			return err
		}
		if _, err := promptStore.AdoptLegacy(context.Background()); err != nil {
			return err
		}
		delivery.turns = promptStore
		if delivery.queuedTurns != nil {
			delivery.queuedTurns.BindTransactionStore(promptStore)
		}
		if delivery.sendAttempts != nil {
			delivery.sendAttempts.BindTransactionStore(promptStore)
		}
	}
	return nil
}

func (delivery *Delivery) queuedPromptStore() Queue         { return delivery.queuedTurns }
func (delivery *Delivery) sendJournal() Journal             { return delivery.sendAttempts }
func (delivery *Delivery) expiryCounter() *DeliveryExpiries { return delivery.deliveryExpiries }
func (delivery *Delivery) memberWorkPending() bool          { return delivery.owedHint.Load() }

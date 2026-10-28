package sessions

import (
	"database/sql"

	"github.com/dkroshow/nano/internal/core/prompts"
)

// Queue consumes the hosted owner's compatibility view of durable prompts.
type Queue interface {
	// Get reads compatibility evidence for an action.
	Get(string) (queuedTurnRecord, bool)
	// First reads the next queued prompt for an attachment.
	First(string, string, string) (queuedTurnRecord, bool)
	// ByProviderTurn reads a prompt bound to the named provider turn.
	ByProviderTurn(string, string, string, string) (queuedTurnRecord, bool)
	// ListThread reads compatibility records for a thread.
	ListThread(string, string, string) []queuedTurnRecord
	// ListThreadFrom reads compatibility records through the supplied journal.
	ListThreadFrom(prompts.JournalReader, string, string, string) []queuedTurnRecord
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
	Get(string) (sendAttemptRecord, bool)
	// ListThread reads compatibility records for a thread.
	ListThread(string, string) []sendAttemptRecord
	// TransactionStore returns the canonical transactional prompt store.
	TransactionStore() *prompts.Store
	// BindTransactionStore sets the canonical transactional prompt store.
	BindTransactionStore(*prompts.Store)
	// RunIdentity returns the process identity attached to delivery evidence.
	RunIdentity() string
}

// QueuedPrompt reads one queued prompt from the attachment store.
func (sess *Session) QueuedPrompt(actionID string) (queuedTurnRecord, bool) {
	if sess.queuedTurns == nil {
		return queuedTurnRecord{}, false
	}
	return sess.queuedTurns.Get(actionID)
}

func (sess *Session) sendAttempt(actionID string) (sendAttemptRecord, bool) {
	if sess.sendAttempts == nil {
		return sendAttemptRecord{}, false
	}
	return sess.sendAttempts.Get(actionID)
}

func (sess *Session) queuedPrompts(nativeID string) []queuedTurnRecord {
	if sess.queuedTurns == nil {
		return nil
	}
	return sess.queuedTurns.ListThread(sess.provider, nativeID, sess.hostedThreadID)
}

// HasDeliveryEvidence reads this attachment's journal and queue, using the
// application's stores only when that source is absent on the attachment.
func (sess *Session) HasDeliveryEvidence(actionID string, queue Queue, journal Journal) bool {
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
func (sess *Session) QueuePositionOr(actionID string, fallback Queue) int {
	if sess != nil && sess.queuedTurns != nil {
		fallback = sess.queuedTurns
	}
	return fallback.Position(actionID)
}

// DeliveryWasAttempted reads acceptance/sending evidence from the attachment's journal.
func (sess *Session) DeliveryWasAttempted(actionID string, fallback Journal) bool {
	if sess != nil && sess.sendAttempts != nil {
		fallback = sess.sendAttempts
	}
	attempt, ok := fallback.Get(actionID)
	return ok && (attempt.Status == sendAttemptSending || attempt.Status == sendAttemptAccepted)
}

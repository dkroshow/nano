package app

import (
	"context"
	"database/sql"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// promptJournal is the shared read surface of a writer or read-only journal.
type promptJournal interface {
	List(context.Context, string) ([]prompts.Record, error)
	ListThreadAttempts(context.Context, model.Provider, string) ([]prompts.AttemptStatus, error)
}

// sessionQueue consumes the hosted owner's compatibility view of durable prompts.
type sessionQueue interface {
	// Get reads compatibility evidence for an action.
	Get(string) (queuedTurnRecord, bool)
	// First reads the next queued prompt for an attachment.
	First(string, string, string) (queuedTurnRecord, bool)
	// ByProviderTurn reads a prompt bound to the named provider turn.
	ByProviderTurn(string, string, string, string) (queuedTurnRecord, bool)
	// ListThread reads compatibility records for a thread.
	ListThread(string, string, string) []queuedTurnRecord
	// ListThreadFrom reads compatibility records through the supplied journal.
	ListThreadFrom(promptJournal, string, string, string) []queuedTurnRecord
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

// sessionJournal reads delivery evidence and its restart identity.
type sessionJournal interface {
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

func (sess *session) queuedPrompt(actionID string) (queuedTurnRecord, bool) {
	if sess.queuedTurns == nil {
		return queuedTurnRecord{}, false
	}
	return sess.queuedTurns.Get(actionID)
}

func (sess *session) sendAttempt(actionID string) (sendAttemptRecord, bool) {
	if sess.sendAttempts == nil {
		return sendAttemptRecord{}, false
	}
	return sess.sendAttempts.Get(actionID)
}

func (sess *session) queuedPrompts(nativeID string) []queuedTurnRecord {
	if sess.queuedTurns == nil {
		return nil
	}
	return sess.queuedTurns.ListThread(sess.provider, nativeID, sess.hostedThreadID)
}

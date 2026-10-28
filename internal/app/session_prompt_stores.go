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
	get(string) (queuedTurnRecord, bool)
	first(string, string, string) (queuedTurnRecord, bool)
	byProviderTurn(string, string, string, string) (queuedTurnRecord, bool)
	listThread(string, string, string) []queuedTurnRecord
	listThreadFrom(promptJournal, string, string, string) []queuedTurnRecord
	bootParksFor(string, string, string) []prompts.BootPark
	queuedCount(string, string, string) int
	position(string) int
	transactionStore() *prompts.Store
	bindTransactionStore(*prompts.Store)
	database() *sql.DB
}

// sessionJournal reads delivery evidence and its restart identity.
type sessionJournal interface {
	get(string) (sendAttemptRecord, bool)
	listThread(string, string) []sendAttemptRecord
	transactionStore() *prompts.Store
	bindTransactionStore(*prompts.Store)
	runIdentity() string
}

func (sess *session) queuedPrompt(actionID string) (queuedTurnRecord, bool) {
	if sess.queuedTurns == nil {
		return queuedTurnRecord{}, false
	}
	return sess.queuedTurns.get(actionID)
}

func (sess *session) sendAttempt(actionID string) (sendAttemptRecord, bool) {
	if sess.sendAttempts == nil {
		return sendAttemptRecord{}, false
	}
	return sess.sendAttempts.get(actionID)
}

func (sess *session) queuedPrompts(nativeID string) []queuedTurnRecord {
	if sess.queuedTurns == nil {
		return nil
	}
	return sess.queuedTurns.listThread(sess.provider, nativeID, sess.hostedThreadID)
}

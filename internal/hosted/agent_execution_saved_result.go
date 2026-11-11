package hosted

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcript"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// SavedOperationResultFinished wakes the owning live or detached settlement loop.
func (s *AttachmentCoordinator) SavedOperationResultFinished(hostedID string) {
	if sess := s.Registry.SessionForHostedThread(hostedID); sess != nil {
		if sess.Provider() == "claude" {
			sess.Delivery().RequestMemberStep()
		}
		return
	}
	s.runDetachedClaudeOwed(hostedID)
}

// SavedOperationResult is an operation's answer and final item id from the
// saved transcript: the final row of the completed saved turn that holds the
// operation's own prompt, all at one catalog generation.
func (s *AttachmentCoordinator) SavedOperationResult(ctx context.Context, rec threads.TurnRecord) (string, string, error) {
	// The prompt's binding, not its delivery state, proves membership below:
	// settlement can lag the file, or expire a prompt whose answer it holds.
	prompt, _, err := s.Turns.Get(ctx, rec.TransportActionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	if err != nil || prompt.Outbox.HostedThreadID != rec.HostedThreadID || string(prompt.Provider) != rec.Provider {
		return "", "", history.ErrSavedTranscriptUnavailable
	}
	entry := prompt.Outbox
	claude := rec.Provider == string(model.ProviderClaude)
	if claude {
		entry = sessions.ClaudeSettlementEntry(entry, prompt.NativeThreadID, rec.NativeThreadID, nil)
		if native, ok := sessions.ExactClaudeNative(entry); !ok || native != rec.NativeThreadID {
			return "", "", history.ErrSavedTranscriptUnavailable
		}
		answer, err := s.ClaudeAnswers(rec.HostedThreadID).Answer(ctx, entry)
		if err != nil {
			return "", "", err
		}
		if answer.Outcome != threads.AgentTurnCompleted {
			return "", "", history.ErrSavedTranscriptUnavailable // an interrupted turn has no answer
		}
		return answer.Text, answer.FinalID, nil
	}
	// The prompt's binding must name the operation's thread and, except for
	// Claude, whose execution turn is never saved, its provider turn.
	binding := entry.Binding
	if rec.NativeThreadID == "" || binding == nil || binding.Scope.NativeThreadID == nil || *binding.Scope.NativeThreadID != rec.NativeThreadID ||
		!claude && (binding.Scope.NativeTurnID == nil || *binding.Scope.NativeTurnID != rec.ProviderTurnID) {
		return "", "", history.ErrSavedTranscriptUnavailable
	}
	// A binding without its own event matches any user row of its turn, so the
	// rows other prompts in that turn claim by their events are not its own.
	// The store keeps one such binding per turn; settlement's rule weighs them.
	stored := []prompts.OutboxEntry{entry}
	if binding.EventID == "" && binding.Scope.NativeTurnID != nil {
		events, err := s.Turns.TurnEvents(ctx, rec.HostedThreadID, prompt.Provider, rec.NativeThreadID, *binding.Scope.NativeTurnID)
		if err != nil {
			return "", "", err
		}
		for _, event := range events {
			stored = append(stored, prompts.OutboxEntry{ActionID: "event:" + event, Binding: &prompts.Binding{Scope: binding.Scope, EventID: event}})
		}
	}
	var keys history.SavedUserKeys
	sessions.AddSavedUserKey(&keys, binding, rec.Provider)
	shared := rec.NativeThreadID + "\x00" + strings.Join(keys.NativeTurnIDs, "\x00") + "\x00\x00" + strings.Join(keys.NativeEventIDs, "\x00")
	rows, err := s.savedResultRows.read(ctx, shared, func(ctx context.Context) ([]history.SavedUserRow, error) {
		return s.Catalog.SavedPromptRows(ctx, rec.NativeThreadID, keys)
	})
	if err != nil {
		return "", "", err
	}
	generation, ok := sessions.SavedRowsGeneration(rows)
	if !ok && len(rows) > 0 {
		return "", "", history.ErrSavedGenerationChanged // rows from two generations: a rebuild is in flight
	}
	settlement, turn := transcript.SavedSettlementTurn(entry, stored, sessions.SavedRowsOf(rows))
	if !ok || settlement != transcript.SettlementSettled {
		return "", "", history.ErrSavedTranscriptUnavailable
	}
	for _, row := range rows {
		if row.Item.TurnKey == turn && row.TurnStatus != history.TurnCompleted {
			return "", "", history.ErrSavedTranscriptUnavailable // an interrupted turn has no answer
		}
	}
	text, itemID, err := s.Catalog.SavedTurnFinal(ctx, rec.NativeThreadID, generation, turn)
	if err != nil {
		return "", "", err
	}
	if itemID == "" || rec.FinalItemID != "" && itemID != rec.FinalItemID {
		return "", "", history.ErrSavedTranscriptUnavailable
	}
	return text, itemID, nil
}

// sharedSavedRows lets operations that need the same saved rows share one
// read. Every prompt steered into one Codex turn reads all of that turn's
// user rows, so a read for each cost prompts × rows and stalled the server.
// Reads of one key run one at a time, and a caller takes only the result of a
// read that began after it asked: never rows older than the commit that woke
// it. Nothing is kept once the last caller has its answer.
type sharedSavedRows struct {
	mu   sync.Mutex
	keys map[string]*sharedSavedRead
}

type sharedSavedRead struct {
	turn              chan struct{} // held by the one caller reading
	callers           int
	started, finished int // reads begun; the read rows and err came from
	rows              []history.SavedUserRow
	err               error
}

func (s *sharedSavedRows) read(ctx context.Context, key string, read func(context.Context) ([]history.SavedUserRow, error)) ([]history.SavedUserRow, error) {
	s.mu.Lock()
	shared := s.keys[key]
	if shared == nil {
		if s.keys == nil {
			s.keys = make(map[string]*sharedSavedRead)
		}
		shared = &sharedSavedRead{turn: make(chan struct{}, 1)}
		s.keys[key] = shared
	}
	shared.callers++
	asked := shared.started
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if shared.callers--; shared.callers == 0 {
			delete(s.keys, key)
		}
		s.mu.Unlock()
	}()
	select {
	case shared.turn <- struct{}{}:
		defer func() { <-shared.turn }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.mu.Lock()
	if shared.finished > asked {
		defer s.mu.Unlock()
		return shared.rows, shared.err
	}
	shared.started++
	mine := shared.started
	s.mu.Unlock()
	rows, err := read(ctx)
	s.mu.Lock()
	shared.rows, shared.err, shared.finished = rows, err, mine
	s.mu.Unlock()
	return rows, err
}

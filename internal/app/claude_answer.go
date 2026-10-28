package app

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	sessionruntime "github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// claudeAnswers is the rule for a hosted thread, read with the server's own
// stores and saved readers: an operation's answer, attached or not, and a
// thread with no session.
func (s *server) claudeAnswers(hosted string) sessionruntime.ClaudeAnswers {
	return sessionruntime.NewClaudeAnswers(hosted, s.turns, s.agentExecutions, s.savedPromptRows, s.savedTurnFinal)
}

// claudeOwedRun keeps one detached owed pass per hosted thread: a request
// during a pass is kept and runs one more pass, never dropped.
type claudeOwedRun struct {
	mu    sync.Mutex
	dirty atomic.Bool
}

// finishDetachedClaudeOwed finishes, after a catalog commit of one native
// thread, the owed prompts of each hosted thread no session holds: an exited
// thread's prompts are finished by the same rule, with no provider. A thread
// with a session runs its own member step.
func (s *server) finishDetachedClaudeOwed(native string) {
	if s.turns == nil || s.agentExecutions == nil || s.completedTurns == nil || native == "" {
		return
	}
	hosts, err := s.turns.OwedHosts(context.Background(), model.ProviderClaude, native)
	if err != nil {
		log.Printf("Claude owed prompts in %s unreadable: %v", native, err)
		return
	}
	for _, hosted := range hosts {
		if s.SessionForHostedThread(hosted) == nil {
			s.runDetachedClaudeOwed(hosted)
		}
	}
}

// runDetachedClaudeOwed runs one hosted thread's owed pass, one at a time: a
// request during a pass runs one more pass after it.
func (s *server) runDetachedClaudeOwed(hosted string) {
	if s.turns == nil || s.agentExecutions == nil || s.completedTurns == nil {
		return
	}
	value, _ := s.claudeOwedRuns.LoadOrStore(hosted, &claudeOwedRun{})
	run := value.(*claudeOwedRun)
	run.dirty.Store(true)
	for run.mu.TryLock() {
		for run.dirty.Swap(false) {
			s.claudeOwedPass(hosted)
		}
		run.mu.Unlock()
		if !run.dirty.Load() {
			break
		}
	}
}

// claudeOwedPass finishes every owed Claude prompt of one hosted thread the
// saved transcript decides, reading provisional handles' recorded
// resolutions. Undecided prompts stay owed for the next commit.
func (s *server) claudeOwedPass(hosted string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := s.completedTurns.store.coreStore(ctx)
	if err != nil {
		log.Printf("Claude owed prompts of %s: turn store unavailable: %v", hosted, err)
		return
	}
	resolve := func(handle string) string {
		native, err := s.controlMetadata.ResolvedNative(ctx, hosted, string(model.ProviderClaude), handle)
		if err != nil {
			return ""
		}
		return native
	}
	answers := s.claudeAnswers(hosted)
	for from := uint64(0); ctx.Err() == nil; {
		batch, err := s.turns.Owed(ctx, hosted, from, sessionruntime.ClaudeMemberBatch)
		if err != nil {
			log.Printf("Claude owed prompts of %s unreadable: %v", hosted, err)
			return
		}
		for _, record := range batch {
			answers.FinishOwed(ctx, store, record, sessionruntime.ClaudeSettlementEntry(record.Outbox, record.NativeThreadID, "", resolve))
		}
		if len(batch) < sessionruntime.ClaudeMemberBatch {
			return
		}
		from = batch[len(batch)-1].Outbox.Sequence
	}
}

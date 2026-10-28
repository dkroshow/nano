package hosted

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The turn-operations changes feed: an in-memory, loss-tolerant ring of
// semantic operation transitions behind an epoch.seq bookmark, modeled on
// fleet_changes.go. Durable truth stays in the operation records — the ring is
// delivery, not history. A restart mints a fresh epoch, so stale bookmarks
// resnapshot instead of silently skipping; callers recover by re-reading the
// operations they retained.
//
// Only semantic status changes are emitted. Reattachment bulk-touches bump
// every non-terminal turn's revision without changing status (bindSession,
// updateSession); a revision-delta feed would spew noise on every restart.

const (
	agentChangeRingSize = 1024
	agentEpochBytes     = 8

	agentResnapshotNoBookmark = "no_bookmark"
	agentResnapshotUnknown    = "unknown_bookmark"
	agentResnapshotExpired    = "bookmark_expired"
)

// AgentOperationTransition describes one semantic status change in the transient operation feed.
type AgentOperationTransition struct {
	Seq                   uint64 `json:"seq"`
	OperationID           string `json:"operationId"`
	HostedThreadID        string `json:"hostedThreadId"`
	InvokerHostedThreadID string `json:"invokerHostedThreadId,omitempty"`
	From                  string `json:"from"`
	To                    string `json:"to"`
	At                    string `json:"at"`
}

type agentChangesResponse struct {
	Transitions []AgentOperationTransition `json:"transitions"`
	Bookmark    string                     `json:"bookmark"`
	Resnapshot  bool                       `json:"resnapshot,omitempty"`
	Reason      string                     `json:"reason,omitempty"`
}

type agentChangeLog struct {
	mu    sync.Mutex
	epoch string
	ring  []AgentOperationTransition
	next  uint64
}

func newAgentChangeLog() *agentChangeLog {
	epoch := make([]byte, agentEpochBytes)
	if _, err := rand.Read(epoch); err != nil {
		return &agentChangeLog{epoch: strconv.FormatInt(time.Now().UnixNano(), 36),
			ring: make([]AgentOperationTransition, agentChangeRingSize), next: 1}
	}
	return &agentChangeLog{epoch: hex.EncodeToString(epoch),
		ring: make([]AgentOperationTransition, agentChangeRingSize), next: 1}
}

func (l *agentChangeLog) append(transition AgentOperationTransition) {
	l.mu.Lock()
	defer l.mu.Unlock()
	transition.Seq = l.next
	l.ring[l.slot(l.next)] = transition
	l.next++
}

// since answers the bookmark pull, optionally filtered. The bookmark always
// advances to the head even when every transition is filtered out, so a
// filtered caller does not replay unrelated activity forever.
func (l *agentChangeLog) since(bookmark string, keep func(AgentOperationTransition) bool) agentChangesResponse {
	l.mu.Lock()
	defer l.mu.Unlock()
	head := l.next - 1
	response := agentChangesResponse{
		Transitions: []AgentOperationTransition{},
		Bookmark:    fmt.Sprintf("%s.%d", l.epoch, head),
	}
	bookmark = strings.TrimSpace(bookmark)
	if bookmark == "" {
		response.Resnapshot, response.Reason = true, agentResnapshotNoBookmark
		return response
	}
	seq, ok := l.parse(bookmark)
	if !ok || seq > head {
		response.Resnapshot, response.Reason = true, agentResnapshotUnknown
		return response
	}
	if seq+1 < l.oldest() {
		response.Resnapshot, response.Reason = true, agentResnapshotExpired
		return response
	}
	for cursor := seq + 1; cursor <= head; cursor++ {
		transition := l.ring[l.slot(cursor)]
		if keep == nil || keep(transition) {
			response.Transitions = append(response.Transitions, transition)
		}
	}
	return response
}

func (l *agentChangeLog) parse(bookmark string) (uint64, bool) {
	epoch, raw, found := strings.Cut(bookmark, ".")
	if !found || epoch != l.epoch {
		return 0, false
	}
	seq, err := strconv.ParseUint(raw, 10, 64)
	return seq, err == nil
}

func (l *agentChangeLog) oldest() uint64 {
	if l.next <= uint64(len(l.ring)) {
		return 1
	}
	return l.next - uint64(len(l.ring))
}

func (l *agentChangeLog) slot(seq uint64) int { return int((seq - 1) % uint64(len(l.ring))) }

// noteTurnTransitionLocked buffers a semantic transition. The buffer is
// flushed only after the normalized row transaction commits.
func (s *ExecutionStore) noteTurnTransitionLocked(oldStatus string, rec agentTurnRecord) {
	if s.changes == nil || oldStatus == rec.Status {
		return
	}
	invoker := ""
	if sessionRec, ok := s.sessions[rec.HostedThreadID]; ok {
		invoker = sessionRec.InvokerHostedThreadID
	}
	s.pendingTransitions = append(s.pendingTransitions, AgentOperationTransition{
		OperationID:           rec.ID,
		HostedThreadID:        rec.HostedThreadID,
		InvokerHostedThreadID: invoker,
		From:                  oldStatus,
		To:                    rec.Status,
		At:                    time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// waitChanges long-polls the ring: it returns as soon as the caller's cursor
// has filtered transitions or must resnapshot, else blocks on the store's
// broadcast channel until the deadline. A timeout returns an empty page with
// the same bookmark semantics; it never changes provider state.
func (s *ExecutionStore) waitChanges(ctx context.Context, bookmark string, wait time.Duration, keep func(AgentOperationTransition) bool) agentChangesResponse {
	deadline := time.NewTimer(wait)
	if wait <= 0 {
		deadline.Stop()
	}
	for {
		s.mu.RLock()
		ch := s.changed
		s.mu.RUnlock()
		response := s.changes.since(bookmark, keep)
		if len(response.Transitions) > 0 || response.Resnapshot || wait <= 0 {
			return response
		}
		select {
		case <-ctx.Done():
			return response
		case <-deadline.C:
			return response
		case <-ch:
			bookmark = response.Bookmark
		}
	}
}

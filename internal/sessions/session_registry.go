package sessions

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/observability"
	"github.com/dkroshow/nano/internal/threads"
)

// Registry owns the live-session map and its lock.
type Registry struct {
	mu       sync.Mutex
	sessions map[string]*Session
}

// SessionForThread returns the LIVE session hosting (provider,
// nativeThreadID), if any. Exited sessions are passed over: a resume that died
// pre-init (purged transcript) leaves a corpse still holding the thread id it
// seeded at spawn, and both callers mean "live" — the claude resume guard
// (session.go) must let the operator retry rather than 409 them into the dead
// Session, and RuntimeFor must read the thread as detached-or-durable, not
// attached. The guard in codexDaemon.claimThread/registerThread keeps codex
// threads single-attached and the claude resume guard does the same for
// claude, so at most one *live* session matches; exited corpses may coexist
// with their replacement until closed.
func (s *Registry) SessionForThread(provider, nativeThreadID string) *Session {
	if nativeThreadID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.identity.provider != provider || sess.state.ThreadID() != nativeThreadID {
			continue
		}
		exited := sess.state.exited()
		if exited {
			continue
		}
		return sess
	}
	return nil
}

// LiveSessionForResume finds a live session that already hosts the thread a
// resume names, by native id or, before Codex learns its native id, by
// hosted id. Exited sessions do not count: the operator may resume those.
func (s *Registry) LiveSessionForResume(provider, nativeThreadID, hostedThreadID string) *Session {
	if existing := s.SessionForThread(provider, nativeThreadID); existing != nil {
		return existing
	}
	existing := s.SessionForHostedThread(hostedThreadID)
	if existing == nil || hostedThreadID == "" {
		return nil
	}
	exited := existing.state.exited()
	if exited {
		return nil
	}
	return existing
}

// SessionForHostedThread returns the session a hosted thread's operations
// use. A failed attach can leave an exited session beside the one that
// replaced it, so map order must not choose: the live session wins
// (liveHostedThreadSession). Callers must not hold a session's stateMu.
func (s *Registry) SessionForHostedThread(id string) *Session {
	matches := s.hostedThreadSessions(id)
	switch len(matches) {
	case 0:
		return nil
	case 1:
		return matches[0]
	}
	if live := liveHostedThreadSession(matches); live != nil {
		return live
	}
	return matches[0]
}

// hostedThreadSessions lists the registered sessions of one hosted thread.
func (s *Registry) hostedThreadSessions(id string) []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	matches := make([]*Session, 0, 1)
	for _, sess := range s.sessions {
		if sess.identity.hostedThreadID == id {
			matches = append(matches, sess)
		}
	}
	return matches
}

// liveHostedThreadSession is the oldest of a hosted thread's sessions that has
// not exited, with the fleet summary's timestamp precision and ID tie-break,
// or nil when every session has exited.
func liveHostedThreadSession(matches []*Session) *Session {
	var oldest *Session
	var oldestCreated string
	for _, sess := range matches {
		exited := sess.state.exited()
		if exited {
			continue
		}
		created := sess.identity.createdAt.Format(time.RFC3339)
		if oldest == nil || created < oldestCreated || (created == oldestCreated && sess.identity.id < oldest.identity.id) {
			oldest, oldestCreated = sess, created
		}
	}
	return oldest
}

// ResolveLiveHostedThreadRecord resolves an attached identity before calling the hosted record lookup outside the registry lock.
func (s *Registry) ResolveLiveHostedThreadRecord(ref string, lookup func(string) (threads.SessionRecord, bool)) (threads.SessionRecord, bool) {
	s.mu.Lock()
	hostedThreadID := ""
	for _, sess := range s.sessions {
		if sess.identity.id == ref {
			hostedThreadID = sess.identity.hostedThreadID
			break
		}
	}
	if hostedThreadID == "" {
		for _, sess := range s.sessions {
			if sess.state.ThreadID() != ref {
				continue
			}
			if hostedThreadID != "" && hostedThreadID != sess.identity.hostedThreadID {
				s.mu.Unlock()
				return threads.SessionRecord{}, false // provider-native reference is ambiguous
			}
			hostedThreadID = sess.identity.hostedThreadID
		}
	}
	s.mu.Unlock()
	if hostedThreadID == "" {
		return threads.SessionRecord{}, false
	}
	return lookup(hostedThreadID)
}

// LookupSession reads one registered transport attachment.
func (s *Registry) LookupSession(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// SnapshotSessions copies registry membership before callers inspect sessions.
func (s *Registry) SnapshotSessions() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	return sessions
}

// RegisterSession publishes an attachment under its immutable session ID.
func (s *Registry) RegisterSession(sess *Session) {
	s.mu.Lock()
	s.sessions[sess.identity.id] = sess
	s.mu.Unlock()
}

// RemoveSession removes the attachment registered under an ID.
func (s *Registry) RemoveSession(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// RemoveCurrentSession leaves any replacement attachment registered.
func (s *Registry) RemoveCurrentSession(sess *Session) {
	s.mu.Lock()
	if s.sessions[sess.identity.id] == sess {
		delete(s.sessions, sess.identity.id)
	}
	s.mu.Unlock()
}

// VolatileSummaries snapshots live state without reading canonical stores.
func (s *Registry) VolatileSummaries(needsAttention map[string]bool) []Summary {
	sessions := s.SnapshotSessions()
	summaries := make([]Summary, 0, len(sessions))
	for _, sess := range sessions {
		summary, live := sess.state.volatileSummary(needsAttention[sess.identity.hostedThreadID])
		if live {
			summaries = append(summaries, summary)
		}
	}
	sort.Slice(summaries, func(i, j int) bool {
		// volatileSummary formats CreatedAt as RFC3339, including the zero time.
		a, _ := time.Parse(time.RFC3339, summaries[i].CreatedAt)
		b, _ := time.Parse(time.RFC3339, summaries[j].CreatedAt)
		if !a.Equal(b) {
			return a.Before(b)
		}
		return summaries[i].ID < summaries[j].ID
	})
	return summaries
}

// AttachmentSession includes exited attachments for explicit-close routing.
func (s *Registry) AttachmentSession(provider, nativeID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.identity.provider == provider && sess.state.ThreadID() == nativeID {
			return sess
		}
	}
	return nil
}

// ForgetDetachedClaude keeps registration excluded until durable forgetting ends.
func (s *Registry) ForgetDetachedClaude(nativeID string, forget func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.identity.provider == "claude" && sess.state.ThreadID() == nativeID && sess.view.Summary().Status != StatusExited {
			return fmt.Errorf("thread is attached; end its session before forgetting")
		}
	}
	return forget()
}

// CheckRetiredReferences excludes registration during the durable reference check.
func (s *Registry) CheckRetiredReferences(retired map[string]bool, checkDetached func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if retired[sess.identity.hostedThreadID] || retired[sess.identity.invokerHostedThreadID] {
			return fmt.Errorf("retired identity still has a runtime attachment or invocation edge")
		}
	}
	return checkDetached()
}

// NewRegistry constructs the live membership owner.
func NewRegistry() Registry {
	return Registry{sessions: map[string]*Session{}}
}

// LiveHostedSummary reads the oldest live attachment without summarizing unrelated sessions.
func (s *Registry) LiveHostedSummary(hostedID string, needsAttention func(string) bool) (Summary, bool) {
	oldest := liveHostedThreadSession(s.hostedThreadSessions(hostedID))
	if oldest == nil {
		return Summary{}, false
	}
	return oldest.state.volatileSummary(needsAttention(hostedID))
}

// MonitorSnapshot counts held sessions and snapshots their active turns.
func (s *Registry) MonitorSnapshot() (int, []observability.ActiveTurn) {
	sessions := s.SnapshotSessions()
	turns := []observability.ActiveTurn{}
	for _, sess := range sessions {
		if turn, active := sess.state.ActiveTurnSnapshot(); active {
			turns = append(turns, turn)
		}
	}
	return len(sessions), turns
}

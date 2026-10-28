package app

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// sessionRegistry owns the live-session map and its lock.
type sessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]*session
}

// sessionForThread returns the LIVE session hosting (provider,
// nativeThreadID), if any. Exited sessions are passed over: a resume that died
// pre-init (purged transcript) leaves a corpse still holding the thread id it
// seeded at spawn, and both callers mean "live" — the claude resume guard
// (session.go) must let the operator retry rather than 409 them into the dead
// session, and RuntimeFor must read the thread as detached-or-durable, not
// attached. The guard in codexDaemon.claimThread/registerThread keeps codex
// threads single-attached and the claude resume guard does the same for
// claude, so at most one *live* session matches; exited corpses may coexist
// with their replacement until closed.
func (s *sessionRegistry) sessionForThread(provider, nativeThreadID string) *session {
	if nativeThreadID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.provider != provider || sess.ThreadID() != nativeThreadID {
			continue
		}
		sess.stateMu.Lock()
		exited := sess.connectionStatusLocked() == sessionStatusExited
		sess.stateMu.Unlock()
		if exited {
			continue
		}
		return sess
	}
	return nil
}

// liveSessionForResume finds a live session that already hosts the thread a
// resume names, by native id or, before Codex learns its native id, by
// hosted id. Exited sessions do not count: the operator may resume those.
func (s *sessionRegistry) liveSessionForResume(provider, nativeThreadID, hostedThreadID string) *session {
	if existing := s.sessionForThread(provider, nativeThreadID); existing != nil {
		return existing
	}
	existing := s.sessionForHostedThread(hostedThreadID)
	if existing == nil || hostedThreadID == "" {
		return nil
	}
	existing.stateMu.Lock()
	exited := existing.connectionStatusLocked() == sessionStatusExited
	existing.stateMu.Unlock()
	if exited {
		return nil
	}
	return existing
}

// sessionForHostedThread returns the session a hosted thread's operations
// use. A failed attach can leave an exited session beside the one that
// replaced it, so map order must not choose: the live session wins
// (liveHostedThreadSession). Callers must not hold a session's stateMu.
func (s *sessionRegistry) sessionForHostedThread(id string) *session {
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
func (s *sessionRegistry) hostedThreadSessions(id string) []*session {
	s.mu.Lock()
	defer s.mu.Unlock()
	matches := make([]*session, 0, 1)
	for _, sess := range s.sessions {
		if sess.hostedThreadID == id {
			matches = append(matches, sess)
		}
	}
	return matches
}

// liveHostedThreadSession is the oldest of a hosted thread's sessions that has
// not exited, with the fleet summary's timestamp precision and ID tie-break,
// or nil when every session has exited.
func liveHostedThreadSession(matches []*session) *session {
	var oldest *session
	var oldestCreated string
	for _, sess := range matches {
		sess.stateMu.Lock()
		exited := sess.connectionStatusLocked() == sessionStatusExited
		sess.stateMu.Unlock()
		if exited {
			continue
		}
		created := sess.createdAt.Format(time.RFC3339)
		if oldest == nil || created < oldestCreated || (created == oldestCreated && sess.id < oldest.id) {
			oldest, oldestCreated = sess, created
		}
	}
	return oldest
}

func (s *sessionRegistry) resolveLiveHostedThreadRecord(ref string, lookup func(string) (agentSessionRecord, bool)) (agentSessionRecord, bool) {
	s.mu.Lock()
	hostedThreadID := ""
	for _, sess := range s.sessions {
		if sess.id == ref {
			hostedThreadID = sess.hostedThreadID
			break
		}
	}
	if hostedThreadID == "" {
		for _, sess := range s.sessions {
			if sess.ThreadID() != ref {
				continue
			}
			if hostedThreadID != "" && hostedThreadID != sess.hostedThreadID {
				s.mu.Unlock()
				return agentSessionRecord{}, false // provider-native reference is ambiguous
			}
			hostedThreadID = sess.hostedThreadID
		}
	}
	s.mu.Unlock()
	if hostedThreadID == "" {
		return agentSessionRecord{}, false
	}
	return lookup(hostedThreadID)
}

// lookupSession reads one registered transport attachment.
func (s *sessionRegistry) lookupSession(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// snapshotSessions copies registry membership before callers inspect sessions.
func (s *sessionRegistry) snapshotSessions() []*session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	return sessions
}

func (s *sessionRegistry) registerSession(sess *session) {
	s.mu.Lock()
	s.sessions[sess.id] = sess
	s.mu.Unlock()
}

func (s *sessionRegistry) removeSession(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// removeCurrentSession leaves any replacement attachment registered.
func (s *sessionRegistry) removeCurrentSession(sess *session) {
	s.mu.Lock()
	if s.sessions[sess.id] == sess {
		delete(s.sessions, sess.id)
	}
	s.mu.Unlock()
}

// volatileSummaries snapshots live state without reading canonical stores.
func (s *sessionRegistry) volatileSummaries(needsAttention map[string]bool) []sessionSummary {
	sessions := s.snapshotSessions()
	summaries := make([]sessionSummary, 0, len(sessions))
	for _, sess := range sessions {
		summary, live := sess.volatileSummary(needsAttention[sess.hostedThreadID])
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

// attachmentSession includes exited attachments for explicit-close routing.
func (s *sessionRegistry) attachmentSession(provider, nativeID string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.provider == provider && sess.ThreadID() == nativeID {
			return sess
		}
	}
	return nil
}

// forgetDetachedClaude keeps registration excluded until durable forgetting ends.
func (s *sessionRegistry) forgetDetachedClaude(nativeID string, forget func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.provider == "claude" && sess.ThreadID() == nativeID && sess.summary().Status != sessionStatusExited {
			return fmt.Errorf("thread is attached; end its session before forgetting")
		}
	}
	return forget()
}

// checkRetiredReferences excludes registration during the durable reference check.
func (s *sessionRegistry) checkRetiredReferences(retired map[string]bool, checkDetached func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if retired[sess.hostedThreadID] || retired[sess.invokerHostedThreadID] {
			return fmt.Errorf("retired identity still has a runtime attachment or invocation edge")
		}
	}
	return checkDetached()
}

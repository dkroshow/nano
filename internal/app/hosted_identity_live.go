package app

import (
	"github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/sessions"
)

func (s *server) liveHostedSession(hostedID string) (sessions.Summary, bool) {
	return s.LiveHostedSummary(hostedID, s.agentExecutions.SessionNeedsAttention)
}

type hostedIdentityLive struct{ server *server }

func (a hostedIdentityLive) HostedIdentity(id string) (hosted.LiveIdentity, bool) {
	summary, ok := a.server.liveHostedSession(id)
	return liveIdentity(summary), ok
}
func (a hostedIdentityLive) Identities() []hosted.LiveIdentity {
	summaries := a.server.volatileSessionSummaries()
	result := make([]hosted.LiveIdentity, 0, len(summaries))
	for _, summary := range summaries {
		result = append(result, liveIdentity(summary))
	}
	return result
}
func liveIdentity(summary sessions.Summary) hosted.LiveIdentity {
	return hosted.LiveIdentity{ID: summary.ID, Name: summary.Name, Provider: summary.Provider, ThreadID: summary.ThreadID, HostedThreadID: summary.HostedThreadID, CreatedAt: summary.CreatedAt, LastTurnEndedAt: summary.LastTurnEndedAt}
}

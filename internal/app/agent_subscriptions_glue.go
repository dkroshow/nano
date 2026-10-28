package app

import (
	"log"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/interact"
)

type subscriptionDispatcher = subscriptions.Dispatcher
type subscriptionTarget = subscriptions.Target

func serverFeatureAction(actionID string) bool {
	return subscriptions.IsFeatureAction(actionID)
}

// startSubscriptionDispatcher publishes the dispatcher before its catch-up
// sweep starts. Nothing polls, so a turn saved after the catch-up has read its
// child's feed reaches the invoker only if its finished-turn hook finds the
// dispatcher.
func startSubscriptionDispatcher(s *server) {
	if s == nil || s.agentExecutions == nil || s.agentExecutions.Control() == nil {
		return
	}
	d := subscriptions.NewDispatcher(s.agentExecutions.Control().CanonicalDatabase(), interact.NewHTTPClient(s.defaults.apiOrigin, nil), s.agentDisplay, log.Printf)
	s.subscriptionMu.Lock()
	s.subscriptions = d
	s.subscriptionMu.Unlock()
	d.Start()
}

// sweepSubscriptions is every session's finished-turn hook. It takes the
// subscription lock on its own goroutine, so a caller that holds that lock cannot
// deadlock on it.
func (s *server) sweepSubscriptions(hostedThreadID string) {
	go func() {
		s.subscriptionMu.Lock()
		d := s.subscriptions
		s.subscriptionMu.Unlock()
		if d != nil {
			d.Sweep(hostedThreadID)
		}
	}()
}

var _ subscriptions.Client = (*interact.HTTPClient)(nil)

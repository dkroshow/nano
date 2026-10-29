package agentapi

import "github.com/dkroshow/nano/internal/features/subscriptions"

// StartSubscriptions publishes the dispatcher before its catch-up sweep starts.
func (s *Controller) StartSubscriptions(d *subscriptions.Dispatcher) {
	s.subscriptionMu.Lock()
	s.subscriptions = d
	s.subscriptionMu.Unlock()
	d.Start()
}

// SweepSubscriptions schedules a finished-turn sweep without holding the caller.
func (s *Controller) SweepSubscriptions(hostedThreadID string) {
	go func() {
		s.subscriptionMu.Lock()
		d := s.subscriptions
		s.subscriptionMu.Unlock()
		if d != nil {
			d.Sweep(hostedThreadID)
		}
	}()
}

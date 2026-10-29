package app

import (
	"fmt"
	"net/http"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
)

// launchd sends SIGKILL 5 s after SIGTERM (the job sets no ExitTimeOut). The
// stop finishes within that deadline.
const (
	stopDeliveryWait = 3 * time.Second
	stopDeadline     = 4 * time.Second
)

// stopReport says what the stop waited for. A delivery still running at exit
// is restored as uncertain on the next boot and is never resent.
type stopReport struct {
	finished, unfinished int
	deadlineReached      bool
}

func (r stopReport) String() string {
	if r.deadlineReached {
		return "stop deadline reached before in-flight deliveries were counted"
	}
	return fmt.Sprintf("%d in-flight deliveries finished, %d still running", r.finished, r.unfinished)
}

// stop begins Nano's exit. Writes are refused and no new provider delivery
// starts; deliveries already in flight get deliveryWait to record their
// outcome. It returns by deadline even if a step blocks. Provider daemons are
// never touched: their turns continue and the next process reattaches them.
func (s *server) stop(deliveryWait, deadline time.Duration) stopReport {
	started := time.Now()
	s.stopping.Store(true)
	waited := make(chan stopReport, 1)
	go func() { waited <- s.awaitInFlightDeliveries(started.Add(deliveryWait)) }()
	timer := time.NewTimer(deadline - time.Since(started))
	defer timer.Stop()
	select {
	case report := <-waited:
		return report
	case <-timer.C:
		return stopReport{deadlineReached: true}
	}
}

// awaitInFlightDeliveries waits until every delivery that started before the
// stop has finished, or until the given time. A claim holds its session's
// sendMu while it registers its operation, so each session's operations are
// complete once that lock is taken after stopping is set.
func (s *server) awaitInFlightDeliveries(until time.Time) stopReport {
	sessions := s.SnapshotSessions()
	var inFlight []chan struct{}
	for _, sess := range sessions {
		inFlight = append(inFlight, sess.Delivery().InFlightDeliveries()...)
	}
	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()
	for i, done := range inFlight {
		select {
		case <-done:
		case <-timer.C:
			unfinished := 0
			for _, rest := range inFlight[i:] {
				select {
				case <-rest:
				default:
					unfinished++
				}
			}
			return stopReport{finished: len(inFlight) - unfinished, unfinished: unfinished}
		}
	}
	return stopReport{finished: len(inFlight)}
}

// refuseWritesWhileStopping answers every write with a retry once Nano has
// begun to stop, before any handler can record it. Reads and open streams
// continue until exit. It wraps the application inside the listener guards,
// so Host, Origin and remote authentication still decide first.
func (s *server) refuseWritesWhileStopping(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.stopping.Load() && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			w.Header().Set("Retry-After", "10")
			httpapi.WriteError(w, http.StatusServiceUnavailable, "restarting", "Nano is restarting. Nothing was recorded; retry in a few seconds.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

package claudeshim

import (
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/wire"
)

// DataLines drops the control channel's frames. Control traffic is real wire
// traffic and belongs in the Event Feed, but a test about conversation lines
// must not have to re-assert its existence on every change.
func DataLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if !strings.Contains(line, `"type":"control_`) {
			out = append(out, line)
		}
	}
	return out
}

func DataEvents(events []wire.Event) []wire.Event {
	out := make([]wire.Event, 0, len(events))
	for _, evt := range events {
		if !strings.Contains(string(evt.Msg), `"type":"control_`) {
			out = append(out, evt)
		}
	}
	return out
}

func WaitForWithin(t *testing.T, timeout time.Duration, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func WaitFor(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

package sessions

import (
	"reflect"
	"testing"

	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/observability"
)

// The application sibling tests the record's aggregation. This half verifies
// the callbacks emitted across the session lifecycle boundary.
func TestRestartRecordTakesLiveAndFailureFromTheLifecycle(t *testing.T) {
	for _, test := range []struct {
		events []lifecycle.Event
		want   []string
	}{
		{[]lifecycle.Event{lifecycle.EventReadyIdle}, []string{"live"}},
		{[]lifecycle.Event{lifecycle.EventRuntimeExited}, []string{"exited"}},
		{[]lifecycle.Event{lifecycle.EventReadyIdle, lifecycle.EventRuntimeExited}, []string{"live", "exited"}},
	} {
		snapshot, err := lifecycle.New(1)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		sess := &Session{lifecycleState: snapshot, restartProgress: func(step observability.RestartStep) {
			if step == restartLive {
				got = append(got, "live")
			}
		}, restartFailure: func(reason string) { got = append(got, reason) }}
		sess.stateMu.Lock()
		for _, event := range test.events {
			sess.transitionLifecycleLocked(event)
		}
		sess.stateMu.Unlock()
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%v: callbacks=%v, want %v", test.events, got, test.want)
		}
	}
}

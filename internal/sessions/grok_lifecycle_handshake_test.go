package sessions

import (
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/lifecycle"
)

func TestGrokCurrentUsesLoadResponseIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		want           lifecycle.Event
		valid          bool
	}{
		{"idle", `{"_meta":{"sessionId":"native"}}`, lifecycle.EventReadyIdle, true},
		{"working", `{"_meta":{"sessionId":"native","x.ai/runningPromptId":"running"}}`, lifecycle.EventReadyWorking, true},
		{"synthetic", `{"_meta":{"sessionId":"native","x.ai/runningPromptId":"subagent-completed-child"}}`, lifecycle.EventReadyIdle, true},
		{"wrong-native", `{"_meta":{"sessionId":"other"}}`, "", false},
		{"requested-native-idle", `{}`, lifecycle.EventReadyIdle, true},
		{"requested-native-working", `{"_meta":{"x.ai/runningPromptId":"running"}}`, lifecycle.EventReadyWorking, true},
		{"detail-native", `{"_meta":{"x.ai/sessionDetail":{"sessionId":"native"}}}`, lifecycle.EventReadyIdle, true},
		{"wrong-detail-native", `{"_meta":{"x.ai/sessionDetail":{"sessionId":"other"}}}`, "", false},
		{"malformed-response", `{"_meta":`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := grokLifecycleAdapter{epoch: 7, sequence: 11, settled: true, nativeID: "native", response: json.RawMessage(tc.response)}
			observation, ok := adapter.Current()
			if ok != tc.valid || (ok && observation != (lifecycle.Observation{Event: tc.want, Epoch: 7, Sequence: 11})) {
				t.Fatalf("Current() = %+v, %v; want event %s, valid %v", observation, ok, tc.want, tc.valid)
			}
		})
	}
}

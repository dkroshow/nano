package codex

import (
	"fmt"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestNativeTaskDurationRetainsItsOwnUnitAndEvidence(t *testing.T) {
	for _, field := range []string{"duration_ms", "durationMs"} {
		for _, value := range []string{"2750", "0", "null", "-1", "2.5", `"3"`} {
			t.Run(field+"/"+value, func(t *testing.T) {
				raw := []byte(fmt.Sprintf(`{"type":"event_msg","timestamp":"2026-01-01T00:00:03Z","payload":{"type":"task_complete","turn_id":"turn",%q:%s}}`+"\n", field, value))
				state, batch, err := parseFileView(parsedView{}, inputFor(raw))
				if err != nil {
					t.Fatal(err)
				}
				if len(state.Turns) != 1 {
					t.Fatal("native task terminal did not retain its turn")
				}
				for _, turn := range state.Turns {
					if value != "2750" && value != "0" {
						if len(turn.Durations) != 0 || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "invalid_duration" {
							t.Fatal("invalid native duration became a number or lost its diagnostic")
						}
						continue
					}
					want := int64(2750)
					if value == "0" {
						want = 0
					}
					if len(turn.Durations) != 1 || turn.DurationMS != want || len(batch.Diagnostics) != 0 {
						t.Fatal("reported duration was lost or computed from unrelated clocks")
					}
					duration := turn.Durations[0]
					if duration.Value != want || duration.Unit != "millisecond" || duration.Basis != "native" || duration.SourceField != "payload."+field || duration.Scope.Kind != model.ScopeTurn || len(turn.Completion) != 1 || duration.ObservationRef != turn.Completion[0].ObservationRefs[0] {
						t.Fatal("duration lost its native field, scope or completion reference")
					}
				}
			})
		}
	}
}

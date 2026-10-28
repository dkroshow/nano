package hosted

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestTrimThreadPinContextKeepsAnchorWithinBudget(t *testing.T) {
	big := strings.Repeat("x", 60<<10)
	response := threadPinContextResponse{}
	for ordinal := 1; ordinal <= 6; ordinal++ {
		response.Turns = append(response.Turns, threadPinContextTurn{Ordinal: ordinal, Messages: []threadPinContextMessage{
			{ItemID: fmt.Sprintf("u%d", ordinal), Text: big, ContentState: "retained_complete", Complete: true, preview: "p"},
			{ItemID: fmt.Sprintf("a%d", ordinal), Text: "short", ContentState: "inline", Complete: true, preview: "short"},
		}})
	}
	last := &response.Turns[5]
	last.IsAnchorTurn = true
	last.Messages = append(last.Messages, threadPinContextMessage{ItemID: "anchor", Text: strings.Repeat("y", 8<<10), IsAnchor: true, ContentState: "inline", Complete: true})
	trimThreadPinContext(&response)
	encoded, _ := json.Marshal(response)
	if len(encoded) > pinContextResponseBudget || !response.TrimmedForSize || len(response.Turns) != 6 {
		t.Fatalf("size %d trimmed %v turns %d", len(encoded), response.TrimmedForSize, len(response.Turns))
	}

	huge := strings.Repeat("z", 8<<10)
	response = threadPinContextResponse{}
	for ordinal, count := range []int{5, 40} {
		turn := threadPinContextTurn{Ordinal: ordinal + 1}
		for index := 0; index < count; index++ {
			turn.Messages = append(turn.Messages, threadPinContextMessage{ItemID: fmt.Sprintf("m%d-%d", ordinal, index), Text: huge, ContentState: "inline", Complete: true})
		}
		response.Turns = append(response.Turns, turn)
	}
	response.Turns[1].Messages = append(response.Turns[1].Messages, threadPinContextMessage{ItemID: "anchor", Text: "a", IsAnchor: true})
	trimThreadPinContext(&response)
	encoded, _ = json.Marshal(response)
	anchorTurn := response.Turns[len(response.Turns)-1]
	if len(encoded) > pinContextResponseBudget || len(response.Turns) != 1 || !response.HasEarlierHistory || !anchorTurn.StartsMid ||
		anchorTurn.Messages[len(anchorTurn.Messages)-1].ItemID != "anchor" {
		t.Fatalf("size %d turns %d earlier %v", len(encoded), len(response.Turns), response.HasEarlierHistory)
	}
}

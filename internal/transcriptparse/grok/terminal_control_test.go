package grok

import (
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestGrokRPCStopReasonRequiresPromptReplyAuthority(t *testing.T) {
	for _, method := range []string{"", "session/load", "session/request_permission", "session/prompt"} {
		t.Run(method, func(t *testing.T) {
			state, _, err := parseWireView(parsedView{}, inputBytes([]byte(`{"method":"_x.ai/queue/changed","params":{"sessionId":"s","runningPromptId":"p","runningText":"ask"}}`)))
			if err != nil {
				t.Fatal(err)
			}
			if method != "" {
				state, _, err = parseWireView(state, inputBytes([]byte(`{"id":1,"method":"`+method+`","params":{"sessionId":"s"}}`)))
				if err != nil {
					t.Fatal(err)
				}
			}
			state, batch, err := parseWireView(state, inputBytes([]byte(`{"id":1,"result":{"stopReason":"end_turn","_meta":{"promptId":"p"}}}`)))
			if err != nil {
				t.Fatal(err)
			}
			if method == "session/prompt" {
				if len(state.Turns) != 1 {
					t.Fatal("matched native prompt reply lost terminal")
				}
				for _, turn := range state.Turns {
					if turn.Outcome != model.OutcomeCompleted {
						t.Error("matched native prompt did not complete")
					}
				}
				found := false
				for _, obs := range batch.Observations {
					if obs.Source.BlockPath == "$.result" {
						found = true
					}
				}
				if !found {
					t.Error("RPC terminal source path points to notification params")
				}
			} else if len(state.Turns) != 0 {
				t.Error("unrelated or uncorrelated RPC response completed a conversation")
			}
		})
	}
}

func TestGrokMissingSessionTerminalCannotBorrowAnotherConnection(t *testing.T) {
	input := inputBytes([]byte(`{"method":"_x.ai/queue/changed","params":{"sessionId":"s","runningPromptId":"p","runningText":"ask"}}`))
	input.Scope.ConnectionID = "first"
	state, _, err := parseWireView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(state)
	input.Scope.ConnectionID = "second"
	input.Bytes = []byte(`{"method":"_x.ai/session/prompt_complete","params":{"promptId":"p","stopReason":"end_turn"}}`)
	next, batch, err := parseWireView(state, input)
	if err != nil || len(next.Turns) != 0 {
		t.Fatal("terminal borrowed native session from another connection")
	}
	if len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "unscoped_terminal" {
		t.Error("missing same-connection identity was not diagnosed")
	}
	after, _ := json.Marshal(state)
	if string(before) != string(after) {
		t.Error("terminal changed prior checkpoint")
	}
	input.Scope.ConnectionID = "first"
	next, _, err = parseWireView(state, input)
	if err != nil || len(next.Turns) != 1 {
		t.Fatal("exact connection binding no longer accepted")
	}
}

func TestGrokWrappedTerminalRetainsNativeSourcePath(t *testing.T) {
	for _, tc := range []struct{ name, row, path string }{
		{"reply", `{"id":1,"result":{"stopReason":"end_turn","_meta":{"promptId":"p"}}}`, "$.msg.result"},
		{"notification", `{"method":"_x.ai/session/prompt_complete","params":{"sessionId":"s","promptId":"p","stopReason":"end_turn"}}`, "$.msg.params"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, _, err := parseWireView(parsedView{}, inputBytes([]byte(`{"id":1,"method":"session/prompt","params":{"sessionId":"s"}}`)))
			if err != nil {
				t.Fatal(err)
			}
			row := []byte(`{"seq":7,"dir":"recv","ts":"2026-09-07T12:00:00Z","msg":` + tc.row + `}`)
			_, batch, err := parseWireView(state, inputBytes(row))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, obs := range batch.Observations {
				if obs.Outcome != model.OutcomeCompleted {
					continue
				}
				found = true
				if obs.Source.BlockPath != tc.path {
					t.Errorf("terminal source %q; want %q", obs.Source.BlockPath, tc.path)
				}
				if obs.Source.SHA256 != batch.Records[0].Source.SHA256 || obs.Source.Direction != "recv" {
					t.Error("terminal lost capture provenance")
				}
			}
			if !found {
				t.Fatal("native terminal missing")
			}
		})
	}
}

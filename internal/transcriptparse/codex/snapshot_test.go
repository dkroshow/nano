package codex

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexResumeSnapshotRetainsUnprovenOverlap(t *testing.T) {
	rows := bytes.Split(bytes.TrimSpace(fixture(t, "edge-wire-resume-delta-race")), []byte{'\n'})
	state, _, err := parseWireView(parsedView{}, inputFor(rows[0]))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(state)
	var restored parsedView
	if err := json.Unmarshal(before, &restored); err != nil {
		t.Fatal(err)
	}
	next, batch, err := parseWireView(restored, inputFor(rows[1]))
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(restored)
	if !bytes.Equal(before, after) {
		t.Error("snapshot mutated its checkpoint")
	}
	items := orderedItems(next)
	if len(items) != 1 || items[0].Text != "B" || items[0].SourceSeq != 1 || len(items[0].Observations) != 2 {
		t.Fatal("snapshot replaced the received delta or discarded baseline evidence")
	}
	baseline := items[0].Observations[1]
	if baseline.Text != "A" || baseline.Kind != model.ObservationMetadata || baseline.Source.BlockPath != "result.thread.turns[0].items[0]" || items[0].Capture != model.CapturePartial {
		t.Error("ambiguous baseline lacks text, source path or partial coverage")
	}
	if !hasSnapshotDiagnostic(batch, "snapshot_delta_overlap_unproven") {
		t.Error("unproven overlap was not diagnosed")
	}
	if items[0].CreatedAt != nil {
		t.Error("turn start became invented item time")
	}
}

func TestCodexResumeSnapshotKeepsMissingCoverageAndNativeTiming(t *testing.T) {
	state, batch, err := parseWireView(parsedView{}, inputFor(fixture(t, "edge-wire-resume-itemsview-and-timing")))
	if err != nil || len(state.Turns) != 1 || len(state.Items) != 0 {
		t.Fatalf("snapshot: %v, %d turns", err, len(state.Turns))
	}
	if !hasSnapshotDiagnostic(batch, "unresolved_snapshot_response") {
		t.Error("uncorrelated response gained request authority")
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeCompleted || turn.Capture != model.CapturePartial || turn.StartSourceSeq != 0 || turn.EndSourceSeq != 0 {
			t.Error("notLoaded became an empty complete capture")
		}
		seen := map[model.TimeMeaning]bool{}
		for _, stamp := range turn.Times {
			if stamp.Class == model.TimeNative && stamp.Value != nil {
				if stamp.Precision != "second" || stamp.Scope.TurnKey != turn.Key {
					t.Error("native clock unit or owner changed")
				}
				seen[stamp.Meaning] = true
			}
		}
		if !seen[model.TimeStart] || !seen[model.TimeEnd] {
			t.Error("native turn clocks lost")
		}
		if len(turn.Observations) != 1 || !bytes.Contains(turn.Observations[0].Detail, []byte(`"durationMs":2100`)) || !bytes.Contains(turn.Observations[0].Detail, []byte(`"itemsView":"notLoaded"`)) {
			t.Error("native duration/coverage lost")
		}
	}
}

func TestCodexCorrelatedThreadSnapshotsHaveDistinctSourcePaths(t *testing.T) {
	for _, method := range []string{"thread/start", "thread/read", "thread/resume", "thread/fork"} {
		t.Run(method, func(t *testing.T) {
			input := inputFor([]byte(`{"id":8,"method":"` + method + `","params":{"threadId":"thread"}}`))
			input.Source.Direction = "send"
			state, _, err := parseWireView(parsedView{}, input)
			if err != nil {
				t.Fatal(err)
			}
			input.Bytes = []byte(`{"id":8,"result":{"accessToken":"SECRET_VALUE","thread":{"id":"thread","turns":[{"id":"a","status":"completed","itemsView":"full","startedAt":1767225600,"items":[{"type":"userMessage","id":"u","content":[{"type":"text","text":"ask"}]},{"type":"agentMessage","id":"m","text":"answer","phase":"final_answer"}]},{"id":"b","status":"inProgress","itemsView":"notLoaded","items":[]}]}}}`)
			input.Source.Direction = "recv"
			state, batch, err := parseWireView(state, input)
			if err != nil || len(batch.Diagnostics) != 0 || len(state.Turns) != 2 || len(state.Items) != 2 {
				t.Fatalf("snapshot: %v %+v", err, batch.Diagnostics)
			}
			seen := map[string]bool{}
			for _, obs := range batch.Observations {
				if seen[obs.ID] {
					t.Error("nested snapshot observation IDs collided")
				}
				seen[obs.ID] = true
			}
			encoded, _ := json.Marshal(batch)
			if bytes.Contains(encoded, []byte("SECRET_VALUE")) {
				t.Error("unrelated response credential escaped")
			}
			for _, turn := range state.Turns {
				if turn.NativeTurnID == "a" && (turn.Outcome != model.OutcomeCompleted || turn.Capture != model.CaptureComplete || len(turn.FinalItemRefs) != 1) {
					t.Error("completed full snapshot lost evidence")
				}
				if turn.NativeTurnID == "b" && turn.Outcome != model.OutcomeRunning {
					t.Error("active snapshot turn lost its status")
				}
			}
			replayed, batch, err := parseWireView(state, input)
			if err != nil || len(batch.Diagnostics) != 0 || len(replayed.Items) != 2 || len(replayed.Turns) != 2 {
				t.Error("exact RPC replay reapplied snapshot")
			}
		})
	}
}

func hasSnapshotDiagnostic(batch model.Batch, code string) bool {
	for _, diagnostic := range batch.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func TestCodexSnapshotsDoNotSupplyAnActiveTurnDefault(t *testing.T) {
	state, _, err := parseWireView(parsedView{}, inputFor(fixture(t, "edge-wire-resume-itemsview-and-timing")))
	if err != nil {
		t.Fatal(err)
	}
	state, batch, err := parseWireView(state, inputFor([]byte(`{"method":"item/agentMessage/delta","params":{"threadId":"thread-example","itemId":"later","delta":"new"}}`)))
	if err != nil || len(batch.Diagnostics) != 0 {
		t.Fatalf("delta: %v %+v", err, batch.Diagnostics)
	}
	items := orderedItems(state)
	if len(items) != 1 || items[0].Scope.NativeTurnID != nil || len(state.Turns) != 2 {
		t.Error("snapshot supplied an unsupported active turn default")
	}
}

func TestCodexSnapshotEmptyAndErrorResponsesDoNotMintTurns(t *testing.T) {
	for _, response := range []string{`{"id":8,"result":{"thread":{"id":"thread","turns":[]}}}`, `{"id":8,"error":{"code":-1,"message":"SECRET_VALUE"}}`} {
		input := inputFor([]byte(`{"id":8,"method":"thread/read","params":{"threadId":"thread"}}`))
		input.Source.Direction = "send"
		state, _, err := parseWireView(parsedView{}, input)
		if err != nil {
			t.Fatal(err)
		}
		input.Source.Direction, input.Bytes = "recv", []byte(response)
		state, batch, err := parseWireView(state, input)
		if err != nil || len(state.Turns) != 0 || len(state.Items) != 0 {
			t.Error("empty/error snapshot invented content")
		}
		raw, _ := json.Marshal(batch)
		if bytes.Contains(raw, []byte("SECRET_VALUE")) {
			t.Error("RPC error leaked opaque text")
		}
	}
}

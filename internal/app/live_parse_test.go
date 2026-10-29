package app

import (
	"os"
	"testing"
)

func TestClaudeDistinctNativeBlocksSurviveSharedCallInFileAndWire(t *testing.T) {
	raw, err := os.ReadFile("../transcriptparse/claude/testdata/ambiguous-call-id/input.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range [][]fixtureDifferentialView{fixtureConvertedFileView(t, "claude", "file", raw)} {
		calls := map[string]bool{}
		for _, item := range view {
			if (item.Type == "tool_call" || item.Type == "command") && item.Call == "c" {
				calls[item.NativeID] = true
			}
		}
		if len(view) != 4 || len(calls) != 2 || !calls["m1:0"] || !calls["m2:0"] {
			t.Fatal("a shared call ID collapsed distinct native blocks or hid the source")
		}
	}
}

func TestClaudeIndependentControlsSurviveSharedCallInFileAndWire(t *testing.T) {
	raw, err := os.ReadFile("../transcriptparse/claude/testdata/research/control-replay-answered-and-pending/input.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	// The live transcript shows provider requests as pending interactions,
	// not rows, so only saved history carries them as items.
	view := fixtureModuleView(t, "claude", "file", raw)
	if len(view) != 2 || view[0].Tool != "Bash" || view[1].Tool != "AskUserQuestion" || view[0].Call != "call1" || view[1].Call != "call1" {
		t.Fatalf("answered and pending requests must remain independent: %+v", view)
	}
	if view[0].Type != "interaction" || view[1].Type != "interaction" || view[0].Status != "completed" || view[1].Status != "in_progress" {
		t.Fatalf("stored requests lost their independent outcomes: %+v", view)
	}
}

func TestCodexIndependentInvocationsSurviveSharedCallInFile(t *testing.T) {
	raw, err := os.ReadFile("../transcriptparse/codex/testdata/ambiguous-call-id/input.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	view := fixtureConvertedFileView(t, "codex", "file", raw)
	if len(view) != 4 || view[1].Type != "command" || view[2].Type != "command" || view[1].TextHash != fixtureDigest([]byte("false")) || view[2].TextHash != fixtureDigest([]byte("true")) || view[3].Type != "tool_result" {
		t.Fatal("shared call ID collapsed distinct invocations or their unresolved result")
	}
}

func TestClaudeEvictedSecondBlockKeepsItsRow(t *testing.T) {
	raw, err := os.ReadFile("../transcriptparse/claude/testdata/replay-block-index-reset.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range [][]fixtureDifferentialView{fixtureConvertedFileView(t, "claude", "file", raw)} {
		calls := map[string][]string{}
		for _, item := range view {
			if item.Call != "" {
				calls[item.Call] = append(calls[item.Call], item.NativeID)
			}
		}
		if len(calls) != 2 || len(calls["call-a"]) != 1 || len(calls["call-b"]) != 1 || calls["call-a"][0] != "message:0" || calls["call-b"][0] != "message:1" {
			t.Fatalf("late second block: rows=%d calls=%v", len(view), calls)
		}
	}
}

func TestClaudeAmbiguousCallWithoutMessageIDSurvives(t *testing.T) {
	raw, err := os.ReadFile("../transcriptparse/claude/testdata/ambiguous-call-id/input.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte(`{"type":"assistant","uuid":"third","message":{"content":[{"type":"tool_use","id":"c","name":"Bash","input":{"command":"third"}}]}}`+"\n")...)
	for _, view := range [][]fixtureDifferentialView{fixtureConvertedFileView(t, "claude", "file", raw)} {
		calls := 0
		for _, item := range view {
			if item.Call == "c" && (item.Type == "command" || item.Type == "tool_call") {
				calls++
			}
		}
		if calls != 3 {
			t.Fatalf("ambiguous standalone command lost: calls=%d", calls)
		}
	}
}

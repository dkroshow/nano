package codex

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexProcessFixturesRetainMalformedBytesAndNativeExit(t *testing.T) {
	for _, name := range []string{"wire-process-outputdelta", "wire-command-exec-outputdelta"} {
		state, batch, err := parseWireView(parsedView{}, inputFor(fixture(t, name)))
		if err != nil || len(state.Items) != 0 || len(state.Turns) != 0 || !hasSnapshotDiagnostic(batch, "invalid_process_base64") || len(batch.Observations) != 1 || !bytes.Contains(batch.Observations[0].Detail, []byte(`"deltaBase64":"example"`)) {
			t.Errorf("malformed encoded output: %s %v %+v", name, err, batch.Diagnostics)
		}
	}
	state, batch, err := parseWireView(parsedView{}, inputFor(fixture(t, "wire-process-exited")))
	items := orderedItems(state)
	if err != nil || len(items) != 1 || len(state.Turns) != 0 {
		t.Fatalf("process exit: %v %+v", err, batch.Diagnostics)
	}
	if items[0].Type != model.ItemToolResult || !items[0].Complete || items[0].Outcome != model.OutcomeCompleted || len(items[0].Parts) != 2 || items[0].Parts[0].Text != "example" || items[0].Parts[1].Text != "example" {
		t.Error("native exit code or separate streams lost")
	}
	if !hasSnapshotDiagnostic(batch, "unresolved_process_invocation") {
		t.Error("orphan process result claimed invocation evidence")
	}
}

func TestCodexProcessStreamsAssembleBytesAcrossChunks(t *testing.T) {
	rows := []string{
		`{"method":"process/outputDelta","params":{"processHandle":"p","stream":"stdout","deltaBase64":"4g==","capReached":false}}`,
		`{"method":"process/outputDelta","params":{"processHandle":"p","stream":"stdout","deltaBase64":"gqw=","capReached":true}}`,
		`{"method":"process/outputDelta","params":{"processHandle":"p","stream":"stderr","deltaBase64":"eA==","capReached":false}}`,
		`{"method":"process/exited","params":{"processHandle":"p","exitCode":0,"stdout":"","stderr":"","stdoutCapReached":false,"stderrCapReached":false}}`,
	}
	var state parsedView
	for _, row := range rows {
		before, _ := json.Marshal(state)
		var restored parsedView
		if err := json.Unmarshal(before, &restored); err != nil {
			t.Fatal(err)
		}
		input := inputFor([]byte(row))
		thread, turn := "caller-thread", "caller-turn"
		input.Scope.NativeThreadID, input.Scope.NativeTurnID, input.Scope.ConnectionID = &thread, &turn, "connection"
		next, batch, err := parseWireView(restored, input)
		if err != nil {
			t.Fatal(err)
		}
		for _, diagnostic := range batch.Diagnostics {
			if diagnostic.Code != "unresolved_process_invocation" {
				t.Errorf("stream: %+v", diagnostic)
			}
		}
		after, _ := json.Marshal(restored)
		if !bytes.Equal(before, after) {
			t.Error("process update mutated checkpoint")
		}
		state = next
	}
	items := orderedItems(state)
	if len(items) != 1 || len(state.Turns) != 0 {
		t.Fatal("streams split process or invented turn")
	}
	item := items[0]
	if item.Scope.NativeThreadID != nil || item.Scope.NativeTurnID != nil || item.Outcome != model.OutcomeCompleted || !item.Complete || item.Capture != model.CaptureTruncated || !item.ContentTruncated {
		t.Error("native process scope, truncation or outcome lost")
	}
	if len(item.Parts) != 2 || item.Parts[0].Text != "€" || item.Parts[1].Text != "x" {
		t.Error("split UTF-8 or exit's empty buffers erased streamed output")
	}
	if len(item.Aliases) != 1 || item.Aliases[0].Namespace != "codex.processHandle" || item.NativeItemID != "" {
		t.Error("process handle became native item identity")
	}
	if len(state.Continuation.Processes) != 0 || len(state.Continuation.Items) != 0 {
		t.Fatal("exited process kept output buffers in continuation")
	}
}

func TestCodexProcessHandlesStayWithinFamilyAndConnection(t *testing.T) {
	var state parsedView
	for _, tc := range []struct{ method, field, connection string }{{"process/outputDelta", "processHandle", "a"}, {"command/exec/outputDelta", "processId", "a"}, {"process/outputDelta", "processHandle", "b"}} {
		input := inputFor([]byte(`{"method":"` + tc.method + `","params":{"` + tc.field + `":"same","stream":"stdout","deltaBase64":"YQ==","capReached":false}}`))
		input.Scope.ConnectionID = tc.connection
		var err error
		state, _, err = parseWireView(state, input)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(state.Items) != 3 {
		t.Fatal("process handles crossed method family or connection")
	}
	for _, item := range state.Items {
		if item.Text != "a" || item.Outcome != model.OutcomeUnknown || item.Complete {
			t.Error("output fragment invented execution completion")
		}
	}
}

func TestCodexProcessBinaryAndMalformedExitBuffersRemainExplicit(t *testing.T) {
	state, _, err := parseWireView(parsedView{}, inputFor([]byte(`{"method":"process/outputDelta","params":{"processHandle":"p","stream":"stdout","deltaBase64":"/w==","capReached":false}}`)))
	if err != nil {
		t.Fatal(err)
	}
	state, batch, err := parseWireView(state, inputFor([]byte(`{"method":"process/exited","params":{"processHandle":"p","exitCode":2,"stdout":"","stderr":null,"stdoutCapReached":false,"stderrCapReached":false}}`)))
	if err != nil || !hasSnapshotDiagnostic(batch, "non_utf8_process_output") || !hasSnapshotDiagnostic(batch, "invalid_process_buffer") {
		t.Fatalf("binary/malformed exit: %v %+v", err, batch.Diagnostics)
	}
	items := orderedItems(state)
	if len(items) != 1 || items[0].Outcome != model.OutcomeFailed || items[0].Parts[0].Text != "" || !bytes.Contains(items[0].Parts[0].Detail, []byte(`"data":"/w=="`)) {
		t.Error("binary bytes became replacement text or lost exit outcome")
	}
}

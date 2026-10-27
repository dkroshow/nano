package grok

import (
	"bytes"
	"encoding/json"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"os"
	"testing"
)

func TestGrokWholeSummaryKeepsSnapshotScope(t *testing.T) {
	raw, err := os.ReadFile("testdata/research/capture-summary/input.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, split := range []int{0, 1, 37, len(raw) / 2, len(raw)} {
		input := inputBytes(raw[:split])
		input.Format = "json"
		first, batch, err := parseFileView(parsedView{}, input)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.Observations) != 0 {
			t.Fatal("unfinished summary was published")
		}
		checkpoint, _ := json.Marshal(first)
		var restored parsedView
		if err = json.Unmarshal(checkpoint, &restored); err != nil {
			t.Fatal(err)
		}
		input = inputBytes(raw[split:])
		input.Format = "json"
		input.Closed = true
		state, batch, err := parseFileView(restored, input)
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Items) != 0 || len(state.Turns) != 0 || len(batch.Observations) != 1 || len(batch.Records) != 1 {
			t.Fatal("summary created transcript items or failed to emit metadata")
		}
		obs := batch.Observations[0]
		if obs.Scope.Kind != model.ScopeThread || obs.Scope.NativeThreadID == nil || *obs.Scope.NativeThreadID != "id-1" || len(obs.Times) != 6 {
			t.Fatal("summary identity/times lost")
		}
		var detail map[string]json.RawMessage
		if err = json.Unmarshal(obs.Detail, &detail); err != nil {
			t.Fatal(err)
		}
		if documentString(detail, "reasoning_effort") != "high" || documentString(detail, "current_model_id") != "[text]" {
			t.Fatal("summary metadata lost")
		}
		if obs.Metadata[model.MetadataNativeThreadID].Value != "id-1" || obs.Metadata[model.MetadataModel].Value != "[text]" || obs.Metadata[model.MetadataEffort].Value != "high" || obs.Metadata[model.MetadataPriority].State != model.MetadataUnknown {
			t.Fatal("summary catalog facts require provider decoding by the consumer")
		}
		after, _ := json.Marshal(restored)
		if !bytes.Equal(after, checkpoint) {
			t.Fatal("document parser mutated prior state")
		}
	}
}
func TestGrokWholeChildMetadataIsLineageNotParentCompletion(t *testing.T) {
	raw, err := os.ReadFile("testdata/research/capture-subagent-meta/input.json")
	if err != nil {
		t.Fatal(err)
	}
	input := inputBytes(raw)
	input.Format = "json"
	input.Closed = true
	state, batch, err := parseFileView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Turns) != 0 || len(batch.Observations) != 1 {
		t.Fatal("child metadata became a parent turn terminal")
	}
	obs := batch.Observations[0]
	if obs.Scope.NativeThreadID == nil || *obs.Scope.NativeThreadID != "id-654" || obs.Scope.ParentNativeID == nil || *obs.Scope.ParentNativeID != "id-1" {
		t.Fatal("native child lineage lost")
	}
	if obs.Metadata[model.MetadataNativeThreadID].Value != "id-654" || obs.Metadata[model.MetadataParentNativeID].Value != "id-1" || obs.Metadata[model.MetadataSourceKind].Value != "subagent" {
		t.Fatal("lineage requires native sidecar decoding by history")
	}
}

func TestGrokCompactionSnapshotsRetainPhaseWithoutReplayingHistory(t *testing.T) {
	for _, tc := range []struct{ name, phase, alias string }{
		{"capture-compaction-checkpoints", "checkpoint_saved", "grok.compactionCheckpointId"},
		{"capture-compaction-requests", "requested", "grok.compactionRequestId"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/research/" + tc.name + "/input.json")
			if err != nil {
				t.Fatal(err)
			}
			input := inputBytes(raw)
			input.Format = "json"
			input.Closed = true
			unrelated := "caller-turn"
			input.Scope.NativeTurnID = &unrelated
			state, batch, err := parseFileView(parsedView{}, input)
			if err != nil || len(batch.Diagnostics) != 0 {
				t.Fatalf("compaction snapshot: %v %+v", err, batch.Diagnostics)
			}
			items := controlItems(state)
			if len(items) != 1 || len(state.Turns) != 0 || len(batch.Observations) != 1 {
				t.Fatal("compaction lost or nested context became conversation")
			}
			item := items[0]
			if item.Type != model.ItemCompaction || item.Phase != tc.phase || !item.Complete || item.Outcome != model.OutcomeUnknown || item.CreatedAt == nil || item.Scope.NativeTurnID != nil || item.IsFinalAnswer {
				t.Error("snapshot phase, clock or scope wrong")
			}
			if len(item.Aliases) != 1 || item.Aliases[0].Namespace != tc.alias || rpcDigest(fieldsOf(item.Detail)) != rpcDigest(fieldsOf(raw)) {
				t.Error("compaction native identity/detail lost")
			}
			if batch.Records[0].Disposition != model.RecordSemantic {
				t.Error("compaction item has non-content disposition")
			}
		})
	}
}

func TestGrokUnverifiedCompactionSchemaRemainsMetadata(t *testing.T) {
	for _, raw := range []string{
		`{"schema_version":99,"checkpoint_id":"future","compacted_history":[]}`,
		`{"schema_version":1,"checkpoint_id":"broken","compacted_history":{}}`,
	} {
		input := inputBytes([]byte(raw))
		input.Format = "json"
		input.Closed = true
		state, batch, err := parseFileView(parsedView{}, input)
		if err != nil || len(state.Items) != 0 || len(batch.Observations) != 1 || !hasRPCDiagnostic(batch, "unverified_compaction_schema") {
			t.Fatal("unknown compaction schema interpreted or undiagnosed")
		}
		if rpcDigest(fieldsOf(batch.Observations[0].Detail)) != rpcDigest(fieldsOf([]byte(raw))) {
			t.Error("unverified snapshot lost native evidence")
		}
	}
}
func TestGrokWholeDocumentLimitsAndClosedState(t *testing.T) {
	input := inputBytes([]byte("{\n \"info\": {"))
	input.Format = "json"
	input.MaxRowBytes = 8
	state, batch, err := parseFileView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Continuation.Document) > 8 || len(batch.Records) != 0 {
		t.Fatal("open document exceeded memory limit or emitted early")
	}
	input = inputBytes([]byte("}}"))
	input.Format = "json"
	input.Closed = true
	state, batch, err = parseFileView(state, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "document_too_large" || len(batch.Records) != 1 || batch.Records[0].Capture != model.CaptureTruncated {
		t.Fatal("document overflow lost")
	}
	if _, _, err = parseFileView(state, input); err == nil {
		t.Fatal("closed document accepted more bytes")
	}
}

func TestGrokSnapshotLineageConflictsStayExplicit(t *testing.T) {
	var observations []model.Observation
	for _, name := range []string{"before-summary.json", "parent-a.meta.json", "after-summary.json", "parent-b.meta.json"} {
		raw, err := os.ReadFile("testdata/research/summary-rewrite-lineage-conflict/" + name)
		if err != nil {
			t.Fatal(err)
		}
		input := inputBytes(raw)
		input.Source.Label = name
		input.Format = "json"
		input.Closed = true
		_, batch, err := parseFileView(parsedView{}, input)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.Diagnostics) != 0 {
			t.Fatal("valid snapshot rejected")
		}
		observations = append(observations, batch.Observations...)
	}
	diagnostics := CheckLineage(observations)
	if len(diagnostics) != 1 || diagnostics[0].Code != "lineage_conflict" || diagnostics[0].Source.Label != "parent-b.meta.json" {
		t.Fatal("conflicting native parents were silently replaced")
	}
	if *observations[1].Scope.ParentNativeID != "parent-a" {
		t.Fatal("earlier lineage evidence mutated")
	}
	var first, last map[string]json.RawMessage
	_ = json.Unmarshal(observations[0].Detail, &first)
	_ = json.Unmarshal(observations[2].Detail, &last)
	if documentString(first, "current_model_id") != "model-a" || documentString(last, "current_model_id") != "model-b" {
		t.Fatal("new summary restamped earlier model evidence")
	}
}

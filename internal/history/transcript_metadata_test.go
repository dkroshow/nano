package history

import (
	"bytes"
	"os"
	"reflect"
	"testing"
	"time"

	parseclaude "github.com/dkroshow/nano/internal/transcriptparse/claude"
	parsecodex "github.com/dkroshow/nano/internal/transcriptparse/codex"
	parsegrok "github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestParsedJSONLMetadataMatchesExistingCatalogFields(t *testing.T) {
	for _, tc := range []struct {
		provider Provider
		file     string
	}{
		{ProviderClaude, "../transcriptparse/claude/testdata/catalog.jsonl"},
		{ProviderClaude, "../transcriptparse/claude/testdata/research/capture-row-started/input.jsonl"},
		{ProviderClaude, "../transcriptparse/claude/testdata/research/capture-row-result/input.jsonl"},
		{ProviderCodex, "../transcriptparse/codex/testdata/catalog.jsonl"},
		{ProviderClaude, "testdata/claude/33333333-3333-4333-8333-333333333333.jsonl"},
		{ProviderCodex, "testdata/codex/rollout-2026-07-09T12-00-00-11111111-1111-4111-8111-111111111111.jsonl"},
		{ProviderCodex, "testdata/codex/guardian-recent.jsonl"},
		{ProviderCodex, "testdata/codex/guardian-parentless.jsonl"},
		{ProviderCodex, "testdata/transcript/codex-recommended-plugins-opener.jsonl"},
	} {
		provider := tc.provider
		t.Run(tc.file, func(t *testing.T) {
			raw, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			for _, wire := range []bool{false, true} {
				if wire && provider == ProviderCodex {
					continue
				}
				source := SourceCandidate{Root: SourceRoot{Provider: provider}, Path: "/fixture/catalog.jsonl", Kind: ThreadMain}
				state := newTranscriptMetadataState(provider, source, nil)
				var claudeState parseclaude.State
				var codexState parsecodex.State
				var prefix []byte
				for line, row := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
					prefix = append(append(prefix, row...), '\n')
					input := model.Input{Bytes: append(append([]byte(nil), row...), '\n'), Source: model.SourceRef{Label: "catalog", Incarnation: "1"}}
					var batch model.Batch
					var old ProjectionResult
					if provider == ProviderClaude {
						if wire {
							claudeState, batch, err = parseclaude.ParseWire(claudeState, input)
						} else {
							claudeState, batch, err = parseclaude.ParseFile(claudeState, input)
						}
						if err != nil {
							t.Fatal(err)
						}
						old, err = catalogFixtureOracle(t, ProviderClaude, ProjectionRequest{Source: source, Reader: bytes.NewReader(prefix)})
					} else {
						codexState, batch, err = parsecodex.ParseFile(codexState, input)
						if err != nil {
							t.Fatal(err)
						}
						old, err = catalogFixtureOracle(t, ProviderCodex, ProjectionRequest{Source: source, Reader: bytes.NewReader(prefix)})
					}
					if err != nil {
						t.Fatal(err)
					}
					for _, obs := range batch.Observations {
						applyTranscriptMetadata(&state, obs)
					}
					got, want := state.projection, old.Projection
					if got.UpdatedAt == nil {
						got.UpdatedAt = cloneTime(state.lastActivity)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("line %d wire=%t catalog differs: got %+v want %+v", line+1, wire, got, want)
					}
				}
			}
		})
	}
}

func TestParsedGrokMetadataMatchesExistingCatalogFields(t *testing.T) {
	fixture, err := os.ReadFile("../transcriptparse/grok/testdata/research/capture-summary/input.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{fixture, []byte(`{"info":{"id":"id"},"created_at":"2026-01-01T00:00:00-07:00","updated_at":" 2026-01-02T00:00:00-07:00 ","current_model_id":"model"}`), []byte(`{"info":{"id":" id ","cwd":" /workspace "},"updated_at":"invalid","last_active_at":"2026-01-02T00:00:00Z","current_model_id":" model ","reasoning_effort":" high ","sandbox_profile":" off ","generated_title":" A title ","session_summary":"First\n second"}`)} {
		source := SourceCandidate{Root: SourceRoot{Provider: ProviderGrok}, Path: "/fixture/summary.json", Kind: ThreadMain}
		old, err := catalogFixtureOracle(t, ProviderGrok, ProjectionRequest{Source: source, Reader: bytes.NewReader(raw)})
		if err != nil {
			t.Fatal(err)
		}
		_, batch, err := parsegrok.ParseFile(parsegrok.State{}, model.Input{Format: "json", Closed: true, Bytes: raw, Source: model.SourceRef{Label: "summary", Incarnation: "1"}})
		if err != nil {
			t.Fatal(err)
		}
		state := newTranscriptMetadataState(ProviderGrok, source, nil)
		for _, obs := range batch.Observations {
			applyTranscriptMetadata(&state, obs)
		}
		if !reflect.DeepEqual(state.projection, old.Projection) {
			t.Fatalf("typed catalog fields differ: got %+v want %+v", state.projection, old.Projection)
		}
	}
}

func TestParsedGrokCatalogKeepsDiscoveredNativeParent(t *testing.T) {
	raw := []byte(`{"info":{"id":"child","cwd":"/child"},"updated_at":"2026-01-01T00:00:00Z"}`)
	source := SourceCandidate{Root: SourceRoot{Provider: ProviderGrok}, Path: "/fixture/summary.json", Kind: ThreadSubagent, ParentNativeID: "parent"}
	old, err := catalogFixtureOracle(t, ProviderGrok, ProjectionRequest{Source: source, Reader: bytes.NewReader(raw)})
	if err != nil {
		t.Fatal(err)
	}
	_, batch, err := parsegrok.ParseFile(parsegrok.State{}, model.Input{Format: "json", Closed: true, Bytes: raw, Source: model.SourceRef{Label: "summary", Incarnation: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	state := newTranscriptMetadataState(ProviderGrok, source, nil)
	for _, obs := range batch.Observations {
		applyTranscriptMetadata(&state, obs)
	}
	if !reflect.DeepEqual(state.projection, old.Projection) {
		t.Fatal("summary discarded native parent context from its source")
	}
}

func TestMetadataReducerKeepsAvailabilityAndNativeClockSeparate(t *testing.T) {
	clock := time.Unix(100, 0)
	obs := model.Observation{Metadata: model.Metadata{
		model.MetadataModel:    {State: model.MetadataKnown, Value: "model"},
		model.MetadataPriority: {State: model.MetadataUnsupported},
		model.MetadataEffort:   {State: model.MetadataUnknown},
	}, Times: []model.TimeObservation{{Class: model.TimeCapture, Meaning: model.TimeWrite, Value: &clock}}}
	for _, provider := range []Provider{ProviderClaude, ProviderCodex, ProviderGrok} {
		state := projectionState{projection: initialProjection(provider, SourceCandidate{}, nil)}
		applyTranscriptMetadata(&state, obs)
		fields := state.projection.Metadata
		if fields.Model.State != MetadataKnown || fields.Model.Value == nil || *fields.Model.Value != "model" || fields.Model.ObservedAt != nil || fields.Priority.State != MetadataUnsupported || fields.Effort.State != MetadataUnknown {
			t.Fatal("metadata availability or native time depended on provider or capture clock")
		}
	}
}

func TestParsedClaudeCatalogSelectsSourceWithoutNativeDecoding(t *testing.T) {
	for _, tc := range []struct {
		name, path, raw string
		kind            ThreadKind
	}{
		{"root_with_worker", "/fixture/root.jsonl", `{"type":"user","sessionId":"root","timestamp":"2026-01-01T00:00:00Z","cwd":"/root","message":{"role":"user","content":"Root prompt"}}
{"type":"assistant","sessionId":"root","agentId":"worker","isSidechain":true,"timestamp":"2026-01-01T02:00:00Z","cwd":"/child","message":{"role":"assistant","model":"child-model","content":"Child answer"}}
{"type":"assistant","sessionId":"root","isSidechain":true,"timestamp":"2026-01-01T03:00:00Z","cwd":"/unknown-child","message":{"role":"assistant","model":"other-child","content":"Unidentified child"}}
{"type":"assistant","sessionId":"root","timestamp":"2026-01-01T01:00:00Z","message":{"role":"assistant","model":"root-model","content":"Root answer"}}`, ThreadMain},
		{"worker_with_mismatched_agent", "/fixture/subagents/agent-worker.jsonl", `{"type":"assistant","sessionId":"root","agentId":"intruder","isSidechain":true,"timestamp":"2026-01-01T03:00:00Z","cwd":"/intruder","message":{"role":"assistant","model":"wrong-model","content":"Wrong source"}}
{"type":"assistant","sessionId":"root","agentId":"worker","isSidechain":true,"timestamp":"2026-01-01T01:00:00Z","cwd":"/worker","message":{"role":"assistant","model":"child-model","content":"Child answer"}}`, ThreadSubagent},
		{"root_identity_conflict", "/fixture/root.jsonl", `{"type":"user","sessionId":"root","timestamp":"2026-01-01T00:00:00Z","cwd":"/root","message":{"role":"user","content":"Root prompt"}}
{"type":"custom-title","sessionId":"intruder","timestamp":"2026-01-01T03:00:00Z","cwd":"/intruder","customTitle":"Wrong title"}`, ThreadMain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := SourceCandidate{Root: SourceRoot{Provider: ProviderClaude}, Path: tc.path, Kind: tc.kind}
			old, err := catalogFixtureOracle(t, ProviderClaude, ProjectionRequest{Source: source, Reader: bytes.NewBufferString(tc.raw + "\n")})
			if err != nil {
				t.Fatal(err)
			}
			for _, wire := range []bool{false, true} {
				var reader parseclaude.State
				state := projectionState{projection: initialProjection(ProviderClaude, source, nil), source: source}
				for _, row := range bytes.Split([]byte(tc.raw), []byte("\n")) {
					input := model.Input{Bytes: append(append([]byte(nil), row...), '\n'), Source: model.SourceRef{Label: "catalog", Incarnation: "1"}}
					var batch model.Batch
					if wire {
						reader, batch, err = parseclaude.ParseWire(reader, input)
					} else {
						reader, batch, err = parseclaude.ParseFile(reader, input)
					}
					if err != nil {
						t.Fatal(err)
					}
					for _, obs := range batch.Observations {
						applyTranscriptMetadata(&state, obs)
					}
				}
				if state.projection.UpdatedAt == nil {
					state.projection.UpdatedAt = state.lastActivity
				}
				if !reflect.DeepEqual(state.projection, old.Projection) {
					t.Fatalf("wire=%t scope selection differs: got %+v want %+v", wire, state.projection, old.Projection)
				}
			}
		})
	}
}

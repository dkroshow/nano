package history

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	parsecodex "github.com/dkroshow/nano/internal/transcriptparse/codex"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Failure observed live: 95 prompts steered into one Codex turn of 290 user
// rows. The live-text match loaded every row of the turn once per prompt, on
// every tick, and the server stalled. Boundary: the statements one
// ReadTranscripts match issues, over rows the Codex reader parsed and the
// store wrote as a catalog commit does. Each prompt must still match its own
// row, and the work must not grow with the number of prompts in the turn.
func TestTurnEventMatchLoadsACrowdedTurnOnce(t *testing.T) {
	recorder := registerRecordingDriver(t)
	ctx := context.Background()
	store, err := openStore(recordingDriverName, filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: t.TempDir()}
	metadata := testSource(root, "crowded.jsonl", "crowded", ThreadMain, nil)
	thread, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil {
		t.Fatal(err)
	}
	const prompts = 24
	var file strings.Builder
	file.WriteString(`{"type":"session_meta","payload":{"id":"crowded"}}` + "\n")
	file.WriteString(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-crowded"}}` + "\n")
	for i := 0; i < prompts; i++ {
		fmt.Fprintf(&file, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"steered prompt %d\",\"client_id\":\"steered-event-%d\"}}\n", i, i)
	}
	_, batch, err := parsecodex.ParseFile(parsecodex.State{}, model.Input{Bytes: []byte(file.String()), Source: model.SourceRef{Label: "fixture", Incarnation: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: thread.ThreadID, SourceID: source.ID, SourceGeneration: source.Generation,
		Provider: ProviderCodex, ParserVersion: CurrentTranscriptParserVersion(ProviderCodex), CompletionRuleVersion: batch.Versions.Assembly,
		Result: TranscriptProjectionResult{ProjectionKey: "crowded", CompleteOffset: int64(file.Len()), CompleteLines: len(batch.Records), ParserState: []byte(`{}`), recordSource: "fixture",
			parsedBatches: func(yield func(model.Batch) error) error { return yield(batch) }}}); err != nil {
		t.Fatal(err)
	}
	match := func(count int) (map[string]string, int) {
		turns := map[string]string{}
		var keys []string
		for i := 0; i < count; i++ {
			key := fmt.Sprintf("steered-event-%d", i)
			keys, turns[key] = append(keys, key), "turn-crowded"
		}
		recorder.reset()
		_, found, err := store.ReadTranscripts(ctx, thread.ThreadID, nil, []TranscriptMatch{{Namespace: MatchTurnEvent, ItemType: "user_message", Keys: keys, Turns: turns}})
		if err != nil {
			t.Fatal(err)
		}
		return found, len(recorder.statements())
	}
	_, one := match(1)
	found, all := match(prompts)
	rows := map[string]bool{}
	for _, id := range found {
		rows[id] = true
	}
	if len(found) != prompts || len(rows) != prompts {
		t.Fatalf("%d prompts matched %d distinct rows of %d", len(found), len(rows), prompts)
	}
	if all != one {
		t.Fatalf("matching %d prompts of one turn issued %d statements, one prompt %d: the turn was loaded more than once", prompts, all, one)
	}
}

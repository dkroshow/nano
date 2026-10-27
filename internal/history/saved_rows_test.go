package history

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/claude"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Failure caught in review: a Claude worker's turn, saved as sidechain rows in
// the root file while the root turn still runs, counted as the later turn that
// proves the root turn was cut off; so did its housekeeping rows, which are
// stored without the sidechain flag. The running root prompt then read
// interrupted, and its operation could not take the answer that came later.
// Boundary: the turn status the saved user-row read reports, over rows the
// Claude reader parsed and the store wrote as a catalog commit does.
func TestClaudeOpenTurnIsCutOffOnlyByALaterRootTurn(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: t.TempDir()}
	metadata := testSource(root, "root.jsonl", "root", ThreadMain, nil)
	thread, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil {
		t.Fatal(err)
	}
	running := `{"type":"user","uuid":"root-prompt","promptId":"root-turn","sessionId":"root","message":{"role":"user","content":[{"type":"text","text":"Root prompt"}]}}
{"type":"assistant","uuid":"root-call","sessionId":"root","message":{"id":"root-message","role":"assistant","content":[{"type":"tool_use","id":"task-call","name":"Task","input":{}}],"stop_reason":"tool_use"}}
{"type":"user","uuid":"worker-prompt","sessionId":"root","agentId":"worker","isSidechain":true,"message":{"role":"user","content":[{"type":"text","text":"Worker prompt"}]}}
{"type":"assistant","uuid":"worker-answer","sessionId":"root","agentId":"worker","isSidechain":true,"message":{"id":"worker-message","role":"assistant","content":[{"type":"text","text":"Worker answer"}],"stop_reason":"end_turn"}}
{"type":"attachment","uuid":"worker-housekeeping","sessionId":"root","agentId":"worker","isSidechain":true,"attachment":{"type":"total_tokens_reminder"}}
`
	restarted := running + `{"type":"user","uuid":"next-prompt","promptId":"next-turn","sessionId":"root","message":{"role":"user","content":[{"type":"text","text":"Next prompt"}]}}
`
	rootStatus := func(rows, key string) TranscriptTurnStatus {
		_, batch, err := claude.ParseFile(claude.State{}, model.Input{Bytes: []byte(rows), Source: model.SourceRef{Label: "fixture", Incarnation: "1"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: thread.ThreadID, SourceID: source.ID, SourceGeneration: source.Generation,
			Provider: ProviderClaude, ParserVersion: CurrentTranscriptParserVersion(ProviderClaude), CompletionRuleVersion: batch.Versions.Assembly,
			Result: TranscriptProjectionResult{ProjectionKey: key, CompleteOffset: int64(len(rows)), CompleteLines: len(batch.Records), ParserState: []byte(`{}`), recordSource: "fixture",
				parsedBatches: func(yield func(model.Batch) error) error { return yield(batch) }}}); err != nil {
			t.Fatal(err)
		}
		saved, err := store.ReadSavedUserRows(ctx, thread.ThreadID, SavedUserKeys{NativeEventIDs: []string{"root-prompt"}})
		if err != nil || len(saved) != 1 {
			t.Fatalf("root prompt rows = %+v, %v", saved, err)
		}
		return saved[0].TurnStatus
	}
	if got := rootStatus(running, "running"); got == TurnInterrupted {
		t.Fatalf("root turn followed only by a worker's turn = %s; it may still be running", got)
	}
	if got := rootStatus(restarted, "restarted"); got != TurnInterrupted {
		t.Fatalf("root turn a later root turn follows = %s; want %s", got, TurnInterrupted)
	}
}

// Attach reads saved user rows by exact key before it publishes prompts. A
// plain join let SQLite scan every item of the projection instead: 21 s for
// one 60,000-item thread. Each lookup must start from its exact-key index.
// Boundary: the SQL plan of the saved user-row lookups.
// A decided saved turn is read back only at the generation that decided it:
// its user rows carry that projection key, more rows than the read's bound are
// refused rather than truncated, its own final row names its native message,
// and another generation or a turn with no final row is refused rather than
// answered from elsewhere.
func TestDecidedSavedTurnReadsHoldTheirGeneration(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "decided")
	ctx := context.Background()
	var threadID, nativeThreadID string
	if err := store.db.QueryRow(`SELECT thread_id FROM conversation_projections WHERE id=?`, projection).Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT native_thread_id FROM threads WHERE id=?`, threadID).Scan(&nativeThreadID); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	turn := TranscriptTurn{Key: "decided-turn", Ordinal: 1, Status: TurnCompleted, CompletionEvidence: CompletionStopReason}
	if err := writeTranscriptBatch(ctx, tx, projection, TranscriptProjectionResult{Turns: []TranscriptTurn{turn}}); err != nil {
		t.Fatal(err)
	}
	row := func(id string, seq int64, itemType model.ItemType, role model.Role, text string) model.Item {
		return model.Item{
			ID: id, SourceSeq: seq, TurnKey: turn.Key, TurnOrdinal: 1,
			Scope: model.Scope{Kind: model.ScopeItem, Provider: model.ProviderClaude, Source: "fixture", Incarnation: "parser", TurnKey: turn.Key, ItemID: id},
			Type:  itemType, Category: model.CategoryMessages, Status: model.ItemStatusCompleted, Role: role, Text: text, Complete: true,
		}
	}
	for i, id := range []string{"prompt-row", "absorbed-row"} {
		if _, _, err := writeParsedTranscriptItemTx(ctx, tx, projection, nativeThreadID, turn, row(id, int64(i+1), model.ItemUserMessage, model.RoleUser, id+" text")); err != nil {
			t.Fatal(err)
		}
	}
	answer, _, err := writeParsedTranscriptItemTx(ctx, tx, projection, nativeThreadID, turn, row("answer-row", 3, model.ItemAgentMessage, model.RoleAssistant, "the answer"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE conversation_turns SET final_item_id=? WHERE projection_id=? AND turn_key=?`, answer.ID, projection, turn.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE conversation_items SET native_item_id='msg_answer:0' WHERE projection_id=? AND item_id=?`, projection, answer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE conversation_projections SET item_count=3,latest_seq=? WHERE id=?`, answer.ProjectedSeq, projection); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// An older reader grouped Claude's saved turns differently; its turns
	// decide no answer until the upgrade backfill reads the file again.
	if _, err := store.ReadSavedUserRows(ctx, threadID, SavedUserKeys{TurnKeys: []string{turn.Key}, ProjectionKey: "decided", MaxRows: 2}); !errors.Is(err, ErrSavedReaderOutdated) {
		t.Fatalf("an older reader's projection = %v; want ErrSavedReaderOutdated", err)
	}
	if _, err := store.db.Exec(`UPDATE conversation_projections SET parser_version=? WHERE id=?`, CurrentTranscriptParserVersion(ProviderClaude), projection); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ReadSavedUserRows(ctx, threadID, SavedUserKeys{TurnKeys: []string{turn.Key}, ProjectionKey: "decided", MaxRows: 2})
	if err != nil || len(rows) != 2 || rows[0].Item.Type != model.ItemUserMessage || rows[0].ProjectionKey != "decided" || rows[0].TurnStatus != TurnCompleted {
		t.Fatalf("decided turn rows = %+v, %v; want its two user rows at generation decided", rows, err)
	}
	if _, err := store.ReadSavedUserRows(ctx, threadID, SavedUserKeys{TurnKeys: []string{turn.Key}, ProjectionKey: "decided", MaxRows: 1}); !errors.Is(err, ErrSavedRowsOverflow) {
		t.Fatalf("a turn over the bound = %v; want ErrSavedRowsOverflow, not a truncated read", err)
	}
	if _, err := store.ReadSavedUserRows(ctx, threadID, SavedUserKeys{TurnKeys: []string{turn.Key}, ProjectionKey: "older"}); !errors.Is(err, ErrSavedGenerationChanged) {
		t.Fatalf("another generation's read = %v; want ErrSavedGenerationChanged", err)
	}
	final, err := store.ReadSavedTurnFinal(ctx, threadID, "decided", turn.Key)
	if err != nil || final.NativeItemID != "msg_answer:0" || !strings.Contains(final.Text, "the answer") || final.ItemID != answer.ID {
		t.Fatalf("final = %+v, %v", final, err)
	}
	if _, err := store.ReadSavedTurnFinal(ctx, threadID, "older", turn.Key); !errors.Is(err, ErrSavedGenerationChanged) {
		t.Fatalf("another generation's final = %v; want ErrSavedGenerationChanged", err)
	}
	if _, err := store.ReadSavedTurnFinal(ctx, threadID, "decided", "missing-turn"); !errors.Is(err, ErrTranscriptBodyUnavailable) {
		t.Fatalf("a turn without a final row = %v; want ErrTranscriptBodyUnavailable", err)
	}
}

func TestSavedUserRowLookupsStartFromTheirExactKey(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for query, index := range map[string]string{
		savedUserRowsByTurn: "conversation_turns_native", savedUserRowsByEvent: "conversation_observation_aliases",
		// A decided saved turn's rows: its turn by primary key, then its items.
		savedUserRowsByTurnKey: "conversation_turns",
	} {
		rows, err := store.db.Query(`EXPLAIN QUERY PLAN `+query, 1, "key")
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		if len(plan) == 0 || !strings.Contains(plan[0], index) {
			t.Fatalf("lookup does not start from %s: %v", index, plan)
		}
		for _, step := range plan {
			if strings.HasPrefix(step, "SCAN") {
				t.Fatalf("lookup scans a table: %v", plan)
			}
		}
	}
}

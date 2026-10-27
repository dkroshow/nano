package history

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// A live turn whose terminal event was lost settles only from exact saved
// evidence: the one saved turn with its native id, with its status as the
// committed prefix shows it, and whether a later conversation turn exists.
// Boundary: ReadSavedTurnStatus over a real two-turn projection.
func TestReadSavedTurnStatusGivesExactEvidenceOnly(t *testing.T) {
	for _, tc := range []struct {
		name, turnID, statement string
		ok, newer               bool
		status                  TranscriptTurnStatus
	}{
		{name: "earlier turn", turnID: "exact-turn", ok: true, newer: true, status: TurnCompleted},
		{name: "newest turn", turnID: "other-turn", ok: true, newer: false, status: TurnCompleted},
		{name: "missing", turnID: "missing"},
		// Duplicate saved ids, such as Codex saved twins, are not exact.
		{name: "twins", turnID: "exact-turn", statement: "UPDATE conversation_turns SET native_turn_id='exact-turn'"},
		{name: "completion not committed", turnID: "exact-turn", ok: true, newer: true, status: TurnInProgress,
			statement: "UPDATE conversation_turns SET completion_seq=NULL WHERE native_turn_id='exact-turn'"},
		// A later turn of only notices is not a conversation turn.
		{name: "later notice only", turnID: "exact-turn", ok: true, newer: false, status: TurnCompleted,
			statement: "UPDATE conversation_items SET item_type='notice' WHERE turn_key=(SELECT turn_key FROM conversation_turns WHERE native_turn_id='other-turn')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, threadID := savedTurnStatusFixture(t)
			if tc.statement != "" {
				if _, err := store.db.Exec(tc.statement); err != nil {
					t.Fatal(err)
				}
			}
			got, ok, err := store.ReadSavedTurnStatus(context.Background(), threadID, tc.turnID)
			if err != nil || ok != tc.ok || got.Newer != tc.newer || (ok && (got.Turn.NativeTurnID != tc.turnID || got.Turn.Status != tc.status)) {
				t.Fatalf("status = %+v ok=%v err=%v; want ok=%v newer=%v status=%q", got, ok, err, tc.ok, tc.newer, tc.status)
			}
		})
	}
}

// The read runs on every catalog change while a turn is open, so it must stay
// two indexed lookups however long the thread is. Boundary: the query plans.
func TestReadSavedTurnStatusSearchesByIndexOnly(t *testing.T) {
	store, _ := savedTurnStatusFixture(t)
	for query, search := range map[string]string{
		savedTurnByNativeIDQuery: "USING INDEX conversation_turns_native",
		newerSavedTurnQuery:      "(projection_id=? AND ordinal>?)",
	} {
		rows, err := store.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, 1, "exact-turn")
		if err != nil {
			t.Fatal(err)
		}
		var steps []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			steps = append(steps, detail)
		}
		rows.Close()
		joined := strings.Join(steps, "; ")
		if !strings.Contains(joined, search) || strings.Contains(strings.ReplaceAll(joined, "SCAN CONSTANT ROW", ""), "SCAN ") {
			t.Fatalf("turn-status query plan = %s; want %q and no table scan\n%s", joined, search, query)
		}
	}
}

func savedTurnStatusFixture(t *testing.T) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: t.TempDir()}
	metadata := testSource(root, "thread.jsonl", "native-thread", ThreadMain, nil)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil {
		t.Fatal(err)
	}
	adapter := newTranscriptFixture(t, ProviderCodex)
	result, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{Source: metadata.Source, Reader: strings.NewReader(codexTurnsJSONL(1, 2))})
	if err != nil {
		t.Fatal(err)
	}
	result.Turns[0].NativeTurnID = "exact-turn"
	result.Turns[1].NativeTurnID = "other-turn"
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation, Provider: ProviderCodex, ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(), Result: result}); err != nil {
		t.Fatal(err)
	}
	return store, applied.ThreadID
}

// Grok records a backgrounded shell task without a prompt id, so its row lands
// in a history-only group after the running turn. That group is not a newer
// turn: treating it as one settled a working Grok turn as finished mid-turn. A
// next prompt saved before its id binds is a newer turn, so a lost terminal
// still settles. Boundary: ReadSavedTurnStatus over a scrubbed real Grok file.
func TestReadSavedTurnStatusIgnoresGrokBackgroundTaskGroup(t *testing.T) {
	raw, err := os.ReadFile("testdata/grok/background-task-mid-turn.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	nextPrompt := `{"timestamp":1790898990,"method":"session/update","params":{"sessionId":"grok-bg-session","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"scrubbed text"},"_meta":{"modelId":"grok-4.7","promptIndex":1}},"_meta":{"eventId":"grok-bg-session-1278600","agentTimestampMs":1790898990000}}}` + "\n"
	for _, tc := range []struct {
		name, extra string
		newer       bool
	}{
		{name: "background task mid-turn"},
		{name: "next prompt before its id", extra: nextPrompt, newer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			projection := identityStoreProjection(t, store, "grok-background-task")
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var state grok.State
			for _, frame := range bytes.SplitAfter(append(append([]byte(nil), raw...), tc.extra...), []byte{'\n'}) {
				var batch model.Batch
				state, batch, err = grok.ParseFile(state, model.Input{Bytes: frame, Source: model.SourceRef{Label: "fixture", Incarnation: "1"}})
				if err != nil {
					t.Fatal(err)
				}
				if err := writeParsedTranscriptBatchTx(ctx, tx, projection, "grok-bg-session", batch); err != nil {
					t.Fatal(err)
				}
			}
			var groups int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM conversation_turns t WHERE t.projection_id=? AND t.parsed_json='{}' AND EXISTS (SELECT 1 FROM conversation_items i WHERE i.projection_id=t.projection_id AND i.turn_key=t.turn_key AND i.item_type='command')`, projection).Scan(&groups); err != nil || groups != 1 {
				t.Fatalf("fixture no longer puts the task in a history-only group: groups=%d err=%v", groups, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			var threadID string
			if err := store.db.QueryRow(`SELECT thread_id FROM conversation_projections WHERE id=?`, projection).Scan(&threadID); err != nil {
				t.Fatal(err)
			}
			got, ok, err := store.ReadSavedTurnStatus(ctx, threadID, "bg-prompt")
			if err != nil || !ok || got.Newer != tc.newer || got.Turn.Status != TurnInProgress {
				t.Fatalf("status = %+v ok=%v err=%v; want the running turn with newer=%v", got, ok, err, tc.newer)
			}
		})
	}
}

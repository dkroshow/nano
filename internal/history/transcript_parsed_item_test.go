package history

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestParsedTranscriptReplayKeepsOneRowAndItsFirstOwner(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "parsed-items")
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	firstTurn := TranscriptTurn{Key: "first-view-turn", Ordinal: 1, Status: TurnCompleted, CompletionEvidence: CompletionStopReason}
	nextTurn := TranscriptTurn{Key: "next-view-turn", Ordinal: 2, Status: TurnInProgress, CompletionEvidence: CompletionNone}
	if err := writeTranscriptBatch(ctx, tx, projection, TranscriptProjectionResult{Turns: []TranscriptTurn{firstTurn, nextTurn}}); err != nil {
		t.Fatal(err)
	}
	original := identityTestItem()
	original.ID = "first-local"
	original.Scope.ItemID = original.ID
	original.Scope.TurnKey = "first-source-turn"
	original.TurnKey = original.Scope.TurnKey
	original.NativeItemID = ""
	original.CallID = ""
	original.Type = model.ItemToolResult
	original.Role = model.RoleTool
	original.Text = "Original result"
	original.SourceLine = 988
	original.SourceLineSHA256 = strings.Repeat("a", 64)
	original.Status = model.ItemStatusCompleted
	original.Observations[0].ID = "first-observation"
	original.Observations[0].Text = original.Text
	original.Observations[0].Scope = original.Scope
	first, _, err := writeParsedTranscriptItemTx(ctx, tx, projection, "native-thread", firstTurn, original)
	if err != nil {
		t.Fatal(err)
	}
	late := original
	late.ID = "second-local"
	late.Scope.ItemID = late.ID
	late.Scope.TurnKey = "second-source-turn"
	late.TurnKey = late.Scope.TurnKey
	late.SourceLine = 1229
	late.SourceLineSHA256 = strings.Repeat("b", 64)
	late.Observations = append([]model.Observation(nil), original.Observations...)
	late.Observations[0].ID = "second-observation"
	late.Observations[0].Scope = late.Scope
	second, _, err := writeParsedTranscriptItemTx(ctx, tx, projection, "native-thread", nextTurn, late)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || second.ID != first.ID || first.ProjectedSeq != 1 || second.ProjectedSeq != 1 || second.TurnKey != firstTurn.Key || second.SourceLine != 988 || second.SourceLineSHA256 != original.SourceLineSHA256 || second.Text != original.Text {
		t.Fatalf("late replay moved or replaced its row: first=%+v second=%+v", first, second)
	}
	var count int
	for _, tc := range []struct {
		query string
		want  int
	}{
		{`SELECT COUNT(*) FROM conversation_items WHERE projection_id=?`, 1},
		{`SELECT COUNT(*) FROM conversation_item_observations WHERE projection_id=?`, 2},
		{`SELECT COUNT(*) FROM conversation_relations WHERE projection_id=?`, 1},
		{`SELECT COUNT(*) FROM conversation_parse_identities WHERE projection_id=? AND identity_kind='local'`, 2},
	} {
		if err := tx.QueryRow(tc.query, projection).Scan(&count); err != nil || count != tc.want {
			t.Fatalf("persisted replay count=%d want=%d err=%v", count, tc.want, err)
		}
	}
	var raw []byte
	if err := tx.QueryRow(`SELECT parsed_json FROM conversation_items WHERE projection_id=? AND item_id=?`, projection, first.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	saved, err := decodeParsedItemTx(ctx, tx, projection, raw)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID != original.ID || saved.Text != "" || len(saved.Observations) != 0 {
		t.Fatal("current item duplicated evidence or lost its initial local identity")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestParsedTranscriptSnapshotRevisionsRefreshBodiesAndSearch(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "revisions")
	ctx := context.Background()
	turn := TranscriptTurn{Key: "view-turn", Ordinal: 1, Status: TurnInProgress, CompletionEvidence: CompletionNone}
	item := identityTestItem()
	item.ID = "local"
	item.Scope.ItemID = item.ID
	item.Scope.TurnKey = "source-turn"
	item.TurnKey = item.Scope.TurnKey
	item.NativeItemID = "message:0"
	item.CallID = ""
	item.Type = model.ItemUserMessage
	item.Role = model.RoleUser
	item.Category = model.CategoryMessages
	item.Status = model.ItemStatusCompleted
	item.Text = strings.Repeat("obsoletebody ", 2000)
	item.SourceLine = 1
	item.SourceRowType = "user"
	item.SourceLineSHA256 = strings.Repeat("a", 64)
	item.Observations[0].ID = "obs-first"
	item.Observations[0].Scope = item.Scope
	item.Observations[0].Text = item.Text
	apply := func(value model.Item) TranscriptItem {
		t.Helper()
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := writeTranscriptBatch(ctx, tx, projection, TranscriptProjectionResult{Turns: []TranscriptTurn{turn}}); err != nil {
			t.Fatal(err)
		}
		got, _, err := writeParsedTranscriptItemTx(ctx, tx, projection, "native-thread", turn, value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`UPDATE conversation_projections SET item_count=1,latest_seq=1 WHERE id=?`, projection); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := apply(item)
	if first.ContentRef == "" {
		t.Fatal("fixture must require a retained body")
	}
	buildSearchModes(t, ctx, store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 50)
	checkSearch := func(term string, want int) {
		t.Helper()
		for _, mode := range []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms} {
			table := searchFTSTable(mode)
			var count int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+table+` MATCH ?`, term).Scan(&count); err != nil || count != want {
				t.Fatalf("%s term %s: count=%d want=%d err=%v", mode, term, count, want, err)
			}
		}
	}
	checkSearch("obsoletebody", 1)
	changed := item
	changed.Text = strings.Repeat("freshnewbody ", 2000)
	next := item.Observations[0]
	next.ID = "obs-second"
	next.NativeEventID = "second-event"
	next.NativeEventDigest = "second-digest"
	next.Text = changed.Text
	changed.Observations = append(append([]model.Observation(nil), item.Observations...), next)
	second := apply(changed)
	if first.ID != second.ID || second.ProjectedSeq != 1 || second.ContentRef == first.ContentRef {
		t.Fatal("same-length snapshot kept a stale body reference or changed row identity")
	}
	var body string
	if err := store.db.QueryRow(`SELECT body_text FROM conversation_item_bodies WHERE projection_id=? AND item_id=?`, projection, second.ID).Scan(&body); err != nil || body != strings.TrimSpace(changed.Text) {
		t.Fatal("retained body did not follow snapshot replacement")
	}
	checkSearch("obsoletebody", 0)
	checkSearch("freshnewbody", 1)
	short := changed
	short.Text = "shortreplacement"
	next.ID = "obs-third"
	next.NativeEventID = "third-event"
	next.NativeEventDigest = "third-digest"
	next.Text = short.Text
	short.Observations = append(append([]model.Observation(nil), changed.Observations...), next)
	third := apply(short)
	if third.ContentRef != "" {
		t.Fatal("shorter snapshot retained an obsolete body reference")
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_item_bodies WHERE projection_id=?`, projection).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale retained body count=%d err=%v", count, err)
	}
	checkSearch("freshnewbody", 0)
	checkSearch("shortreplacement", 1)
	// Fail after revising the view, body and search document. The caller's
	// transaction must roll back those changes and the newly added identity.
	broken := short
	broken.Text = strings.Repeat("rollbackbody ", 2000)
	badObservation := next
	badObservation.Source.Start++
	newObservation := next
	newObservation.ID, newObservation.NativeEventID = "rollback-observation", "rollback-event"
	broken.Observations = []model.Observation{newObservation, badObservation}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, writeErr := writeParsedTranscriptItemTx(ctx, tx, projection, "native-thread", turn, broken)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(writeErr, ErrTranscriptConflict) {
		t.Fatalf("changed observation accepted: %v", writeErr)
	}
	var text string
	var private []byte
	if err := store.db.QueryRow(`SELECT display_text,parsed_json FROM conversation_items WHERE projection_id=? AND item_id=?`, projection, third.ID).Scan(&text, &private); err != nil || text != short.Text || strings.Contains(string(private), short.Text) {
		t.Fatalf("failed revision changed stored text: %v", err)
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM conversation_item_bodies WHERE projection_id=?`,
		`SELECT COUNT(*) FROM conversation_observations o JOIN conversation_parse_values k ON k.id=o.key_id WHERE o.projection_id=? AND k.value_key=CAST(char(1)||'J"rollback-observation"' AS BLOB)`,
		`SELECT COUNT(*) FROM conversation_parse_identities a JOIN conversation_parse_values v ON v.id=a.observation_value_id WHERE a.projection_id=? AND v.value_key=CAST(char(1)||'J"rollback-observation"' AS BLOB)`,
	} {
		if err := store.db.QueryRow(query, projection).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed revision left body or evidence: count=%d err=%v", count, err)
		}
	}
	checkSearch("shortreplacement", 1)
	checkSearch("rollbackbody", 0)
	hidden := short
	hidden.Synthetic = true
	hidden.Type, hidden.Role = model.ItemNotice, model.RoleSystem
	apply(hidden)
	checkSearch("shortreplacement", 0)
}

package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestTranscriptEvidenceIsImmutableAndIdempotent(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "evidence")
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	observation := model.Observation{ID: "evidence-one", Scope: model.Scope{Provider: model.ProviderClaude, Source: "source", Incarnation: "1"}, Kind: model.ObservationSnapshot, Text: "  native text\n", Capture: model.CaptureComplete, NativeEventID: "uuid", NativeEventDigest: "digest", Detail: []byte(`{"opaque":null}`), Times: []model.TimeObservation{{Class: model.TimeNative, Meaning: model.TimeWrite, Raw: []byte(`null`), Value: nil}}}
	observation.Text += strings.Repeat("native payload", 10000)
	observation.Source = model.SourceRef{Kind: "jsonl", Label: "fixture", Incarnation: "1", Line: 4, Start: 80, End: 140080, BlockPath: "/message/content/0", SHA256: strings.Repeat("a", 64)}
	at := time.Date(2026, 9, 10, 8, 0, 1, 120000000, time.UTC)
	observation.Times = append(observation.Times,
		model.TimeObservation{Scope: observation.Scope, ObservationRef: observation.ID, Class: model.TimeNative, Meaning: model.TimeStart},
		model.TimeObservation{Scope: observation.Scope, ObservationRef: observation.ID, Class: model.TimeNative, Meaning: model.TimeWrite, Value: &at, Raw: []byte(`"2026-09-10T08:00:01.12Z"`)},
		model.TimeObservation{Scope: observation.Scope, ObservationRef: "other", Class: model.TimeCapture, Meaning: model.TimeEnd, Value: &at, Raw: []byte(`"2026-09-10T08:00:01.120000Z"`)})
	for i := 0; i < 2; i++ {
		if err := writeTranscriptObservationTx(ctx, tx, projection, observation); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`UPDATE conversation_observations SET sha256=zeroblob(32) WHERE projection_id=?`, projection); err != nil {
		t.Fatal(err)
	}
	if err := writeTranscriptObservationTx(ctx, tx, projection, observation); err != nil {
		t.Fatalf("matching stored facts did not upgrade a legacy digest: %v", err)
	}
	var count int
	var raw, digest []byte
	if err := tx.QueryRow(`SELECT COUNT(*), facts_blob, sha256 FROM conversation_observations WHERE projection_id=?`, projection).Scan(&count, &raw, &digest); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(digest, make([]byte, sha256.Size)) {
		t.Fatal("legacy observation digest was not refreshed")
	}
	got, err := loadTranscriptObservationTx(ctx, tx, projection, observation.ID)
	if err != nil {
		t.Fatal(err)
	}
	facts := observation
	facts.Text, facts.Detail = "", nil
	if count != 1 || !reflect.DeepEqual(got, facts) {
		t.Fatal("source address or derived facts changed, or native null time was lost")
	}
	var payload map[string]json.RawMessage
	raw, err = decodeTranscriptFacts(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload["text"] != nil || payload["detail"] != nil {
		t.Fatal("observation persisted a payload copy")
	}
	var storedBytes int
	if err := tx.QueryRow(`SELECT (SELECT COALESCE(SUM(length(facts_blob)),0) FROM conversation_observations WHERE projection_id=?) + (SELECT COALESCE(SUM(length(value_key)),0) FROM conversation_parse_values WHERE projection_id=?)`, projection, projection).Scan(&storedBytes); err != nil || storedBytes > 4096 {
		t.Fatalf("observation copied native payload into fact or dictionary storage: bytes=%d err=%v", storedBytes, err)
	}
	changed := observation
	changed.Text = "changed"
	changed.Detail = []byte(`{"changed":true}`)
	if err := writeTranscriptObservationTx(ctx, tx, projection, changed); err != nil {
		t.Fatalf("discarded payload changed stored observation identity: %v", err)
	}
	changed.Source.Start++
	if err := writeTranscriptObservationTx(ctx, tx, projection, changed); !errors.Is(err, ErrTranscriptConflict) {
		t.Fatalf("stored source address changed silently: %v", err)
	}
	relation := model.Relation{Kind: model.RelationReplayOf, FromObservation: "later", ToObservation: observation.ID, Scope: observation.Scope, ObservationRefs: []string{observation.ID, "later"}}
	for i := 0; i < 2; i++ {
		if err := writeTranscriptRelationTx(ctx, tx, projection, relation); err != nil {
			t.Fatal(err)
		}
	}
	var kind model.RelationKind
	if err := tx.QueryRow(`SELECT COUNT(*),relation_kind,relation_json FROM conversation_relations WHERE projection_id=?`, projection).Scan(&count, &kind, &raw); err != nil {
		t.Fatal(err)
	}
	saved, err := loadTranscriptRelationTx(ctx, tx, projection, kind, raw)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || !reflect.DeepEqual(saved, relation) {
		t.Fatal("typed relationship or evidence order changed")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Only this disposable test projection is removed.
	if _, err := store.db.Exec(`DELETE FROM conversation_projections WHERE id=?`, projection); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"conversation_observations", "conversation_relations"} {
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE projection_id=?`, projection).Scan(&count); err != nil || count != 0 {
			t.Fatalf("collected projection retained %s: count=%d err=%v", table, count, err)
		}
	}
}

func TestTranscriptObservationMembershipKeepsSourceOrder(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "memberships")
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	first := model.Observation{ID: "z-first", Text: "First source record"}
	second := model.Observation{ID: "a-second", Text: "Second source record"}
	for _, obs := range []model.Observation{first, second} {
		if err := writeTranscriptObservationTx(ctx, tx, projection, obs); err != nil {
			t.Fatal(err)
		}
	}
	turn := TranscriptTurn{Key: "turn", Ordinal: 1, StartSeq: 1, EndSeq: 1, Status: TurnUnknown, CompletionEvidence: CompletionNone}
	item := TranscriptItem{ID: "item", ProjectedSeq: 1, SourceKey: "line:1:0", TurnKey: turn.Key, TurnOrdinal: 1, Type: ItemUserMessage, Status: ItemStatusCompleted, Role: RoleUser, SourceLine: 1, SourceRowType: "user"}
	if err := writeTranscriptBatch(ctx, tx, projection, TranscriptProjectionResult{Items: []TranscriptItem{item}, Turns: []TranscriptTurn{turn}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := linkTranscriptObservationsTx(ctx, tx, projection, "item", item.ID, []model.Observation{second, first}); err != nil {
			t.Fatal(err)
		}
		if err := linkTranscriptObservationsTx(ctx, tx, projection, "turn", turn.Key, []model.Observation{first}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := store.db.Query(`SELECT k.value_key FROM conversation_item_observations m JOIN conversation_observations o ON o.id=m.observation_id JOIN conversation_parse_values k ON k.id=o.key_id WHERE m.projection_id=? AND m.item_id=? ORDER BY o.id`, projection, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, testTranscriptValueString(t, []byte(id)))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if !reflect.DeepEqual(ids, []string{first.ID, second.ID}) {
		t.Fatalf("observation order or multiplicity changed: %v", ids)
	}
	if _, err := store.db.Exec(`DELETE FROM conversation_items WHERE projection_id=?`, projection); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_item_observations WHERE projection_id=?`, projection).Scan(&count); err != nil || count != 0 {
		t.Fatalf("removed synthetic item retained membership: %d %v", count, err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_observations WHERE projection_id=?`, projection).Scan(&count); err != nil || count != 2 {
		t.Fatalf("item removal erased independent source evidence: %d %v", count, err)
	}
}

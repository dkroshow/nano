package history

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestParsedTranscriptTurnBindsLateNativeIDAndKeepsSourceFacts(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "parsed-turn")
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	source := model.Turn{Key: "source-turn", Ordinal: 80, StartSourceSeq: 400, EndSourceSeq: 700, Status: model.TurnInProgress, CompletionEvidence: model.CompletionNone, Scope: identityTestItem().Scope, Capture: model.CapturePartial, Outcome: model.OutcomeRunning}
	source.Scope.Kind, source.Scope.ItemID, source.Scope.TurnKey = model.ScopeTurn, "", source.Key
	view, err := ensureParsedTranscriptTurnTx(ctx, tx, projection, source)
	if err != nil {
		t.Fatal(err)
	}
	if view.Ordinal != 1 || view.StartSeq != 0 || view.EndSeq != 0 || view.NativeTurnID != "" {
		t.Fatalf("source ordinals became view positions: %+v", view)
	}
	for i, key := range []string{"first-final", "last-final"} {
		item := model.Item{ID: key, Scope: source.Scope, TurnKey: source.Key, Type: model.ItemAgentMessage, Role: model.RoleAssistant, Category: model.CategoryMessages, Status: model.ItemStatusCompleted, SourceLine: i + 1, Text: key, Finality: model.Finality{State: model.FinalityFinal, Basis: model.BasisNative}}
		item.Scope.Kind, item.Scope.ItemID = model.ScopeItem, key
		if _, _, err := writeParsedTranscriptItemTx(ctx, tx, projection, "native-thread", view, item); err != nil {
			t.Fatal(err)
		}
	}
	value := int64(10)
	bound := source
	bound.NativeTurnID = "native-turn"
	bound.Scope.NativeTurnID = &bound.NativeTurnID
	bound.Status, bound.Outcome, bound.CompletionEvidence = model.TurnCompleted, model.OutcomeCompleted, model.CompletionClaudeResult
	bound.FinalItemID, bound.FinalItemRefs = "last-final", []string{"first-final", "last-final"}
	obs := model.Observation{ID: "completion", Kind: model.ObservationResult, Scope: bound.Scope}
	bound.Observations = []model.Observation{obs}
	bound.Completion = []model.CompletionFact{{Scope: bound.Scope, Outcome: model.OutcomeCompleted, Basis: model.BasisNative, ObservationRefs: []string{obs.ID}, Reason: "result"}}
	bound.Usage = &model.TokenUsage{Input: &value, Contract: model.UsageContractPerTurnV1}
	bound.UsageBasis, bound.UsageRefs = "native", []string{obs.ID}
	bound.Times = []model.TimeObservation{{Class: model.TimeNative, Meaning: model.TimeEnd, Value: nil, ObservationRef: obs.ID}}
	finished, err := writeParsedTranscriptTurnTx(ctx, tx, projection, bound, nil)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Key != view.Key || finished.Ordinal != 1 || finished.StartSeq != 1 || finished.EndSeq != 2 || finished.CompletionSeq != 2 || finished.NativeTurnID != bound.NativeTurnID || finished.FinalItemID == "" || finished.Usage == nil || finished.Usage.Input == nil || *finished.Usage.Input != 10 || finished.CompletedAt != nil {
		t.Fatalf("late binding lost view facts or fabricated time: %+v", finished)
	}
	var parsed []byte
	if err := tx.QueryRow(`SELECT parsed_json FROM conversation_turns WHERE projection_id=? AND turn_key=?`, projection, view.Key).Scan(&parsed); err != nil {
		t.Fatal(err)
	}
	saved, err := decodeParsedTurnTx(ctx, tx, projection, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Ordinal != 80 || saved.StartSourceSeq != 400 || saved.EndSourceSeq != 700 || len(saved.FinalItemRefs) != 2 || len(saved.Completion) != 1 || len(saved.Times) != 1 || saved.Times[0].Value != nil || saved.Capture != model.CapturePartial || len(saved.Observations) != 0 {
		t.Fatal("turn conversion discarded source facts or copied source ordinals into view positions")
	}
	late := bound
	late.Key, late.Scope.TurnKey = "new-source-key-after-eviction", "new-source-key-after-eviction"
	late.Ordinal = 81
	resolved, err := ensureParsedTranscriptTurnTx(ctx, tx, projection, late)
	if err != nil || resolved.Key != view.Key || resolved.Ordinal != view.Ordinal {
		t.Fatalf("native turn did not resolve after source-local eviction: %+v %v", resolved, err)
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM conversation_turn_observations WHERE projection_id=? AND turn_key=?`, projection, view.Key).Scan(&count); err != nil || count != 1 {
		t.Fatalf("turn observation linkage=%d err=%v", count, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestParsedTranscriptTurnScopeIsolationAndConflict(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "turn-scopes")
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	turn := model.Turn{Key: "local", Scope: identityTestItem().Scope, NativeTurnID: "native"}
	turn.Scope.Kind, turn.Scope.ItemID, turn.Scope.TurnKey = model.ScopeTurn, "", turn.Key
	turn.Scope.NativeTurnID = &turn.NativeTurnID
	first, err := ensureParsedTranscriptTurnTx(ctx, tx, projection, turn)
	if err != nil {
		t.Fatal(err)
	}
	child := turn
	parent := "parent-call"
	child.Scope.NativeParentCallID = &parent
	other, err := ensureParsedTranscriptTurnTx(ctx, tx, projection, child)
	if err != nil || other.Key == first.Key {
		t.Fatalf("child turn joined its parent scope: %v", err)
	}
	conflict := turn
	conflict.NativeTurnID = "different-native"
	conflict.Scope.NativeTurnID = &conflict.NativeTurnID
	if _, err := ensureParsedTranscriptTurnTx(ctx, tx, projection, conflict); err == nil {
		t.Fatal("same local key accepted conflicting native turn identity")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT turn_key FROM conversation_turn_aliases WHERE projection_id=?`, projection).Scan(new(string)); err != sql.ErrNoRows {
		t.Fatalf("rolled-back turn aliases survived: %v", err)
	}
}

func TestParsedTurnDisplayTimesSurviveAppendAndEvictedAlias(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "display-times")
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	start, end, later := time.Unix(10, 0), time.Unix(20, 0), time.Unix(30, 0)
	turn := model.Turn{Key: "first", NativeTurnID: "native", Scope: identityTestItem().Scope, Status: model.TurnCompleted, Outcome: model.OutcomeCompleted}
	turn.Scope.Kind, turn.Scope.ItemID, turn.Scope.TurnKey = model.ScopeTurn, "", turn.Key
	turn.Scope.NativeTurnID = &turn.NativeTurnID
	turn.Completion = []model.CompletionFact{{Scope: turn.Scope, Outcome: model.OutcomeCompleted, Basis: model.BasisNative, ObservationRefs: []string{"terminal"}}}
	turn.Observations = []model.Observation{{ID: "terminal", Scope: turn.Scope, Kind: model.ObservationStop, Times: []model.TimeObservation{{Class: model.TimeNative, Meaning: model.TimeWrite, Value: &end}}}}
	view, err := writeParsedTranscriptTurnTx(ctx, tx, projection, turn, &start)
	if err != nil {
		t.Fatal(err)
	}
	for _, evicted := range []bool{false, true} {
		if evicted {
			turn.Key, turn.Scope.TurnKey = "after-eviction", "after-eviction"
			turn.Status, turn.Outcome, turn.Completion, turn.Observations = model.TurnUnknown, model.OutcomeUnknown, nil, nil
		}
		updated, err := writeParsedTranscriptTurnTx(ctx, tx, projection, turn, &later)
		if err != nil || updated.Key != view.Key || updated.StartedAt == nil || !updated.StartedAt.Equal(start) || updated.CompletedAt == nil || !updated.CompletedAt.Equal(end) {
			t.Fatalf("append/eviction changed display clocks: evicted=%t view=%+v err=%v", evicted, updated, err)
		}
	}
	_, saved, err := loadParsedTranscriptTurnTx(ctx, tx, projection, view.Key, true)
	if err != nil || saved.StartedAt != nil || saved.CompletedAt != nil {
		t.Fatal("display fallback became a provider-reported model start/end")
	}
}

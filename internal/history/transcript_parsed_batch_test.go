package history

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestParsedTranscriptApplyWritesViewsAndEvidenceAtomically(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: t.TempDir()}
	metadata := testSource(root, "thread.jsonl", "native-thread", ThreadMain, nil)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil {
		t.Fatal(err)
	}
	scope := identityTestItem().Scope
	scope.Kind, scope.ItemID, scope.TurnKey = model.ScopeTurn, "", "source-turn"
	turn := model.Turn{Key: scope.TurnKey, Ordinal: 10, Scope: scope, Status: model.TurnInProgress, CompletionEvidence: model.CompletionNone}
	first := model.Item{ID: "prompt", Scope: scope, TurnKey: turn.Key, Type: model.ItemUserMessage, Category: model.CategoryMessages, Role: model.RoleUser, Status: model.ItemStatusCompleted, Text: "initialtext", SourceLine: 1, SourceLineSHA256: strings.Repeat("a", 64), SourceRowType: "user", SourceSeq: 80}
	first.Scope.Kind, first.Scope.ItemID = model.ScopeItem, first.ID
	first.Observations = []model.Observation{{ID: "obs-1", Kind: model.ObservationSnapshot, Scope: first.Scope, Text: first.Text}}
	revised := first
	revised.Text = "replacementtext"
	more := model.Observation{ID: "obs-2", Kind: model.ObservationSnapshot, Scope: first.Scope, Text: revised.Text, NativeEventID: "uuid", NativeEventDigest: "digest"}
	revised.Observations = append(append([]model.Observation(nil), first.Observations...), more)
	standalone := model.Observation{ID: "control", Kind: model.ObservationUnknown, Scope: scope, Detail: []byte(`{"kind":"future"}`)}
	batch := model.Batch{Observations: []model.Observation{first.Observations[0], more, standalone}, Changes: []model.Change{{Turn: &turn}, {Item: &first}, {Item: &revised}}, Diagnostics: []model.Diagnostic{{Code: "unknown_control", Scope: scope, Severity: "warning"}}}
	result := func(key string, offset int64, value model.Batch) TranscriptProjectionResult {
		return TranscriptProjectionResult{ProjectionKey: key, CompleteOffset: offset, CompleteLines: int(offset), ParserState: []byte(`{"version":1}`), parsedBatches: func(yield func(model.Batch) error) error { return yield(value) }}
	}
	request := TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation, Provider: ProviderClaude, ParserVersion: "module-test", CompletionRuleVersion: "module-test", Result: result("module-first", 3, batch)}
	got, err := store.ApplyTranscript(ctx, request)
	if err != nil || got.ItemCount != 1 || got.LatestSeq != 1 {
		t.Fatalf("parsed rebuild: %+v %v", got, err)
	}
	page, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: applied.ThreadID})
	if err != nil || len(page.Items) != 1 || page.Items[0].Text != revised.Text || page.Items[0].ProjectedSeq != 1 || len(page.Turns) != 1 || page.Turns[0].Ordinal != 1 {
		t.Fatalf("converted store view: %+v %v", page, err)
	}
	firstWriter := request
	firstWriter.ExpectNoProjection = true
	firstWriter.Result.ProjectionKey = "concurrent-first-writer"
	visited := false
	firstWriter.Result.parsedBatches = func(func(model.Batch) error) error { visited = true; return nil }
	if _, err := store.ApplyTranscript(ctx, firstWriter); !errors.Is(err, ErrTranscriptChanged) || visited {
		t.Fatalf("concurrent first projection guard: visited=%t err=%v", visited, err)
	}
	canonical := page.Items[0].ID
	var projection int64
	if err := store.db.QueryRow(`SELECT id FROM conversation_projections WHERE projection_key=?`, got.ProjectionKey).Scan(&projection); err != nil {
		t.Fatal(err)
	}
	var count int
	for _, tc := range []struct {
		table string
		count int
	}{{"conversation_observations", 3}, {"conversation_item_observations", 2}, {"conversation_parse_diagnostics", 1}} {
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM `+tc.table+` WHERE projection_id=?`, projection).Scan(&count); err != nil || count != tc.count {
			t.Fatalf("%s count=%d want=%d err=%v", tc.table, count, tc.count, err)
		}
	}
	buildSearchModes(t, ctx, store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 50)
	late := revised
	late.ID, late.Scope.ItemID = "late-local", "late-local"
	late.SourceLine = 4
	late.Observations = []model.Observation{more}
	late.Observations[0].ID, late.Observations[0].Scope = "obs-late", late.Scope
	request.Mode, request.ExpectedProjectionKey = TranscriptAppend, got.ProjectionKey
	request.Result = result(got.ProjectionKey, 4, model.Batch{Observations: late.Observations, Changes: []model.Change{{Item: &late}}})
	appended, err := store.ApplyTranscript(ctx, request)
	if err != nil || appended.ItemCount != 1 || appended.LatestSeq != 1 {
		t.Fatalf("late replay append: %+v %v", appended, err)
	}
	page, err = store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: applied.ThreadID})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != canonical || page.Items[0].Text != revised.Text {
		t.Fatalf("replay changed canonical row: %v", err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_relations WHERE projection_id=? AND relation_kind='replay_of'`, projection).Scan(&count); err != nil || count != 1 {
		t.Fatalf("converted replay relations=%d err=%v", count, err)
	}
	// The persisted replay is not a competing endpoint for a later native ref.
	ref := model.Relation{Kind: model.RelationReplayOf, Scope: scope, FromObservation: "native-ref", ToNativeObservation: &model.NativeObservationRef{Scope: scope, Namespace: "native.eventId", Value: []byte(`"uuid"`)}}
	ref.ToNativeObservation.Scope.Kind, ref.ToNativeObservation.Scope.TurnKey = model.ScopeThread, ""
	refObservation := model.Observation{ID: ref.FromObservation, Scope: scope, Kind: model.ObservationSnapshot}
	request.Result = result(got.ProjectionKey, 5, model.Batch{Observations: []model.Observation{refObservation}, Changes: []model.Change{{Relation: &ref}}})
	if _, err := store.ApplyTranscript(ctx, request); err != nil {
		t.Fatal(err)
	}
	var target string
	var raw []byte
	if err := store.db.QueryRow(`SELECT t.value_key,r.relation_json FROM conversation_relations r JOIN conversation_parse_values f ON f.id=r.from_observation_value_id JOIN conversation_parse_values t ON t.id=r.to_observation_value_id WHERE r.projection_id=? AND f.value_key=?`, projection, testTranscriptValueKey(t, ref.FromObservation)).Scan(&target, &raw); err != nil || target != string(testTranscriptValueKey(t, more.ID)) {
		t.Fatalf("native endpoint resolution target=%q err=%v", target, err)
	}
	readTx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	sourceRelation, decodeErr := loadTranscriptRelationTx(ctx, readTx, projection, model.RelationReplayOf, raw)
	readTx.Rollback()
	if decodeErr != nil || sourceRelation.ToNativeObservation == nil || sourceRelation.ToObservation != "" {
		t.Fatal("resolved endpoint replaced the original source reference")
	}
	called := false
	guarded := request
	guarded.Result = result(got.ProjectionKey, 6, batch)
	guarded.Result.parsedBatches = func(func(model.Batch) error) error { called = true; return nil }
	guarded.SourceGeneration++
	if _, err := store.ApplyTranscript(ctx, guarded); !errors.Is(err, ErrTranscriptChanged) || called {
		t.Fatalf("stale source entered parser batch writer: called=%v err=%v", called, err)
	}
	guarded.SourceGeneration = request.SourceGeneration
	guarded.ExpectedProjectionKey = "different-projection"
	if _, err := store.ApplyTranscript(ctx, guarded); !errors.Is(err, ErrTranscriptChanged) || called {
		t.Fatalf("stale projection entered parser batch writer: called=%v err=%v", called, err)
	}
	failure := errors.New("spool failed")
	failed := request
	failed.Mode, failed.Result = TranscriptRebuild, result("failed-rebuild", 6, batch)
	failed.Result.parsedBatches = func(yield func(model.Batch) error) error {
		if err := yield(batch); err != nil {
			return err
		}
		return failure
	}
	if _, err := store.ApplyTranscript(ctx, failed); !errors.Is(err, failure) {
		t.Fatalf("batch failure did not abort rebuild: %v", err)
	}
	checkpoint, _, err := store.LoadTranscriptCheckpoint(ctx, applied.ThreadID)
	if err != nil || checkpoint.ProjectionKey != got.ProjectionKey || checkpoint.CompleteOffset != 5 || checkpoint.LatestSeq != 1 {
		t.Fatalf("failed rebuild replaced previous served projection: %+v %v", checkpoint, err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_projections WHERE projection_key='failed-rebuild'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed shadow projection survived rollback")
	}
	ready, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	rebuilding := request
	rebuilding.Mode, rebuilding.Result = TranscriptRebuild, result("background-rebuild", 6, batch)
	rebuilding.Result.parsedBatches = func(yield func(model.Batch) error) error {
		if err := yield(batch); err != nil {
			return err
		}
		close(ready)
		<-release
		return nil
	}
	go func() { _, err := store.ApplyTranscript(ctx, rebuilding); finished <- err }()
	select {
	case <-ready:
	case err := <-finished:
		t.Fatalf("background rebuild failed before pause: %v", err)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("background rebuild did not reach its guarded write")
	}
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	old, readErr := store.ReadTranscript(readCtx, TranscriptReadRequest{ThreadID: applied.ThreadID})
	cancel()
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if readErr != nil || old.Projection.Key != got.ProjectionKey || len(old.Items) != 1 {
		t.Fatalf("in-flight rebuild hid the previous served rows: key=%s items=%d err=%v", old.Projection.Key, len(old.Items), readErr)
	}
	current, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: applied.ThreadID})
	if err != nil || current.Projection.Key != rebuilding.Result.ProjectionKey || len(current.Items) != 1 {
		t.Fatalf("completed rebuild did not atomically replace the view: %v", err)
	}
}

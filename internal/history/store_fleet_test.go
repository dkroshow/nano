package history

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fleetCourtFixture writes one thread whose projection holds the given items in
// order, and returns its nano id. Items are described as (type, sidechain,
// final, text) so a test can pin exactly which one the court query must pick.
type fleetCourtItem struct {
	itemType  TranscriptItemType
	sidechain bool
	final     bool
	text      string
}

func fleetCourtThread(t *testing.T, store *Store, root SourceRoot, name, nativeID string, kind ThreadKind, updatedAt *time.Time, items []fleetCourtItem) string {
	return fleetCourtThreadWithStatus(t, store, root, name, nativeID, kind, updatedAt, TurnCompleted, items)
}

func fleetCourtThreadWithStatus(t *testing.T, store *Store, root SourceRoot, name, nativeID string, kind ThreadKind, updatedAt *time.Time, status TranscriptTurnStatus, items []fleetCourtItem) string {
	t.Helper()
	ctx := context.Background()
	metadata := testSource(root, name, nativeID, kind, updatedAt)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("stored source = %v, %v", ok, err)
	}
	turn := TranscriptTurn{Key: "ntr_" + nativeID, Ordinal: 1, StartSeq: 1,
		EndSeq: int64(len(items)), Status: status, CompletionEvidence: CompletionTaskComplete}
	result := TranscriptProjectionResult{
		ProjectionKey: "ntp_" + nativeID, CompleteOffset: 1, CompleteLines: len(items),
		ParserState: json.RawMessage(`{}`),
	}
	for index, item := range items {
		suffix := string(rune('a' + index))
		projected := TranscriptItem{
			ID: "nit_" + nativeID + "_" + suffix, ProjectedSeq: int64(index + 1),
			SourceKey: nativeID + "-" + suffix, TurnKey: turn.Key, TurnOrdinal: 1,
			Type: item.itemType, Status: ItemStatusCompleted, Text: item.text,
			Sidechain: item.sidechain, IsFinalAnswer: item.final, SourceLine: index + 1,
			SourceRowType: "test", SourceLineSHA256: strings.Repeat("0", sha256.Size*2),
		}
		if item.final {
			// The store derives is_final_answer from the turn's final item, so
			// the turn has to name it — the same invariant the real adapters
			// maintain.
			turn.FinalItemID = projected.ID
		}
		result.Items = append(result.Items, projected)
	}
	result.Turns = []TranscriptTurn{turn}
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID,
		SourceGeneration: stored.Generation, Provider: root.Provider,
		ParserVersion: "test-v1", CompletionRuleVersion: "test-rules-v1", Result: result,
	}); err != nil {
		t.Fatal(err)
	}
	return applied.ThreadID
}

// TestFleetCourtMessagesPicksTheDecidingMessage is the read half of the
// classifier in one test: the last user or final-answer message wins, mid-turn
// agent chatter and a main thread's sidechain traffic do not, a subagent thread
// (all sidechain by construction) still reports its own conversation, and a
// thread with nothing to say is present with an empty type rather than absent.
func TestFleetCourtMessagesPicksTheDecidingMessage(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: filepath.Join(t.TempDir(), "projects")}
	updated := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	longTail := strings.Repeat("x", fleetCourtTailRunes+fleetCourtHeadRunes)
	working := fleetCourtThread(t, store, root, "working.jsonl", "native-working", ThreadMain, &updated, []fleetCourtItem{
		{itemType: ItemUserMessage, text: "please investigate"},
		{itemType: ItemAgentMessage, text: "still thinking " + longTail},
	})
	answered := fleetCourtThread(t, store, root, "answered.jsonl", "native-answered", ThreadMain, &updated, []fleetCourtItem{
		{itemType: ItemUserMessage, text: "please investigate"},
		{itemType: ItemAgentMessage, final: true, text: "Done. Want me to deploy it?"},
	})
	leaky := fleetCourtThread(t, store, root, "leaky.jsonl", "native-leaky", ThreadMain, &updated, []fleetCourtItem{
		{itemType: ItemUserMessage, text: "please investigate"},
		{itemType: ItemAgentMessage, sidechain: true, final: true, text: "subagent wrap-up"},
	})
	worker := fleetCourtThread(t, store, root, "worker.jsonl", "native-worker", ThreadSubagent, &updated, []fleetCourtItem{
		{itemType: ItemUserMessage, sidechain: true, text: "do the sweep"},
		{itemType: ItemAgentMessage, sidechain: true, final: true, text: "sweep complete"},
	})
	interrupted := fleetCourtThreadWithStatus(t, store, root, "interrupted.jsonl", "native-interrupted", ThreadSubagent, &updated, TurnInterrupted, []fleetCourtItem{
		{itemType: ItemUserMessage, sidechain: true, text: "do the sweep"},
	})
	quiet := fleetCourtThread(t, store, root, "quiet.jsonl", "native-quiet", ThreadMain, nil, []fleetCourtItem{
		{itemType: ItemToolCall, text: "grep"},
	})

	courts, err := store.FleetCourtMessages(ctx, []string{working, answered, leaky, worker, interrupted, quiet, "nth_missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(courts) != 6 {
		t.Fatalf("court rows = %d; an unknown id must contribute nothing", len(courts))
	}
	for _, tc := range []struct {
		name     string
		id       string
		itemType TranscriptItemType
		head     string
	}{
		{"mid-turn agent chatter never decides the court", working, ItemUserMessage, "please investigate"},
		{"a final answer decides the court", answered, ItemAgentMessage, "Done. Want me to deploy it?"},
		{"a main thread ignores sidechain traffic", leaky, ItemUserMessage, "please investigate"},
		{"a subagent thread reads its own sidechain", worker, ItemAgentMessage, "sweep complete"},
		{"tool-only threads have no court message", quiet, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			court, ok := courts[tc.id]
			if !ok || court.ItemType != tc.itemType || court.Head != tc.head {
				t.Fatalf("court = %#v, present = %v", court, ok)
			}
		})
	}
	if court := courts[working]; court.LastActivity == nil || !court.LastActivity.Equal(updated) {
		t.Fatalf("activity clock = %v", court.LastActivity)
	}
	if court := courts[quiet]; court.LastActivity != nil {
		t.Fatalf("a thread with no timestamps must report no clock: %v", court.LastActivity)
	}
	if court := courts[interrupted]; !court.Interrupted {
		t.Fatalf("interrupted turn lost its terminal state: %#v", court)
	}

	long := fleetCourtThread(t, store, root, "long.jsonl", "native-long", ThreadMain, &updated, []fleetCourtItem{
		{itemType: ItemAgentMessage, final: true, text: "start" + longTail + "end?"},
	})
	longCourts, err := store.FleetCourtMessages(ctx, []string{long})
	if err != nil {
		t.Fatal(err)
	}
	tail := longCourts[long]
	if len([]rune(tail.Head)) != fleetCourtHeadRunes || len([]rune(tail.Tail)) != fleetCourtTailRunes ||
		!strings.HasPrefix(tail.Head, "start") || !strings.HasSuffix(tail.Tail, "end?") {
		t.Fatalf("bounded windows: head=%d tail=%d", len([]rune(tail.Head)), len([]rune(tail.Tail)))
	}
}

// TestResolveThreadReferencesPrefersExactThenPrefix pins the id resolution an
// agent types: exact ids win over prefixes of longer ids, an unambiguous prefix
// resolves, an ambiguous one reports every candidate, and LIKE wildcards in the
// reference are literal characters rather than a match-everything query.
func TestResolveThreadReferencesPrefersExactThenPrefix(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	updated := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	for _, native := range []string{"abc", "abcdef", "zz9"} {
		if _, err := store.ApplySource(ctx, testSource(root, native+".jsonl", native, ThreadMain, &updated)); err != nil {
			t.Fatal(err)
		}
	}

	exact, err := store.ResolveThreadReferences(ctx, "abc", 10)
	if err != nil || len(exact) != 1 || exact[0].NativeID != "abc" {
		t.Fatalf("exact native id = %#v, %v", exact, err)
	}
	nano, err := store.ResolveThreadReferences(ctx, exact[0].ID, 10)
	if err != nil || len(nano) != 1 || nano[0].ID != exact[0].ID {
		t.Fatalf("exact nano id = %#v, %v", nano, err)
	}
	unique, err := store.ResolveThreadReferences(ctx, "abcd", 10)
	if err != nil || len(unique) != 1 || unique[0].NativeID != "abcdef" {
		t.Fatalf("unambiguous prefix = %#v, %v", unique, err)
	}
	ambiguous, err := store.ResolveThreadReferences(ctx, "ab", 10)
	if err != nil || len(ambiguous) != 2 {
		t.Fatalf("ambiguous prefix = %#v, %v", ambiguous, err)
	}
	wildcard, err := store.ResolveThreadReferences(ctx, "%", 10)
	if err != nil || len(wildcard) != 0 {
		t.Fatalf("wildcard reference matched %d threads, %v", len(wildcard), err)
	}
	if got := escapeLikePrefix(`a_b%c\d`); got != `a\_b\%c\\d%` {
		t.Fatalf("escaped pattern = %q", got)
	}
	// Every nano id shares the nth_ prefix, so the bound is what stops a
	// reference like that from returning the whole catalog.
	bounded, err := store.ResolveThreadReferences(ctx, "nth_", 2)
	if err != nil || len(bounded) != 2 {
		t.Fatalf("bounded candidates = %#v, %v", bounded, err)
	}
	if bounded[0].Provider != ProviderCodex || bounded[0].NativeID == "" {
		t.Fatalf("candidate identity = %#v", bounded[0])
	}
}

package history

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pinFixtureThread writes one thread with `turns` turns. Each turn holds a user
// message, a reasoning item, and two agent messages. idPrefix lets a rebuild
// emit different projection-scoped ids for the same source keys.
func pinFixtureThread(t *testing.T, store *Store, root SourceRoot, turns int, idPrefix, projectionKey string, skip map[string]bool) string {
	t.Helper()
	ctx := context.Background()
	updated := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	metadata := testSource(root, "pins.jsonl", "native-pins", ThreadMain, &updated)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("stored source = %v, %v", ok, err)
	}
	result := TranscriptProjectionResult{ProjectionKey: projectionKey, CompleteOffset: 1, ParserState: json.RawMessage(`{}`)}
	seq := int64(0)
	for ordinal := 1; ordinal <= turns; ordinal++ {
		turn := TranscriptTurn{Key: fmt.Sprintf("ntr_%d", ordinal), NativeTurnID: fmt.Sprintf("native-turn-%d", ordinal),
			Ordinal: ordinal, StartSeq: seq + 1, Status: TurnCompleted, CompletionEvidence: CompletionTaskComplete}
		for index, kind := range []TranscriptItemType{ItemUserMessage, ItemReasoning, ItemAgentMessage, ItemAgentMessage} {
			source := fmt.Sprintf("t%d-%d", ordinal, index)
			if skip[source] {
				continue
			}
			seq++
			created := updated.Add(time.Duration(seq) * time.Second)
			item := TranscriptItem{
				ID: idPrefix + source, ProjectedSeq: seq, SourceKey: source, TurnKey: turn.Key, TurnOrdinal: ordinal,
				Type: kind, Status: ItemStatusCompleted, Text: fmt.Sprintf("%s turn %d item %d", kind, ordinal, index),
				CreatedAt: &created, SourceLine: int(seq), SourceRowType: "test",
				SourceLineSHA256: strings.Repeat("0", sha256.Size*2),
			}
			if ordinal == 2 && index == 3 {
				item.Text, item.ContentRef, item.OriginalBytes = "preview", "ncb_"+idPrefix+source, 20000
				result.Bodies = append(result.Bodies, TranscriptBody{ItemID: item.ID, ContentRef: item.ContentRef,
					Text: strings.Repeat("b", 20000), OriginalBytes: 20000, StoredBytes: 20000})
			}
			result.Items = append(result.Items, item)
		}
		turn.EndSeq = seq
		turn.CompletionSeq = seq
		result.Turns = append(result.Turns, turn)
	}
	result.CompleteLines = int(seq)
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: root.Provider, ParserVersion: "test-v1", CompletionRuleVersion: "test-rules-v1", Result: result,
	}); err != nil {
		t.Fatal(err)
	}
	return applied.ThreadID
}

func openPinStore(t *testing.T) (*Store, SourceRoot) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: filepath.Join(t.TempDir(), "projects")}
}

func TestLocatePinAnchorAcceptsOnlyMessages(t *testing.T) {
	ctx := context.Background()
	store, root := openPinStore(t)
	thread := pinFixtureThread(t, store, root, 3, "nit_", "ntp_a", nil)

	anchor, err := store.LocatePinAnchor(ctx, thread, "nit_t2-2")
	if err != nil || anchor.TurnOrdinal != 2 || anchor.Type != ItemAgentMessage || anchor.TurnKey != "ntr_2" {
		t.Fatalf("anchor = %#v, %v", anchor, err)
	}
	latest, err := store.LocatePinAnchor(ctx, thread, "")
	if err != nil || latest.ItemID != "nit_t3-3" {
		t.Fatalf("latest = %#v, %v", latest, err)
	}
	for _, id := range []string{"nit_t2-1", "nit_missing"} {
		if _, err := store.LocatePinAnchor(ctx, thread, id); !errors.Is(err, ErrPinAnchorNotFound) {
			t.Fatalf("%s: err = %v, want ErrPinAnchorNotFound", id, err)
		}
	}
	if _, err := store.LocatePinAnchor(ctx, "nth_absent", "nit_t2-2"); !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("absent thread err = %v", err)
	}
}

func TestReadPinContextReturnsWholePrecedingTurnsCutAtAnchor(t *testing.T) {
	ctx := context.Background()
	store, root := openPinStore(t)
	thread := pinFixtureThread(t, store, root, 14, "nit_", "ntp_a", nil)

	// Anchor the first agent message of turn 12: 10 whole turns before it
	// (2..11), then turn 12 up to and including the anchor only.
	got, err := store.ReadPinContext(ctx, thread, "nit_t12-2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 11 || got.Turns[0].Ordinal != 2 || got.Turns[10].Ordinal != 12 || !got.HasEarlierHistory {
		t.Fatalf("turns = %#v earlier=%v", got.Turns, got.HasEarlierHistory)
	}
	last := got.Messages[len(got.Messages)-1]
	if last.ItemID != "nit_t12-2" || got.Anchor.ItemID != "nit_t12-2" {
		t.Fatalf("last message = %#v anchor = %#v", last, got.Anchor)
	}
	// Three messages per whole turn (reasoning excluded) plus two in the anchor turn.
	if len(got.Messages) != 10*3+2 {
		t.Fatalf("messages = %d", len(got.Messages))
	}
	for _, message := range got.Messages {
		if message.Type == ItemReasoning || message.ItemID == "nit_t12-3" {
			t.Fatalf("unexpected message %#v", message)
		}
	}

	all, err := store.ReadPinContext(ctx, thread, "nit_t12-2", 20)
	if err != nil || all.Turns[0].Ordinal != 1 || all.HasEarlierHistory {
		t.Fatalf("all = %#v, %v", all.Turns, err)
	}
	var body *PinMessage
	for index := range all.Messages {
		if all.Messages[index].ItemID == "nit_t2-3" {
			body = &all.Messages[index]
		}
	}
	if body == nil || body.Body == nil || len(*body.Body) != 20000 || body.ContentRef == "" {
		t.Fatalf("retained body = %#v", body)
	}

	zero, err := store.ReadPinContext(ctx, thread, "nit_t1-0", 0)
	if err != nil || len(zero.Turns) != 1 || len(zero.Messages) != 1 || zero.HasEarlierHistory {
		t.Fatalf("zero = %#v, %v", zero, err)
	}
	if _, err := store.ReadPinContext(ctx, thread, "nit_t1-0", MaxPinContextTurns+1); !errors.Is(err, ErrInvalidTranscriptPage) {
		t.Fatalf("over-limit err = %v", err)
	}
}

// A rebuild keeps canonical ids, and a rebuild with new projection-scoped ids
// records aliases, so a pin written against the old id still resolves. A
// rebuild that no longer contains the message reports a broken anchor.
func TestPinAnchorSurvivesRebuildAndReportsMissing(t *testing.T) {
	ctx := context.Background()
	store, root := openPinStore(t)
	thread := pinFixtureThread(t, store, root, 4, "old_", "ntp_a", nil)
	if _, err := store.LocatePinAnchor(ctx, thread, "old_t3-2"); err != nil {
		t.Fatal(err)
	}

	pinFixtureThread(t, store, root, 4, "nit_", "ntp_b", nil)
	got, err := store.ReadPinContext(ctx, thread, "old_t3-2", 10)
	if err != nil || got.Anchor.ItemID != "nit_t3-2" {
		t.Fatalf("aliased anchor = %#v, %v", got.Anchor, err)
	}

	pinFixtureThread(t, store, root, 4, "nit_", "ntp_c", map[string]bool{"t3-2": true})
	if _, err := store.ReadPinContext(ctx, thread, "nit_t3-2", 10); !errors.Is(err, ErrPinAnchorNotFound) {
		t.Fatalf("missing anchor err = %v", err)
	}
	if _, err := store.LocatePinAnchor(ctx, thread, "old_t3-2"); !errors.Is(err, ErrPinAnchorNotFound) {
		t.Fatalf("missing aliased anchor err = %v", err)
	}
}

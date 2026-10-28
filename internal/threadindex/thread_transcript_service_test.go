package threadindex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

func completedTranscriptFixture(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	var data strings.Builder
	data.WriteString(`{"type":"session_meta","payload":{"id":"native-target","cwd":"/disposable","timestamp":"2026-07-13T20:00:00Z"}}` + "\n")
	for ordinal := 0; ordinal < 7; ordinal++ {
		turnID := fmt.Sprintf("later-%d", ordinal)
		if ordinal == 0 {
			turnID = "old-turn"
		}
		data.WriteString(fmt.Sprintf("{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":%q}}\n", turnID))
		data.WriteString(`{"type":"event_msg","payload":{"type":"user_message","message":"old prompt"}}` + "\n")
		for row := 0; row < 130; row++ {
			data.WriteString(fmt.Sprintf("{\"type\":\"response_item\",\"payload\":{\"type\":\"reasoning\",\"id\":\"reason-%d-%d\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"private\"}]}}\n", ordinal, row))
		}
		data.WriteString(fmt.Sprintf("{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"id\":\"final-%d\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"old final\"}]}}\n", ordinal))
		data.WriteString(fmt.Sprintf("{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"turn_id\":%q}}\n", turnID))
	}
	if err := os.WriteFile(filepath.Join(root, "rollout-native-target.jsonl"), []byte(data.String()), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	service, err := newTestCatalogService(context.Background(), store, Options{Roots: []history.SourceRoot{{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: root}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	service.RequestRefresh() // the explicit discovery a new store needs
	var threadID string
	waitForCatalogWithin(t, time.Minute, func() bool {
		id, err := service.ResolveThreadReference(context.Background(), "native-target")
		if err != nil {
			return false
		}
		threadID = id
		_, err = service.LoadCompletedTranscriptTurn(context.Background(), id, "old-turn")
		return err == nil
	})
	return service, threadID
}

func TestCompletedTranscriptServiceRealLargeTurn(t *testing.T) {
	service, threadID := completedTranscriptFixture(t)
	partial, err := service.LoadTranscript(context.Background(), threadID, TranscriptQuery{Limit: 100, BeforeTurn: 2, TurnLimit: 1, HiddenSpecified: true, View: DefaultThreadTranscriptView})
	if err != nil || !partial.Page.StartsMidTurn {
		t.Fatalf("fixture is not partial: %+v %v", partial.Page, err)
	}
	complete, err := service.LoadCompletedTranscriptTurn(context.Background(), threadID, "old-turn")
	if err != nil || len(complete.Items) != 2 || complete.ThreadID != threadID || complete.Turns[0].NativeTurnID != "old-turn" || complete.Items[1].NativeItemID != "final-0" || complete.Items[1].Text != "old final" || !complete.Items[1].IsFinalAnswer || complete.Page.StartsMidTurn {
		t.Fatalf("exact turn: %+v %v", complete, err)
	}
	if _, err := service.LoadCompletedTranscriptTurn(context.Background(), threadID, "wrong-turn"); err == nil {
		t.Fatal("wrong turn accepted")
	}
	if _, err := service.LoadCompletedTranscriptTurn(context.Background(), "wrong-thread", "old-turn"); err == nil {
		t.Fatal("wrong thread accepted")
	}
}

func TestThreadTranscriptCursorRoundTrip(t *testing.T) {
	want := threadTranscriptCursor{
		Version: threadTranscriptCursorVersion, ThreadID: "nth_one", ProjectionKey: "ntp_one",
		View: DefaultThreadTranscriptView, Profile: threadTranscriptProfile,
		SnapshotHeadSeq: 90, ExclusiveBeforeSeq: 51,
	}
	encoded, err := encodeThreadTranscriptCursor(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeThreadTranscriptCursor(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("cursor = %#v, want %#v", got, want)
	}
}

func TestThreadTranscriptCursorCannotCrossThreads(t *testing.T) {
	cursor, err := encodeThreadTranscriptCursor(threadTranscriptCursor{
		Version: threadTranscriptCursorVersion, ThreadID: "nth_other", ProjectionKey: "ntp_one",
		View: DefaultThreadTranscriptView, Profile: threadTranscriptProfile,
		SnapshotHeadSeq: 90, ExclusiveBeforeSeq: 51,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{}
	_, err = service.LoadTranscript(context.Background(), "nth_one", TranscriptQuery{
		Limit: defaultThreadTranscriptLimit, View: DefaultThreadTranscriptView, Before: cursor,
	})
	if !errors.Is(err, errInvalidTranscriptCursor) {
		t.Fatalf("cross-thread cursor error = %v", err)
	}
}

func TestThreadTranscriptCursorCannotCrossCategoryFilters(t *testing.T) {
	cursor, err := encodeThreadTranscriptCursor(threadTranscriptCursor{
		Version: threadTranscriptCursorVersion, ThreadID: "nth_one", ProjectionKey: "ntp_one",
		View: DevThreadTranscriptView, Profile: threadTranscriptProfile,
		HiddenCategories: "reasoning,system", SnapshotHeadSeq: 90, ExclusiveBeforeSeq: 51,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{}
	_, err = service.LoadTranscript(context.Background(), "nth_one", TranscriptQuery{
		Limit: defaultThreadTranscriptLimit, View: DevThreadTranscriptView, Before: cursor,
		HiddenCategories: []string{"system"}, HiddenSpecified: true,
	})
	if !errors.Is(err, errInvalidTranscriptCursor) {
		t.Fatalf("cross-filter cursor error = %v", err)
	}
}

func TestFinalizeTranscriptResponseEnforcesByteBudget(t *testing.T) {
	items := make([]TranscriptItem, 0, MaxThreadTranscriptLimit)
	turns := make([]TranscriptTurn, 0, MaxThreadTranscriptLimit)
	for sequence := int64(1); sequence <= MaxThreadTranscriptLimit; sequence++ {
		turnID := "turn_" + string(rune(sequence+64))
		items = append(items, TranscriptItem{
			ID: turnID + "_item", ProjectedSeq: sequence, TurnID: turnID,
			Type: "agent_message", Text: strings.Repeat("x", 4<<10), FilterTags: []string{},
		})
		turns = append(turns, TranscriptTurn{ID: turnID, Ordinal: int(sequence), StartSeq: sequence, EndSeq: sequence})
	}
	response := TranscriptResponse{
		ThreadID: "nth_one", View: DefaultThreadTranscriptView, Items: items, Turns: turns,
		Page:       TranscriptPageInfo{StartSeq: 1, EndSeq: MaxThreadTranscriptLimit, LatestSeq: MaxThreadTranscriptLimit, SnapshotHeadSeq: MaxThreadTranscriptLimit},
		Projection: TranscriptProjection{Key: "ntp_one", State: "ready", SourceStatus: "present", EvidenceAvailable: true, ItemCount: MaxThreadTranscriptLimit},
	}
	if err := finalizeTranscriptResponse(&response); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > MaxThreadTranscriptBytes || !response.Page.HasMoreBefore ||
		!response.Page.StartsMidTurn || response.Page.NextBeforeCursor == nil || response.Page.StartSeq <= 1 {
		t.Fatalf("bounded response: bytes=%d page=%#v items=%d", len(body), response.Page, len(response.Items))
	}
}

func TestFinalizeTranscriptResponseKeepsSoleLazyBodyRow(t *testing.T) {
	response := TranscriptResponse{
		ThreadID: "nth_one", View: DefaultThreadTranscriptView,
		Items: []TranscriptItem{{
			ID: "large-final", ProjectedSeq: 7, Type: "agent_message",
			Text:       strings.Repeat("x", MaxThreadTranscriptBytes+1),
			ContentRef: "ncb_large", ContentTruncated: true,
		}},
		Page: TranscriptPageInfo{
			StartSeq: 7, EndSeq: 7, LatestSeq: 7, SnapshotHeadSeq: 7,
			HasMoreBefore: true,
		},
		Projection: TranscriptProjection{
			Key: "ntp_one", State: "ready", SourceStatus: "present", EvidenceAvailable: true, ItemCount: 1,
		},
	}
	if err := finalizeTranscriptResponse(&response); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > MaxThreadTranscriptBytes {
		t.Fatalf("response bytes = %d, cap %d", len(body), MaxThreadTranscriptBytes)
	}
	if len(response.Items) != 1 || response.Items[0].ID != "large-final" ||
		response.Items[0].ContentRef != "ncb_large" || response.Items[0].Text != "" ||
		!response.Items[0].ContentTruncated || response.Page.NextBeforeCursor == nil {
		t.Fatalf("oversized lazy row lost reachability: %#v page=%#v", response.Items, response.Page)
	}
}

func TestThreadTranscriptCursorRejectsMalformedAndUnboundValues(t *testing.T) {
	unknownField := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"threadId":"nth_one","projectionKey":"ntp_one","view":"readable","profile":"readable-v1","snapshotHeadSeq":90,"beforeSeq":51,"extra":true}`))
	wrongView := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"threadId":"nth_one","projectionKey":"ntp_one","view":"raw","profile":"readable-v1","snapshotHeadSeq":90,"beforeSeq":51}`))
	badBoundary := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"threadId":"nth_one","projectionKey":"ntp_one","view":"readable","profile":"readable-v1","snapshotHeadSeq":90,"beforeSeq":92}`))
	for _, value := range []string{
		"not-base64!", unknownField, wrongView, badBoundary,
		strings.Repeat("x", maxThreadTranscriptCursorBytes+1),
	} {
		if _, err := decodeThreadTranscriptCursor(value); !errors.Is(err, errInvalidTranscriptCursor) {
			t.Fatalf("cursor %q error = %v", value, err)
		}
	}
}

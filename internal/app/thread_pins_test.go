package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/threads"
)

const (
	pinNativeA = "aaaaaaaa-1111-4111-8111-111111111111"
	pinNativeB = "bbbbbbbb-2222-4222-8222-222222222222"
)

// seedPinTranscript writes a native Claude thread with `turns` turns; each turn
// has a user message, a reasoning item, and an agent message. Item ids are
// "nit_<native>_t<turn>-<index>".
func seedPinTranscript(t *testing.T, store *history.Store, root history.SourceRoot, native string, turns int, skip map[string]bool) {
	t.Helper()
	ctx := context.Background()
	updated := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	src := testSidebarSource(root, native+".jsonl", native, history.ThreadMain, "", &updated)
	applied, err := store.ApplySource(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustSidebarRootID(t, store, root), src.Source.Path)
	if err != nil || !ok {
		t.Fatalf("stored source = %v %v", ok, err)
	}
	result := history.TranscriptProjectionResult{ProjectionKey: fmt.Sprintf("ntp_%s_%d_%d", native, turns, len(skip)), CompleteOffset: 1, ParserState: json.RawMessage(`{}`)}
	seq := int64(0)
	for ordinal := 1; ordinal <= turns; ordinal++ {
		turn := history.TranscriptTurn{Key: fmt.Sprintf("ntr_%s_%d", native, ordinal), Ordinal: ordinal, StartSeq: seq + 1,
			Status: history.TurnCompleted, CompletionEvidence: history.CompletionTaskComplete}
		for index, kind := range []history.TranscriptItemType{history.ItemUserMessage, history.ItemReasoning, history.ItemAgentMessage} {
			source := fmt.Sprintf("%s-t%d-%d", native, ordinal, index)
			if skip[source] {
				continue
			}
			seq++
			created := updated.Add(time.Duration(seq) * time.Second)
			result.Items = append(result.Items, history.TranscriptItem{
				ID: fmt.Sprintf("nit_%s_t%d-%d", native, ordinal, index), ProjectedSeq: seq, SourceKey: source,
				TurnKey: turn.Key, TurnOrdinal: ordinal, Type: kind, Status: history.ItemStatusCompleted,
				Text: fmt.Sprintf("%s: turn %d topic-%d", kind, ordinal, ordinal), CreatedAt: &created,
				SourceLine: int(seq), SourceRowType: "test", SourceLineSHA256: strings.Repeat("0", 64),
			})
		}
		turn.EndSeq, turn.CompletionSeq = seq, seq
		result.Turns = append(result.Turns, turn)
	}
	result.CompleteLines = int(seq)
	if _, err := store.ApplyTranscript(ctx, history.TranscriptApplyRequest{
		Mode: history.TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: root.Provider, ParserVersion: "test-v1", CompletionRuleVersion: "test-rules-v1", Result: result,
	}); err != nil {
		t.Fatal(err)
	}
}

type pinFixture struct {
	server     *server
	mux        *http.ServeMux
	store      *history.Store
	root       history.SourceRoot
	executions *hostedstore.ExecutionStore
	hosted     threads.SessionRecord
	pinsPath   string
}

func newPinFixture(t *testing.T) *pinFixture {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	root := history.SourceRoot{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: filepath.Join(t.TempDir(), "projects")}
	seedPinTranscript(t, store, root, pinNativeA, 14, nil)
	control, executions := newTestHostedControl(t)
	hosted := seedHostedNative(t, executions, "claude", pinNativeA, "ready")
	pinsPath := filepath.Join(t.TempDir(), "thread-pins.sqlite")
	pins, err := openThreadPinsStore(pinsPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pins.Close() })
	s := &server{
		Registry: newSessionRegistry(), agentExecutions: executions, controlMetadata: control,
		catalog: newStoredCatalog(t, store), threadPins: pins,
	}
	mux := http.NewServeMux()
	mountRoutes(mux, applicationRoutes(fixtureOwners(s)))
	return &pinFixture{server: s, mux: mux, store: store, root: root, executions: executions, hosted: hosted, pinsPath: pinsPath}
}

func (f *pinFixture) do(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Host = "127.0.0.1:7690"
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (f *pinFixture) pin(t *testing.T, body map[string]any) threadPinView {
	t.Helper()
	code, raw := f.do(t, http.MethodPost, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins", body)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create pin status = %d body %s", code, raw)
	}
	var view threadPinView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func decodePinJSON[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return value
}

func TestThreadPinAppendAnchorsExactOrLatestMessage(t *testing.T) {
	f := newPinFixture(t)
	exact := f.pin(t, map[string]any{"text": "standard analysis", "commentary": "resume the reconciler question",
		"itemId": "nit_" + pinNativeA + "_t5-2", "creator": "Operator"})
	if exact.Anchor.ItemID != "nit_"+pinNativeA+"_t5-2" || exact.Anchor.NativeThreadID != pinNativeA ||
		exact.Anchor.Role != "agent" || exact.Anchor.TurnID != "ntr_"+pinNativeA+"_5" || exact.Creator == nil || exact.Creator.Label != "Operator" {
		t.Fatalf("exact pin = %#v", exact)
	}
	wantURL := "http://127.0.0.1:7690/api/v1/hosted-threads/" + f.hosted.ID + "/pins/" + exact.PinID + "/context?turns=10"
	if exact.ContextURL != wantURL || !strings.HasPrefix(exact.PinID, "pin_") {
		t.Fatalf("contextUrl = %q want %q", exact.ContextURL, wantURL)
	}
	latest := f.pin(t, map[string]any{"text": "pin this", "creatorThreadRef": f.hosted.ID})
	if latest.Anchor.ItemID != "nit_"+pinNativeA+"_t14-2" || latest.Creator == nil || latest.Creator.HostedThreadID != f.hosted.ID {
		t.Fatalf("latest pin = %#v", latest)
	}

	for _, tc := range []struct {
		body map[string]any
		code int
		err  string
	}{
		{map[string]any{"text": ""}, http.StatusBadRequest, "invalid_pin"},
		{map[string]any{"text": strings.Repeat("x", maxPinTextBytes+1)}, http.StatusBadRequest, "invalid_pin"},
		{map[string]any{"text": "x", "status": "active"}, http.StatusBadRequest, "invalid_pin"},
		{map[string]any{"text": "x", "itemId": "nit_" + pinNativeA + "_t5-1"}, http.StatusNotFound, "anchor_not_found"},
		{map[string]any{"text": "x", "itemId": "nit_nowhere"}, http.StatusNotFound, "anchor_not_found"},
		{map[string]any{"text": "x", "creatorThreadRef": "nht_unknown"}, http.StatusBadRequest, "invalid_creator"},
	} {
		code, raw := f.do(t, http.MethodPost, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins", tc.body)
		if code != tc.code || !strings.Contains(string(raw), tc.err) {
			t.Fatalf("%v: status %d body %s", tc.body, code, raw)
		}
	}
	code, _ := f.do(t, http.MethodPost, "/api/v1/hosted-threads/nht_unknown/pins", map[string]any{"text": "x"})
	if code != http.StatusNotFound {
		t.Fatalf("unknown hosted thread status = %d", code)
	}
}

func TestThreadPinClientActionIdempotency(t *testing.T) {
	f := newPinFixture(t)
	first := f.pin(t, map[string]any{"text": "retry me", "clientActionId": "a1"})
	seedPinTranscript(t, f.store, f.root, pinNativeA, 15, nil) // "latest" moves on
	again := f.pin(t, map[string]any{"text": "retry me", "clientActionId": "a1"})
	if again.PinID != first.PinID || again.Anchor.ItemID != first.Anchor.ItemID {
		t.Fatalf("retry = %#v first = %#v", again, first)
	}
	code, raw := f.do(t, http.MethodPost, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins", map[string]any{"text": "different", "clientActionId": "a1"})
	if code != http.StatusConflict || !strings.Contains(string(raw), "client_action_reused") {
		t.Fatalf("reuse status %d body %s", code, raw)
	}
}

func TestThreadPinListNewestFirstWithPagination(t *testing.T) {
	f := newPinFixture(t)
	var ids []string
	for turn := 1; turn <= 5; turn++ {
		ids = append(ids, f.pin(t, map[string]any{"text": fmt.Sprintf("topic %d", turn),
			"itemId": fmt.Sprintf("nit_%s_t%d-0", pinNativeA, turn)}).PinID)
	}
	var seen []string
	cursor := ""
	for page := 0; page < 5; page++ {
		path := "/api/v1/hosted-threads/" + f.hosted.ID + "/pins?limit=2"
		if cursor != "" {
			path += "&before=" + cursor
		}
		code, raw := f.do(t, http.MethodGet, path, nil)
		if code != http.StatusOK {
			t.Fatalf("list status %d body %s", code, raw)
		}
		list := decodePinJSON[threadPinListResponse](t, raw)
		if list.Order != "newest_first" {
			t.Fatalf("order = %q", list.Order)
		}
		for _, pin := range list.Pins {
			seen = append(seen, pin.PinID)
		}
		if !list.HasMore {
			if list.NextCursor != nil {
				t.Fatalf("last page cursor = %v", *list.NextCursor)
			}
			break
		}
		cursor = *list.NextCursor
	}
	want := []string{ids[4], ids[3], ids[2], ids[1], ids[0]}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("seen = %v want %v", seen, want)
	}
	for _, bad := range []string{"?limit=0", "?limit=101", "?before=x", "?before=-1"} {
		if code, _ := f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins"+bad, nil); code != http.StatusBadRequest {
			t.Fatalf("%s status = %d", bad, code)
		}
	}
	code, raw := f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins/"+ids[2], nil)
	if code != http.StatusOK || decodePinJSON[threadPinView](t, raw).Text != "topic 3" {
		t.Fatalf("read status %d body %s", code, raw)
	}
}

func TestThreadPinContextReturnsAnchorAndPrecedingTurns(t *testing.T) {
	f := newPinFixture(t)
	pin := f.pin(t, map[string]any{"text": "topic 12", "itemId": "nit_" + pinNativeA + "_t12-0"})
	code, raw := f.do(t, http.MethodGet, strings.TrimPrefix(pin.ContextURL, "http://127.0.0.1:7690"), nil)
	if code != http.StatusOK {
		t.Fatalf("context status %d body %s", code, raw)
	}
	context := decodePinJSON[threadPinContextResponse](t, raw)
	if context.TurnsRequested != 10 || context.TurnsReturned != 10 || len(context.Turns) != 11 ||
		context.Turns[0].Ordinal != 2 || !context.HasEarlierHistory || context.EarlierHistoryURL == "" {
		t.Fatalf("context = %+v", context)
	}
	anchorTurn := context.Turns[10]
	if !anchorTurn.IsAnchorTurn || len(anchorTurn.Messages) != 1 || !anchorTurn.Messages[0].IsAnchor ||
		anchorTurn.Messages[0].ItemID != "nit_"+pinNativeA+"_t12-0" {
		t.Fatalf("anchor turn = %+v", anchorTurn)
	}
	for _, turn := range context.Turns[:10] {
		if len(turn.Messages) != 2 || turn.Messages[0].Role != "user" || turn.Messages[1].Role != "agent" || turn.TurnID == "" {
			t.Fatalf("whole turn = %+v", turn)
		}
	}

	code, raw = f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins/"+pin.PinID+"/context?turns=20", nil)
	all := decodePinJSON[threadPinContextResponse](t, raw)
	if code != http.StatusOK || all.TurnsReturned != 11 || all.HasEarlierHistory || all.EarlierHistoryURL != "" {
		t.Fatalf("all context = %d %+v", code, all)
	}
	code, raw = f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins/"+pin.PinID+"/context?turns=2&format=text", nil)
	if code != http.StatusOK || !strings.Contains(string(raw), "[PINNED]") || !strings.Contains(string(raw), "=== turn 10") ||
		strings.Contains(string(raw), "=== turn 9 ") {
		t.Fatalf("text context %d:\n%s", code, raw)
	}
	for _, bad := range []string{"?turns=-1", "?turns=51", "?turns=x", "?format=xml"} {
		if code, _ := f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins/"+pin.PinID+"/context"+bad, nil); code != http.StatusBadRequest {
			t.Fatalf("%s status = %d", bad, code)
		}
	}
	if code, _ := f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins/pin_missing/context", nil); code != http.StatusNotFound {
		t.Fatalf("missing pin status = %d", code)
	}
}

// A provider-native replacement gives the hosted thread a new native id. Old
// pins stay listed and readable, new pins anchor in the new transcript, and a
// reopened pin store still has all of them.
func TestThreadPinsSurviveNativeReplacementAndReopen(t *testing.T) {
	f := newPinFixture(t)
	old := f.pin(t, map[string]any{"text": "before replacement", "itemId": "nit_" + pinNativeA + "_t3-2"})

	seedPinTranscript(t, f.store, f.root, pinNativeB, 3, nil)
	if err := hostedstore.SkipOwnedNativeClaim(f.executions.Control().ClaimNativeID(f.hosted.ID, "claude", pinNativeB, false)); err != nil {
		t.Fatal(err)
	}
	if err := f.executions.UpdateSession(f.hosted.ID, "nano-"+f.hosted.ID, pinNativeB, "ready", ""); err != nil {
		t.Fatal(err)
	}
	fresh := f.pin(t, map[string]any{"text": "after replacement"})
	if fresh.Anchor.NativeThreadID != pinNativeB || fresh.Anchor.ItemID != "nit_"+pinNativeB+"_t3-2" {
		t.Fatalf("fresh pin = %#v", fresh)
	}
	earlier := f.pin(t, map[string]any{"text": "pin the old analysis", "itemId": "nit_" + pinNativeA + "_t7-2"})
	if earlier.Anchor.NativeThreadID != pinNativeA {
		t.Fatalf("earlier-transcript pin = %#v", earlier)
	}

	if err := f.server.threadPins.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openThreadPinsStore(f.pinsPath)
	if err != nil {
		t.Fatal(err)
	}
	f.server.threadPins = reopened
	t.Cleanup(func() { _ = reopened.Close() })
	// Remount the handlers with the reopened store, as a new process does.
	f.mux = http.NewServeMux()
	mountRoutes(f.mux, applicationRoutes(fixtureOwners(f.server)))
	code, raw := f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+pinNativeB+"/pins", nil)
	list := decodePinJSON[threadPinListResponse](t, raw)
	if code != http.StatusOK || list.Count != 3 || list.Pins[0].PinID != earlier.PinID || list.Pins[2].PinID != old.PinID {
		t.Fatalf("list after replacement = %d %s", code, raw)
	}
	code, raw = f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins/"+old.PinID+"/context?turns=1", nil)
	context := decodePinJSON[threadPinContextResponse](t, raw)
	if code != http.StatusOK || context.Turns[len(context.Turns)-1].Messages[1].ItemID != "nit_"+pinNativeA+"_t3-2" {
		t.Fatalf("old context = %d %s", code, raw)
	}
}

// A rebuild that loses the anchored message is reported, never repaired: the
// pin keeps its text and still lists, and the monitor names it without
// changing anything.
func TestThreadPinBrokenAnchorIsReportedAndMonitorIsReadOnly(t *testing.T) {
	f := newPinFixture(t)
	kept := f.pin(t, map[string]any{"text": "kept", "itemId": "nit_" + pinNativeA + "_t2-2"})
	lost := f.pin(t, map[string]any{"text": "lost", "commentary": "why it mattered", "itemId": "nit_" + pinNativeA + "_t4-2"})
	f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins?limit=1", nil)
	f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins/"+kept.PinID+"/context", nil)

	seedPinTranscript(t, f.store, f.root, pinNativeA, 14, map[string]bool{pinNativeA + "-t4-2": true})
	code, raw := f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins/"+lost.PinID+"/context", nil)
	if code != http.StatusNotFound || !strings.Contains(string(raw), "pin_anchor_missing") || !strings.Contains(string(raw), "why it mattered") {
		t.Fatalf("broken context = %d %s", code, raw)
	}

	var before int
	if err := f.pinDB(t).QueryRow(`SELECT COUNT(*) FROM thread_pins`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	code, raw = f.do(t, http.MethodGet, "/api/v1/pins/monitor", nil)
	if code != http.StatusOK {
		t.Fatalf("monitor = %d %s", code, raw)
	}
	monitor := decodePinJSON[threadPinMonitorResponse](t, raw)
	if monitor.Accumulation.TotalPins != 2 || monitor.Accumulation.Threads != 1 || monitor.Retrieval.ListCalls != 1 ||
		monitor.Retrieval.ContextReads != 2 || monitor.Retrieval.BrokenContextReads != 1 || monitor.Retrieval.PinsNeverRead != 0 ||
		monitor.Retrieval.MaxReadRank != 2 || monitor.Anchors.Checked != 2 || len(monitor.Anchors.Broken) != 1 ||
		monitor.Anchors.Broken[0].PinID != lost.PinID || monitor.Anchors.Broken[0].Reason != "anchor_missing" {
		t.Fatalf("monitor = %s", raw)
	}
	var after int
	if err := f.pinDB(t).QueryRow(`SELECT COUNT(*) FROM thread_pins`).Scan(&after); err != nil || after != before {
		t.Fatalf("monitor changed pins: %d -> %d (%v)", before, after, err)
	}
	code, raw = f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins", nil)
	if code != http.StatusOK || decodePinJSON[threadPinListResponse](t, raw).Count != 2 {
		t.Fatalf("list after break = %d %s", code, raw)
	}
}

func TestThreadPinCandidatesListRecentMessagesNewestFirst(t *testing.T) {
	f := newPinFixture(t)
	code, raw := f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins/candidates?tail=3", nil)
	if code != http.StatusOK {
		t.Fatalf("candidates = %d %s", code, raw)
	}
	var body struct {
		Candidates []struct {
			ItemID string `json:"itemId"`
			Role   string `json:"role"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Candidates) != 3 || body.Candidates[0].ItemID != "nit_"+pinNativeA+"_t14-2" || body.Candidates[1].Role != "user" {
		t.Fatalf("candidates = %s", raw)
	}
}

// A pin recorded under another hosted record but anchored in a native id this
// hosted thread has claimed still belongs to this thread's list.
func TestThreadPinScopeIncludesPinsAnchoredInClaimedNativeIDs(t *testing.T) {
	f := newPinFixture(t)
	if err := f.appendPin(t, threadPinRecord{PinID: "pin_foreign", HostedThreadID: "nht_predecessor",
		Provider: "claude", NativeThreadID: pinNativeA, AnchorItemID: "nit_" + pinNativeA + "_t1-2", AnchorTurnID: "t",
		AnchorType: "agent_message", AnchorPreview: "p", Text: "carried over", CreatedAt: "2026-09-18T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := f.appendPin(t, threadPinRecord{PinID: "pin_unrelated", HostedThreadID: "nht_other",
		Provider: "claude", NativeThreadID: pinNativeB, AnchorItemID: "x", AnchorTurnID: "t",
		AnchorType: "agent_message", AnchorPreview: "p", Text: "not ours", CreatedAt: "2026-09-18T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	code, raw := f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins", nil)
	list := decodePinJSON[threadPinListResponse](t, raw)
	if code != http.StatusOK || list.Count != 1 || list.Pins[0].PinID != "pin_foreign" {
		t.Fatalf("scoped list = %d %s", code, raw)
	}
	if code, _ := f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins/pin_unrelated", nil); code != http.StatusNotFound {
		t.Fatalf("unrelated pin status = %d", code)
	}
}

// clientActionId is scoped per hosted thread, and a listed pin's context link
// names the hosted thread the caller asked through.
func TestThreadPinIdempotencyScopeAndContextURLFollowRequestThread(t *testing.T) {
	f := newPinFixture(t)
	seedPinTranscript(t, f.store, f.root, pinNativeB, 3, nil)
	other := seedHostedNative(t, f.executions, "claude", pinNativeB, "ready")
	f.pin(t, map[string]any{"text": "first thread", "clientActionId": "shared-key"})
	code, raw := f.do(t, http.MethodPost, "/api/v1/hosted-threads/"+other.ID+"/pins", map[string]any{"text": "second thread", "clientActionId": "shared-key"})
	if code != http.StatusCreated {
		t.Fatalf("same key on another thread = %d %s", code, raw)
	}

	if err := f.appendPin(t, threadPinRecord{PinID: "pin_predecessor", HostedThreadID: "nht_predecessor",
		Provider: "claude", NativeThreadID: pinNativeA, AnchorItemID: "nit_" + pinNativeA + "_t1-2", AnchorTurnID: "t",
		AnchorType: "agent_message", AnchorPreview: "p", Text: "written earlier", CreatedAt: "2026-09-18T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	code, raw = f.do(t, http.MethodGet, "/api/v1/hosted-threads/"+f.hosted.ID+"/pins/pin_predecessor", nil)
	view := decodePinJSON[threadPinView](t, raw)
	if code != http.StatusOK || !strings.Contains(view.ContextURL, "/hosted-threads/"+f.hosted.ID+"/pins/pin_predecessor/context") {
		t.Fatalf("predecessor pin = %d %s", code, raw)
	}
	code, _ = f.do(t, http.MethodGet, strings.TrimPrefix(view.ContextURL, "http://127.0.0.1:7690"), nil)
	if code != http.StatusOK {
		t.Fatalf("generated context link status = %d", code)
	}
}

package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/catalog"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

// A harness notification can wake Claude without Nano sending a user line.
// The first provider execution line must open a fresh runtime/projection turn,
// and the ordinary result line remains the single terminal authority.
func TestClaudeProviderExecutionEvidenceTracksNotificationStartedTurn(t *testing.T) {
	fixture := newProviderRuntimeFixture(t, sessions.Config{ID: "notification", Provider: "claude"}, "notification-native", "claude-test")
	sess := fixture.session
	observer := observeProjection(t, sess)
	fixture.emitAt(t, []byte(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"notification-turn"}}}`), time.Time{}, "claude:notification:1")
	waitReconnectCondition(t, func() bool { return sess.View().Summary().TurnActive })
	if sum := sess.View().Summary(); !sum.TurnActive || sum.TurnStartedAtMs == 0 {
		t.Fatalf("execution evidence summary = %+v, want an anchored active turn", sum)
	}
	snapshot := observer.Snapshot()
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].Status != "in_progress" || !snapshot.Runtime.TurnActive {
		t.Fatalf("execution evidence projection = turns %+v runtime %+v", snapshot.Turns, snapshot.Runtime)
	}

	fixture.emitAt(t, []byte(`{"type":"result","subtype":"success"}`), time.Time{}, "claude:notification:1")
	waitReconnectCondition(t, func() bool { return !sess.View().Summary().TurnActive })
	if sum := sess.View().Summary(); sum.TurnActive {
		t.Fatalf("terminal result left notification-started turn active: %+v", sum)
	}
}

// TestRuntimeOverlayOnListAndDetail covers the frozen runtime overlay contract
// on both catalog responses: present with attached/sessionId/status/turnActive
// while a session matches provider+nativeId, absent when detached and when the
// runtime source is nil.
func TestRuntimeOverlayOnListAndDetail(t *testing.T) {
	fake := &catalogRowsFixture{
		page: sidebar.CatalogPage{Items: []sidebar.Thread{
			{Thread: threadindex.Thread{ID: "nth_1", Provider: "codex", NativeID: "native-live", Kind: "main", UpdatedAt: strPtr("2026-07-28T12:00:01Z")}},
			{Thread: threadindex.Thread{ID: "nth_2", Provider: "claude", NativeID: "native-idle", Kind: "main", UpdatedAt: strPtr("2026-07-28T12:00:00Z")}},
		}},
		thread: sidebar.Thread{Thread: threadindex.Thread{ID: "nth_1", Provider: "codex", NativeID: "native-live", Kind: "main"}},
	}
	s := &server{catalog: (fake).source(t), Registry: newSessionRegistry()}
	fixture := newProviderRuntimeFixture(t, sessions.Config{ID: "s1", Provider: "codex"}, "native-live", "model")
	fixture.emit(t, []byte(`{"method":"turn/started","params":{"threadId":"native-live","turn":{"id":"turn-1","status":"inProgress"}}}`))
	waitReconnectCondition(t, func() bool { return fixture.session.View().Summary().TurnActive })
	s.RegisterSession(fixture.session)

	assertRuntime := func(t *testing.T, raw json.RawMessage, wantTurnActive bool) {
		t.Helper()
		var runtime sidebar.RuntimeInfo
		if err := json.Unmarshal(raw, &runtime); err != nil {
			t.Fatal(err)
		}
		if !runtime.Attached || runtime.SessionID != "s1" || runtime.Status != "ready" || runtime.TurnActive != wantTurnActive {
			t.Fatalf("runtime = %#v", runtime)
		}
	}

	rec := httptest.NewRecorder()
	fixtureOwners(s).catalogViews().HandleThreads(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads", nil))
	var list struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("items = %d", len(list.Items))
	}
	assertRuntime(t, list.Items[0]["runtime"], true)
	if _, present := list.Items[1]["runtime"]; present {
		t.Fatal("detached thread carried a runtime field")
	}

	rec = httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_1", nil))
	var detail map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	assertRuntime(t, detail["runtime"], true)

	// turnActive flips are reflected on the next response.
	fixture.emit(t, []byte(`{"method":"turn/completed","params":{"threadId":"native-live","turn":{"id":"turn-1","status":"completed"}}}`))
	waitReconnectCondition(t, func() bool { return !fixture.session.View().Summary().TurnActive })
	rec = httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_1", nil))
	detail = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	assertRuntime(t, detail["runtime"], false)

	// Detached (session closed): the field disappears entirely.
	s.RemoveSession("s1")
	rec = httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_1", nil))
	detail = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if _, present := detail["runtime"]; present {
		t.Fatal("closed session left a runtime field on detail")
	}

	// Nil runtime source (catalog wired without sessions): never present. The
	// fake reuses its page value across calls, so clear the stamp the earlier
	// list request left (the real service builds fresh items per call).
	s.sidebar = sidebar.New(sidebar.Sources{})
	fake.page.Items[0].Runtime = nil
	s.RegisterSession(fixture.session)
	rec = httptest.NewRecorder()
	fixtureOwners(s).catalogViews().HandleThreads(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads", nil))
	list.Items = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if _, present := list.Items[0]["runtime"]; present {
		t.Fatal("nil runtime source still produced a runtime field")
	}
}

// TestRuntimeForThreeStates pins the overlay's three answers per thread —
// attached (a live session hosts it), detached (no live session but a
// remembered attachment survives), and absent (neither) — provider-generic
// across codex and claude records. The exited case is the post-crash reality:
// a dead session lingering in the map must not read as attached, and must not
// hide the detached record behind it.
func TestRuntimeForThreeStates(t *testing.T) {
	fixtureOwners(
		// A nil runtime hub leaves the application publisher inert.
		(&server{})).attachmentCoordinator().NotifyRuntime()
	attachments := newTestStore(t)
	attachments.Remember(hostedstore.Attachment{Provider: "codex", NativeThreadID: "codex-detached", Cwd: "/a", AttachedAt: "2026-07-27T10:00:00Z"})
	attachments.Remember(hostedstore.Attachment{Provider: "claude", NativeThreadID: "claude-detached", Cwd: "/b", AttachedAt: "2026-07-27T11:00:00Z"})
	attachments.Remember(hostedstore.Attachment{Provider: "claude", NativeThreadID: "claude-exited", Cwd: "/c", AttachedAt: "2026-07-27T12:00:00Z"})

	active := newProviderRuntimeFixture(t, sessions.Config{ID: "s1", Provider: "codex"}, "codex-live", "model")
	active.emit(t, []byte(`{"method":"turn/started","params":{"threadId":"codex-live","turn":{"id":"turn-1","status":"inProgress","startedAt":1787600000}}}`))
	waitReconnectCondition(t, func() bool { return active.session.View().Summary().TurnActive })
	s := testServerWithSessions(&server{attachments: attachments, Registry: newSessionRegistry()}, map[string]*sessions.Session{
		"s1": active.session,
		"s2": memorySession(t, "s2", "claude", "claude-live", "starting", "", "", time.Time{}),
		"s3": memorySession(t, "s3", "claude", "claude-exited", "exited", "", "", time.Time{}),
		"s4": memorySession(t, "s4", "codex", "codex-gone", "exited", "", "", time.Time{}),
	})

	cases := []struct {
		name     string
		provider string
		nativeID string
		want     sidebar.RuntimeInfo
		wantOK   bool
	}{
		{"codex attached working", "codex", "codex-live", sidebar.RuntimeInfo{Attached: true, SessionID: "s1", Status: "ready", TurnActive: true, TurnStartedAtMs: 1787600000000, ThreadStatus: catalog.FleetWorking, PresentationMode: presentationShared}, true},
		{"claude attached starting", "claude", "claude-live", sidebar.RuntimeInfo{Attached: true, SessionID: "s2", Status: "starting", ThreadStatus: catalog.FleetIdle, PresentationMode: presentationShared}, true},
		{"codex detached", "codex", "codex-detached", sidebar.RuntimeInfo{Detached: true}, true},
		{"claude detached", "claude", "claude-detached", sidebar.RuntimeInfo{Detached: true}, true},
		{"exited session with record reads detached", "claude", "claude-exited", sidebar.RuntimeInfo{Detached: true}, true},
		{"exited session without record reads absent", "codex", "codex-gone", sidebar.RuntimeInfo{}, false},
		{"unknown thread", "codex", "never-seen", sidebar.RuntimeInfo{}, false},
		{"wrong provider for the record", "codex", "claude-detached", sidebar.RuntimeInfo{}, false},
		{"empty native id", "codex", "", sidebar.RuntimeInfo{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := runtimeForTest(t, s, tc.provider, tc.nativeID)
			if !reflect.DeepEqual(got, tc.want) || ok != tc.wantOK {
				t.Fatalf("RuntimeFor(%q, %q) = %#v, %v; want %#v, %v", tc.provider, tc.nativeID, got, ok, tc.want, tc.wantOK)
			}
		})
	}

	// A server without an attachment store simply has no detached state.
	bare := &server{Registry: newSessionRegistry()}
	if info, ok := runtimeForTest(t, bare, "codex", "codex-detached"); ok || !reflect.DeepEqual(info, sidebar.RuntimeInfo{}) {
		t.Fatalf("nil attachment store = %#v, %v", info, ok)
	}
}

// TestDetachedOverlayJSON pins the wire spelling of the detached shape: the
// frontend keys on `detached`, and a detached row must not carry invented
// session fields.
func TestDetachedOverlayJSON(t *testing.T) {
	attachments := newTestStore(t)
	attachments.Remember(hostedstore.Attachment{Provider: "codex", NativeThreadID: "native-1", Cwd: "/a", AttachedAt: "2026-07-27T10:00:00Z"})
	s := &server{
		catalog:     (&catalogRowsFixture{thread: sidebar.Thread{Thread: threadindex.Thread{ID: "nth_1", Provider: "codex", NativeID: "native-1", Kind: "main"}}}).source(t),
		attachments: attachments,
		Registry:    newSessionRegistry(),
	}

	rec := httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_1", nil))
	var detail map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if got := string(detail["runtime"]); got != `{"attached":false,"detached":true}` {
		t.Fatalf("detached runtime JSON = %s", got)
	}
}

// TestAttachmentDismissNudgesRuntime: dismissing a detached row retires a
// runtime overlay, so open Explorers must be told to refetch — the same nudge
// bind and unbind fire.
func TestAttachmentDismissNudgesRuntime(t *testing.T) {
	s := &server{attachments: newTestStore(t), runtimeEvents: sidebar.NewRuntimeHub(), Registry: newSessionRegistry()}
	s.attachments.Remember(hostedstore.Attachment{Provider: "codex", NativeThreadID: "t-1", AttachedAt: "2026-07-27T10:00:00Z"})
	nudges := s.runtimeEvents.Subscribe()

	rec := httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/attachments/t-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss status = %d", rec.Code)
	}
	select {
	case <-nudges:
	default:
		t.Fatal("dismiss did not nudge the runtime stream")
	}
}

// TestRuntimeEventsOnStream drives the full chain over a live SSE connection:
// bind, turnActive flip, and unbind each emit a compact runtime delta, while
// catalog events keep their version-only payload.
func TestRuntimeEventsOnStream(t *testing.T) {
	fakeCodex := newFakeAppServer(t)
	catalogFixture := &catalogEventFixture{catalogRowsFixture: &catalogRowsFixture{}, version: 5}
	s := newResumeTestServer(t, fakeCodex.WSURL())
	s.catalog = catalogFixture.source(t)

	srv := httptest.NewServer(http.HandlerFunc(fixtureOwners(s).catalogViews().HandleEvents))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)

	if event, data := readSSEEvent(t, reader); event != "catalog" || data != `{"catalogVersion":5,"refresh":true}` {
		t.Fatalf("connect event = %q %q", event, data)
	}

	// Bind: spawning a resume session nudges the stream while the handshake
	// is still in flight (async spawn, status starting). The reply delay
	// separates the bind nudge from the starting→ready nudge deterministically.
	const threadID = "0199a213-abcd-7000-8000-00000000000a"
	fakeCodex.SetReplyDelay("thread/resume", 200*time.Millisecond)
	rec := postResume(t, s, threadID)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sum sessions.Summary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Status != "starting" {
		t.Fatalf("spawn summary status = %q, want starting", sum.Status)
	}
	if event, data := readSSEEvent(t, reader); event != "runtime" || data != `{"rows":[],"seq":1}` {
		t.Fatalf("bind event = %q %q, want a runtime delta naming no thread", event, data)
	}

	// Creation/provider metadata can publish independent runtime wakeups. Skip
	// those lossy hints and require the real ready attachment delta.
	readRuntimeUntil(t, reader, "ready", func(delta sidebar.RuntimeDelta) bool {
		return len(delta.Sessions) == 1 && delta.Sessions[0].ThreadID == threadID && len(delta.Rows) == 1 && delta.Rows[0].NativeID == threadID
	})
	if got := waitForSessionStatus(t, s, sum.ID, "ready"); got.ThreadID != threadID {
		t.Fatalf("ready summary = %#v", got)
	}

	// turnActive flip.
	fakeCodex.EmitThreadNotification(threadID, map[string]any{"method": "turn/started", "params": map[string]any{"threadId": threadID}})
	readRuntimeUntil(t, reader, "turnActive", func(delta sidebar.RuntimeDelta) bool {
		return !delta.Refresh && len(delta.Rows) == 1 && delta.Rows[0].Runtime != nil && delta.Rows[0].Runtime.TurnActive
	})

	// An ordinary catalog update carries the row that needs repair.
	if err := catalogFixture.stored.source.AssignThreadName("codex", "native-changed", "renamed"); err != nil {
		t.Fatal(err)
	}
	if event, data := readSSEEvent(t, reader); event != "catalog" || data != `{"catalogVersion":6,"refresh":true}` {
		t.Fatalf("name refresh event = %q %q", event, data)
	}
	// A newly indexed provider file publishes the changed row on the same stream.
	changedPath := filepath.Join(catalogFixture.stored.root.Root.Path, "rollout-native-stream.jsonl")
	writeSettlementFile(t, changedPath, `{"type":"session_meta","payload":{"id":"native-stream","timestamp":"2026-07-31T12:00:00Z","cwd":"/work"}}
{"type":"event_msg","payload":{"type":"user_message","message":"stream fixture"}}
`)
	catalogFixture.stored.source.RequestRefresh()
	var changedID string
	waitForCatalog(t, func() bool {
		id, err := catalogFixture.stored.source.ResolveThreadReference(context.Background(), "native-stream")
		changedID = id
		return err == nil
	})
	for {
		event, data := readSSEEvent(t, reader)
		if event != "catalog" {
			continue
		}
		var change struct {
			ThreadIDs []string `json:"threadIds"`
		}
		if err := json.Unmarshal([]byte(data), &change); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(change.ThreadIDs, changedID) {
			break
		}
	}

	// Unbind: closing the session nudges the stream.
	rec = httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sum.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d", rec.Code)
	}
	if event, data := readSSEEvent(t, reader); event != "runtime" {
		t.Fatalf("unbind event = %q %q, want runtime delta", event, data)
	} else {
		var delta sidebar.RuntimeDelta
		err := json.Unmarshal([]byte(data), &delta)
		// The closed session is listed as exited until the registry drops it.
		listed := slices.Equal(delta.RemovedSessions, []string{sum.ID}) ||
			len(delta.Sessions) == 1 && delta.Sessions[0].ID == sum.ID && delta.Sessions[0].Status == sessionStatusExited
		if err != nil || delta.Refresh || !listed || len(delta.Rows) != 1 || delta.Rows[0].Runtime != nil {
			t.Fatalf("unbind delta = %q / %#v / %v", data, delta, err)
		}
	}

	// A page whose list read came just before the latest delta connects from
	// that position: its first runtime event replays what it missed, up to now,
	// instead of a refresh that would read the list again.
	at := s.runtimeEvents.Position()
	query := url.Values{"runtimeGeneration": {at.Generation}, "runtimeSeq": {strconv.FormatUint(at.Seq-1, 10)}}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Body.Close()
	readRuntimeUntil(t, bufio.NewReader(replay.Body), "replay", func(delta sidebar.RuntimeDelta) bool {
		if delta.Refresh || delta.Seq < at.Seq {
			t.Fatalf("first runtime event after connecting from seq %d = %+v, want the replay through seq %d", at.Seq-1, delta, at.Seq)
		}
		return true
	})
}

func readRuntimeUntil(t *testing.T, reader *bufio.Reader, want string, matches func(sidebar.RuntimeDelta) bool) {
	t.Helper()
	for attempts := 0; attempts < 8; attempts++ {
		event, data := readSSEEvent(t, reader)
		if event != "runtime" {
			continue
		}
		var delta sidebar.RuntimeDelta
		if err := json.Unmarshal([]byte(data), &delta); err != nil {
			t.Fatalf("%s runtime JSON %q: %v", want, data, err)
		}
		if matches(delta) {
			return
		}
	}
	t.Fatalf("did not receive %s runtime transition after metadata-only wakeups", want)
}

// Failure caught (2026-10-19, served after a restart): each change event of
// the re-attach carried the whole detached list, which then held nearly every
// thread, about 4.5 MB per browser over three minutes. Boundary: the runtime
// delta. One thread re-attaching sends its own row and key, not the list.
func TestRuntimeDeltaSendsOnlyTheDetachedRowsThatChanged(t *testing.T) {
	now := time.Now()
	s := &server{attachments: newTestStore(t), Registry: newSessionRegistry()}
	for i := range 40 {
		s.attachments.Remember(hostedstore.Attachment{Provider: "codex", NativeThreadID: fmt.Sprintf("parked-%02d", i), AttachedAt: "2026-07-27T10:00:00Z"})
	}
	runtimeSnapshotForTest(t, s) // the first snapshot only records the baseline
	s.RegisterSession(memorySession(t, "s-0", "codex", "parked-00", "ready", "/w", "gpt-5.6-sol", now))
	attached := runtimeSnapshotForTest(t, s)
	if attached.Refresh || len(attached.DetachedRows) != 0 || !slices.Equal(attached.RemovedDetached, []string{"codex:parked-00"}) {
		t.Fatalf("one re-attach = refresh:%v detached rows %d removed %v, want only codex:parked-00 removed", attached.Refresh, len(attached.DetachedRows), attached.RemovedDetached)
	}
	if len(attached.Sessions) != 1 || attached.Sessions[0].ID != "s-0" {
		t.Fatalf("one re-attach sessions = %+v, want s-0's row", attached.Sessions)
	}

	// A burst beyond the bound reaches a browser as one refresh marker with no
	// rows of any kind, runtime overlays included: first 33 of the parked
	// threads re-attaching together (33 sessions + 33 detached keys, only 33
	// runtime rows), then a burst of new sessions.
	if s.runtimeEvents == nil {
		s.runtimeEvents = sidebar.NewRuntimeHub()
	}
	browser := s.runtimeEvents.Subscribe()
	defer s.runtimeEvents.Unsubscribe(browser)
	emptyRefresh := func(event sidebar.RuntimeDelta) bool {
		return event.Refresh && event.Rebuild && !event.SidebarCurrent && event.Rows != nil &&
			len(event.Rows)+len(event.Sessions)+len(event.RemovedSessions)+len(event.DetachedRows)+len(event.RemovedDetached) == 0
	}
	for i := 1; i <= 33; i++ {
		id := fmt.Sprintf("r-%d", i)
		s.RegisterSession(memorySession(t, id, "codex", fmt.Sprintf("parked-%02d", i), "ready", "/w", "gpt-5.6-sol", now))
	}
	// Published by a star update (already current for its own change), the
	// re-attach still needs the sidebar rebuilt.
	fixtureOwners(s).sidebarOwner().PublishRuntimeDelta(true, true)
	if reattach := <-browser; !emptyRefresh(reattach) {
		t.Fatalf("33 re-attaches = refresh:%v rows %d sessions %d removed detached %d, want an empty refresh marker",
			reattach.Refresh, len(reattach.Rows), len(reattach.Sessions), len(reattach.RemovedDetached))
	}
	for i := 1; i <= 65; i++ {
		id := fmt.Sprintf("s-%d", i)
		s.RegisterSession(memorySession(t, id, "codex", fmt.Sprintf("burst-%02d", i), "ready", "/w", "gpt-5.6-sol", now))
	}
	fixtureOwners(s).sidebarOwner().PublishRuntimeDelta(false, false)
	if burst := <-browser; !emptyRefresh(burst) {
		t.Fatalf("burst event = refresh:%v Rebuild:%v rows %d sessions %d, want an empty refresh marker", burst.Refresh, burst.Rebuild, len(burst.Rows), len(burst.Sessions))
	}
}

// readSSEEvent reads the next named SSE event (event: X then data: Y),
// skipping heartbeats and blank separators.
func readSSEEvent(t *testing.T, reader *bufio.Reader) (event, data string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE line: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		if !strings.HasPrefix(line, "event: ") {
			continue
		}
		event = strings.TrimPrefix(line, "event: ")
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE data line: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("event %q not followed by data, got %q", event, line)
		}
		return event, strings.TrimPrefix(line, "data: ")
	}
	t.Fatal("no SSE event within deadline")
	return "", ""
}

// A session without a native id has no runtime row, so before this rule a
// spawn that failed pre-identity reached browsers only if something else
// happened to trigger a full session-list read. Since 2026-10-19 the delta
// carries that session's list row instead of asking every browser to re-read
// every session.
func TestRuntimeDeltaCarriesSessionsWithoutRows(t *testing.T) {
	pending := newFixtureSession(runtimeFixtureConfig(t, sessions.Config{ID: "pending", Provider: "codex", HostedThreadID: "fixture-pending"}), "")
	if err := pending.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	s := &server{Registry: newSessionRegistry()}
	runtimeSnapshotForTest(t, s) // the first snapshot only records the baseline

	carries := func(delta sidebar.RuntimeDelta) bool {
		return !delta.Refresh && len(delta.Sessions) == 1 && delta.Sessions[0].ID == "pending"
	}
	s.RegisterSession(pending)
	if joined := runtimeSnapshotForTest(t, s); !carries(joined) {
		t.Fatalf("a new session without a native id = %+v, want its list row and no re-read", joined)
	}
	if same := runtimeSnapshotForTest(t, s); len(same.Sessions) != 0 || same.Refresh || len(same.DetachedRows) != 0 {
		t.Fatalf("an unchanged session set = %+v, want nothing", same)
	}
	fake := newFakeAppServer(t)
	fake.SetStartResult(map[string]any{"thread": map[string]any{"id": "temporary-native"}})
	if err := pending.Runtime().BeginProviderSpawn(sessions.ProviderRequest{Provider: "codex", Mode: "start"}, fake.daemon(), func() {}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pending.Lifecycle().Close)
	waitReconnectCondition(t, func() bool { return pending.View().Summary().Status == "ready" })
	pending.ReserveNativeID("")
	if changed := runtimeSnapshotForTest(t, s); !carries(changed) || changed.Sessions[0].Status != "ready" {
		t.Fatalf("a status change of a session without a native id = %+v, want its new row", changed)
	}
}

// A spawn that fails before Claude or Codex names its thread stays listed with
// its error (failSpawn's contract). Carried as a removal, the page dropped the
// row and detached its tab, losing the only place the error shows.
func TestRuntimeDeltaKeepsAFailedSpawnListed(t *testing.T) {
	pending := newFixtureSession(runtimeFixtureConfig(t, sessions.Config{ID: "pending", Provider: "codex", HostedThreadID: "fixture-pending"}), "")
	if err := pending.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	s := testServerWithSessions(&server{Registry: newSessionRegistry()}, map[string]*sessions.Session{"pending": pending})
	runtimeSnapshotForTest(t, s) // the first snapshot only records the baseline

	fake := newFakeAppServer(t)
	fake.SetStartError("handshake failed")
	if err := pending.Runtime().BeginProviderSpawn(sessions.ProviderRequest{Provider: "codex", Mode: "start"}, fake.daemon(), func() {}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pending.Lifecycle().Close)
	waitReconnectCondition(t, func() bool { return pending.View().Summary().Status == "exited" })
	failed := runtimeSnapshotForTest(t, s)
	if len(failed.RemovedSessions) != 0 || len(failed.Sessions) != 1 || failed.Sessions[0].Status != sessionStatusExited || failed.Sessions[0].Error != "thread/start: rpc error -32000: handshake failed" {
		t.Fatalf("failed spawn delta = sessions %+v removed %v, want its exited row with the error", failed.Sessions, failed.RemovedSessions)
	}
	if again := runtimeSnapshotForTest(t, s); len(again.Sessions) != 0 || len(again.RemovedSessions) != 0 {
		t.Fatalf("an unchanged failed session = %+v, want nothing", again)
	}
}

// The runtime snapshot that follows every boot registration read each detached
// thread's presentation mode under its own execution-store lock, while
// registrations hold that lock across their control-store writes. At a
// restart it waited once per waiting thread: 21 s of the boot loop's first
// 30 s. It reads them all under one lock. Boundary: the snapshot over real
// attachment and execution stores while a writer keeps taking the lock.
func TestRuntimeSnapshotWaitsForTheExecutionStoreOnce(t *testing.T) {
	s, store := bootReattachServer(t, "", "")
	migrateTestControl(t, s.controlMetadata)
	const threads = 200
	for i := 0; i < threads; i++ {
		rec := bootCodexRecord(t)
		rec.NativeThreadID = fmt.Sprintf("codex-thread-%d", i)
		store.Remember(rec)
	}
	seedBootAttachments(t, s)
	// Several registrations and requests take the lock in turn, as at a
	// restart, so a waiter always queues behind someone.
	stop := make(chan struct{})
	held := make(chan struct{}, 1)
	var writers sync.WaitGroup
	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				fixtureExecutionLocked(t, s.agentExecutions, func() {
					select {
					case held <- struct{}{}:
					default:
					}
					time.Sleep(5 * time.Millisecond)
				})
			}
		}()
	}
	<-held // time the snapshot only once the store is busy
	started := time.Now()
	snapshot := fixtureOwners(s).sidebarOwner().SidebarRuntimeSnapshot()
	took := time.Since(started)
	close(stop)
	writers.Wait()
	rows := make([]sidebar.Thread, threads)
	for i := range rows {
		rows[i].Provider = "codex"
		rows[i].NativeID = fmt.Sprintf("codex-thread-%d", i)
	}
	fixtureOwners(s).sidebarOwner().ComposeCatalogThreads(context.Background(), rows, snapshot, true)
	for i, row := range rows {
		if row.Runtime == nil || !row.Runtime.Detached {
			t.Fatalf("snapshot row %d lost its detached overlay: %+v", i, row.Runtime)
		}
	}
	if took > time.Second {
		t.Fatalf("snapshot of %d detached threads took %s behind a busy execution store", threads, took)
	}
}

// Every delta used to carry every held thread. A delta names only the threads whose runtime changed, and
// names a thread Nano released with no overlay.
func TestRuntimeDeltaCarriesOnlyChangedRows(t *testing.T) {
	now := time.Now()
	fixture := newProviderRuntimeFixture(t, sessions.Config{ID: "sess-a", Provider: "codex", CWD: "/w/a", CreatedAt: now}, "thread-a", "gpt-5.6-sol")
	a := fixture.session
	b := memorySession(t, "sess-b", "codex", "thread-b", "ready", "/w/b", "gpt-5.6-sol", now)
	s := testServerWithSessions(&server{Registry: newSessionRegistry()}, map[string]*sessions.Session{"sess-a": a, "sess-b": b})
	if first := runtimeSnapshotForTest(t, s); len(first.Rows) != 2 {
		t.Fatalf("first delta rows = %d, want every held thread", len(first.Rows))
	}
	if same := runtimeSnapshotForTest(t, s); len(same.Rows) != 0 {
		t.Fatalf("an unchanged runtime produced %d rows", len(same.Rows))
	}
	fixture.emit(t, []byte(`{"method":"turn/started","params":{"threadId":"thread-a","turn":{"id":"turn-a","status":"inProgress"}}}`))
	waitReconnectCondition(t, func() bool { return a.View().Summary().TurnActive })
	flip := runtimeSnapshotForTest(t, s)
	if len(flip.Rows) != 1 || flip.Rows[0].NativeID != "thread-a" || flip.Rows[0].Runtime == nil || !flip.Rows[0].Runtime.TurnActive {
		t.Fatalf("turnActive flip delta = %#v, want only thread-a", flip.Rows)
	}
	s.RemoveSession("sess-b")
	gone := runtimeSnapshotForTest(t, s)
	if gone.Refresh || !slices.Equal(gone.RemovedSessions, []string{"sess-b"}) || len(gone.Rows) != 1 || gone.Rows[0].NativeID != "thread-b" || gone.Rows[0].Runtime != nil {
		t.Fatalf("release delta = refresh:%v removed:%v %#v, want sess-b removed and thread-b with no overlay", gone.Refresh, gone.RemovedSessions, gone.Rows)
	}
}

// runtimeSnapshotForTest observes the delta at the published event boundary.
func runtimeSnapshotForTest(t *testing.T, s *server) sidebar.RuntimeDelta {
	t.Helper()
	if s.runtimeEvents == nil {
		s.runtimeEvents = sidebar.NewRuntimeHub()
		s.sidebar = nil
	}
	ch := s.runtimeEvents.Subscribe()
	defer s.runtimeEvents.Unsubscribe(ch)
	fixtureOwners(s).sidebarOwner().PublishRuntimeDelta(false, false)
	select {
	case delta := <-ch:
		return delta
	case <-time.After(time.Second):
		t.Fatal("runtime snapshot was not published")
	}
	return sidebar.RuntimeDelta{}
}

// runtimeForTest reads the same composed catalog overlay as an HTTP caller.
func runtimeForTest(t *testing.T, s *server, provider, native string) (sidebar.RuntimeInfo, bool) {
	t.Helper()
	row := sidebar.Thread{Thread: threadindex.Thread{Provider: provider, NativeID: native}}
	fixtureOwners(s).sidebarOwner().AttachRuntimeOverlay(&row)
	if row.Runtime == nil {
		return sidebar.RuntimeInfo{}, false
	}
	return *row.Runtime, true
}

package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/config"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/testsupport/claudeshim"
)

func causationTestSession(t *testing.T, hosted string) (*sessions.Session, *hostedstore.MetadataStore) {
	return causationTestSessionForProvider(t, hosted, "codex")
}

func causationTestSessionForProvider(t *testing.T, hosted, provider string) (*sessions.Session, *hostedstore.MetadataStore) {
	t.Helper()
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread(hosted, provider, presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	generation, err := store.BeginTransportBinding(hosted)
	if err != nil {
		t.Fatal(err)
	}
	sess := newSession(runtimeFixtureConfig(t, sessions.Config{ID: "sess_" + hosted, Provider: provider, HostedThreadID: hosted, Metadata: store, Projection: newLiveProjection(provider, hosted)}))
	if err := sess.InitializeBinding(generation); err != nil {
		t.Fatal(err)
	}
	sess.ReserveNativeID("native")
	t.Cleanup(sess.Lifecycle().Close)
	return sess, store
}

func requested(t *testing.T, sess *sessions.Session, store *hostedstore.MetadataStore, field, value string) int64 {
	t.Helper()
	if sess.Provider() == "codex" && value != "ultrafast" {
		body, _ := json.Marshal(map[string]string{field: value})
		response := httptest.NewRecorder()
		sess.State().HandleTurnOptionsUpdate(response, httptest.NewRequest(http.MethodPatch, "/turn-options", strings.NewReader(string(body))))
		if response.Code != http.StatusOK {
			t.Fatalf("requested %s: %d %s", field, response.Code, response.Body.String())
		}
	} else {
		if _, err := store.AppendEvent(hostedstore.MetadataEvent{HostedThreadID: sess.HostedThreadID(), Field: field, Value: value, Disposition: metadataDispositionRequested, Source: postureSourceOperator, Evidence: "test", Freshness: metadataFreshnessLive}); err != nil {
			t.Fatal(err)
		}
	}
	view, err := store.View(sess.HostedThreadID())
	if err != nil {
		t.Fatal(err)
	}
	return *view.Fields[field].DesiredEventID
}

func startCausationCodex(t *testing.T, sess *sessions.Session) *fakeAppServer {
	t.Helper()
	fake := newFakeAppServer(t)
	fake.SetResumeResult(map[string]any{"thread": map[string]any{"id": sess.State().ThreadID()}})
	fake.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "causal-turn"}})
	if err := sess.Runtime().BeginProviderSpawn(sessions.ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: sess.State().ThreadID()}, fake.daemon(), func() {}); err != nil {
		t.Fatal(err)
	}
	waitForClaudeStatus(t, sess, "ready")
	return fake
}
func sendCausationTurn(t *testing.T, sess *sessions.Session) {
	t.Helper()
	if response := postTurn(t, sess.Delivery(), context.Background(), "causation-turn", "test prompt"); response.Code != http.StatusOK {
		t.Fatalf("causal turn: %d %s", response.Code, response.Body.String())
	}
}
func emitCausation(t *testing.T, sess *sessions.Session, fake *fakeAppServer, payload json.RawMessage) {
	t.Helper()
	line := append([]byte(`{"method":"thread/settings/updated","params":`), payload...)
	line = append(line, '}')
	(&providerRuntimeFixture{session: sess, codex: fake}).emitAndDrain(t, line)
}
func startCausationResponse(t *testing.T, sess *sessions.Session, request sessions.ProviderRequest, values map[string]any, before func(json.RawMessage)) {
	t.Helper()
	fake := newFakeAppServer(t)
	values["thread"] = map[string]any{"id": sess.State().ThreadID()}
	fake.SetStartResult(values)
	if before != nil {
		fake.SetOnStart(before)
	}
	request.Provider, request.Mode = "codex", "start"
	if err := sess.Runtime().BeginProviderSpawn(request, fake.daemon(), func() {}); err != nil {
		t.Fatal(err)
	}
	waitForClaudeStatus(t, sess, "ready")
}

func fieldStrings(t *testing.T, store *hostedstore.MetadataStore, hosted, field string) (hostedstore.MetadataCurrent, string, string) {
	t.Helper()
	v, err := store.View(hosted)
	if err != nil {
		t.Fatal(err)
	}
	f := v.Fields[field]
	var desired, effective string
	_ = json.Unmarshal(f.DesiredValue, &desired)
	_ = json.Unmarshal(f.EffectiveValue, &effective)
	return f, desired, effective
}

func finishCausationTurn(t *testing.T, sess *sessions.Session, fake *fakeAppServer, actionID string) {
	t.Helper()
	record, _, err := sess.Delivery().PromptStore().Get(context.Background(), actionID)
	if err != nil || record.ProviderTurnID == "" {
		t.Fatalf("accepted turn %q has no provider identity: %+v %v", actionID, record, err)
	}
	line, _ := json.Marshal(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": sess.State().ThreadID(), "turn": map[string]any{"id": record.ProviderTurnID, "status": "completed"}}})
	(&providerRuntimeFixture{session: sess, codex: fake}).emitAndDrain(t, line)
}

func TestCreationRequestPriorityExistsBeforeImmediateThreadStartResponse(t *testing.T) {
	sess, store := causationTestSession(t, "nht_fast_start")
	requestID := requested(t, sess, store, postureFieldServiceTier, "priority")
	startCausationResponse(t, sess, sessions.ProviderRequest{ServiceTier: "priority"}, map[string]any{"serviceTier": "priority"}, func(json.RawMessage) {
		view, err := store.View(sess.HostedThreadID())
		if err != nil {
			t.Error(err)
			return
		}
		if got := view.Fields[postureFieldServiceTier].DesiredEventID; got == nil || *got != requestID {
			t.Errorf("creation request at thread/start = %v, want %d", got, requestID)
		}
	})
	f, desired, effective := fieldStrings(t, store, sess.HostedThreadID(), postureFieldServiceTier)
	if desired != "priority" || effective != "priority" || f.DesiredDisposition != metadataDispositionConfirmed || f.DesiredEventID == nil || *f.DesiredEventID != requestID {
		t.Fatalf("immediate response current = %+v desired=%q effective=%q", f, desired, effective)
	}
}

// This runs the actual asynchronous New Window spawn and runCodexHandshake
// path. The fake inspects control.sqlite at request receipt, before replying.
func TestSpawnSessionCommitsPriorityBeforeImmediateThreadStartAndSettles(t *testing.T) {
	control, executions := newTestHostedControl(t)
	fake := newFakeAppServer(t)
	fake.SetStartResult(map[string]any{"model": "gpt-test", "serviceTier": "priority", "thread": map[string]any{"id": "thread-priority", "cwd": t.TempDir()}})
	received := make(chan struct{}, 1)
	fake.SetOnStart(func(_ json.RawMessage) {
		var count int
		if err := control.CanonicalDatabase().QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE field=? AND value_json=? AND disposition=?`, postureFieldServiceTier, `"priority"`, metadataDispositionRequested).Scan(&count); err != nil || count != 1 {
			t.Errorf("Priority requested event at thread/start receipt = %d, err=%v", count, err)
		}
		received <- struct{}{}
	})
	s := &server{defaults: config.SessionDefaults{CWD: t.TempDir()}, codex: fake.daemon(), spoolDir: t.TempDir(), Registry: newSessionRegistry(), agentExecutions: executions, controlMetadata: control}
	sess, err := fixtureSpawn(s, hostedmodel.SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "codex", Mode: "start", Cwd: s.defaults.CWD, ServiceTier: "priority"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("fake never received thread/start")
	}
	if got := waitForSessionStatus(t, s, sess.ID(), "ready"); got.ThreadID != "thread-priority" {
		t.Fatalf("ready summary = %#v", got)
	}
	f, desired, effective := fieldStrings(t, control, sess.HostedThreadID(), postureFieldServiceTier)
	if desired != "priority" || effective != "priority" || f.DesiredDisposition != metadataDispositionConfirmed {
		t.Fatalf("Priority current = %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestRunCodexHandshakeAcceptsGranularApprovalPolicy(t *testing.T) {
	control, executions := newTestHostedControl(t)
	fake := newFakeAppServer(t)
	fake.SetStartResult(map[string]any{"model": "gpt-test", "approvalPolicy": map[string]any{"granular": map[string]any{"filesystem": "on-request"}}, "thread": map[string]any{"id": "thread-granular", "cwd": t.TempDir()}})
	s := &server{defaults: config.SessionDefaults{CWD: t.TempDir()}, codex: fake.daemon(), spoolDir: t.TempDir(), Registry: newSessionRegistry(), agentExecutions: executions, controlMetadata: control}
	sess, err := fixtureSpawn(s, hostedmodel.SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "codex", Mode: "start", Cwd: s.defaults.CWD}})
	if err != nil {
		t.Fatal(err)
	}
	waitForSessionStatus(t, s, sess.ID(), "ready")
	view, err := control.View(sess.HostedThreadID())
	if err != nil {
		t.Fatal(err)
	}
	if current, ok := view.Fields[postureFieldPermissionMode]; ok && current.EffectiveEventID != nil {
		t.Fatalf("granular approval fabricated permission state: %+v", current)
	}
}

func TestCodexTypedStartRejectionSettlesCreationWithoutEffectiveValue(t *testing.T) {
	control, executions := newTestHostedControl(t)
	fake := newFakeAppServer(t)
	fake.SetStartError("policy rejected start")
	s := &server{defaults: config.SessionDefaults{CWD: t.TempDir()}, codex: fake.daemon(), spoolDir: t.TempDir(), Registry: newSessionRegistry(), agentExecutions: executions, controlMetadata: control}
	sess, err := fixtureSpawn(s, hostedmodel.SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "codex", Mode: "start", Cwd: s.defaults.CWD, ServiceTier: "priority"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitForSessionStatus(t, s, sess.ID(), "exited"); got.Error == "" {
		t.Fatalf("typed rejection was not surfaced: %+v", got)
	}
	f, desired, effective := fieldStrings(t, control, sess.HostedThreadID(), postureFieldServiceTier)
	if desired != "priority" || effective != "" || f.DesiredDisposition != metadataDispositionRejected || f.EffectiveEventID != nil {
		t.Fatalf("typed rejection current = %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestCodexTransportFailureLeavesCreationPending(t *testing.T) {
	control, executions := newTestHostedControl(t)
	fake := newFakeAppServer(t)
	fake.SetDropStart(true)
	s := &server{defaults: config.SessionDefaults{CWD: t.TempDir()}, codex: fake.daemon(), spoolDir: t.TempDir(), Registry: newSessionRegistry(), agentExecutions: executions, controlMetadata: control}
	sess, err := fixtureSpawn(s, hostedmodel.SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "codex", Mode: "start", Cwd: s.defaults.CWD, ServiceTier: "priority"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitForSessionStatus(t, s, sess.ID(), "exited"); got.Error == "" {
		t.Fatalf("transport failure was not surfaced: %+v", got)
	}
	f, desired, effective := fieldStrings(t, control, sess.HostedThreadID(), postureFieldServiceTier)
	if desired != "priority" || effective != "" || f.DesiredDisposition != metadataDispositionRequested || f.EffectiveEventID != nil {
		t.Fatalf("transport failure settled creation: %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestMatchingStartResponseSettlesItsEvent(t *testing.T) {
	sess, store := causationTestSession(t, "nht_match_start")
	id := requested(t, sess, store, postureFieldModel, "gpt-5")
	startCausationResponse(t, sess, sessions.ProviderRequest{Model: "gpt-5"}, map[string]any{"model": "gpt-5"}, nil)
	f, desired, effective := fieldStrings(t, store, sess.HostedThreadID(), postureFieldModel)
	if f.DesiredEventID == nil || *f.DesiredEventID != id || f.DesiredSettlementEventID == nil || f.DesiredDisposition != metadataDispositionConfirmed || desired != "gpt-5" || effective != "gpt-5" {
		t.Fatalf("matching settlement = %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestMismatchingStartResponseRejectsRequestedAndConfirmsActual(t *testing.T) {
	sess, store := causationTestSession(t, "nht_mismatch_start")
	id := requested(t, sess, store, postureFieldServiceTier, "priority")
	startCausationResponse(t, sess, sessions.ProviderRequest{ServiceTier: "priority"}, map[string]any{"serviceTier": "default"}, nil)
	f, desired, effective := fieldStrings(t, store, sess.HostedThreadID(), postureFieldServiceTier)
	if f.DesiredEventID == nil || *f.DesiredEventID != id || f.DesiredDisposition != metadataDispositionRejected || desired != "priority" || effective != "default" || f.EffectiveDisposition != metadataDispositionConfirmed {
		t.Fatalf("mismatch current = %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestOlderStartResponseCannotSettleNewerOperatorChoice(t *testing.T) {
	sess, store := causationTestSession(t, "nht_old_response")
	old := requested(t, sess, store, postureFieldServiceTier, "priority")
	var newer int64
	startCausationResponse(t, sess, sessions.ProviderRequest{ServiceTier: "priority"}, map[string]any{"serviceTier": "priority"}, func(json.RawMessage) { newer = requested(t, sess, store, postureFieldServiceTier, "default") })
	f, desired, _ := fieldStrings(t, store, sess.HostedThreadID(), postureFieldServiceTier)
	if old == newer || f.DesiredEventID == nil || *f.DesiredEventID != newer || f.DesiredDisposition != metadataDispositionRequested || desired != "default" {
		t.Fatalf("older response settled newer request: %+v desired=%q", f, desired)
	}
}

func TestGranularApprovalPolicyLeavesPermissionUnknown(t *testing.T) {
	var response struct {
		ApprovalPolicy json.RawMessage `json:"approvalPolicy"`
	}
	if err := json.Unmarshal([]byte(`{"approvalPolicy":{"granular":{"filesystem":"on-request"}}}`), &response); err != nil {
		t.Fatal(err)
	}
	var policy string
	if err := json.Unmarshal(response.ApprovalPolicy, &policy); err == nil || policy != "" {
		t.Fatalf("granular policy parsed as lossless string %q, err=%v", policy, err)
	}
}

func TestThreadSettingsNotificationConfirmsSameValueTurnOverride(t *testing.T) {
	sess, store := causationTestSession(t, "nht_settings_match")
	fake := startCausationCodex(t, sess)
	id := requested(t, sess, store, postureFieldEffort, "high")
	sendCausationTurn(t, sess)
	emitCausation(t, sess, fake, json.RawMessage(`{"threadId":"native","threadSettings":{"model":"","effort":"high","serviceTier":"","cwd":"","approvalPolicy":"never"}}`))
	f, _, effective := fieldStrings(t, store, sess.HostedThreadID(), postureFieldEffort)
	if f.DesiredEventID == nil || *f.DesiredEventID != id || f.DesiredDisposition != metadataDispositionConfirmed || effective != "high" {
		t.Fatalf("settings confirmation = %+v effective=%q", f, effective)
	}
}

func TestThreadSettingsMismatchRejectsSentOverrideAndRecordsActual(t *testing.T) {
	sess, store := causationTestSession(t, "nht_settings_mismatch")
	fake := startCausationCodex(t, sess)
	requested(t, sess, store, postureFieldServiceTier, "priority")
	sendCausationTurn(t, sess)
	emitCausation(t, sess, fake, json.RawMessage(`{"threadId":"native","threadSettings":{"model":"","effort":"","serviceTier":"default","cwd":"","approvalPolicy":{"granular":{}}}}`))
	f, desired, effective := fieldStrings(t, store, sess.HostedThreadID(), postureFieldServiceTier)
	if f.DesiredDisposition != metadataDispositionRejected || desired != "priority" || effective != "default" {
		t.Fatalf("settings mismatch = %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestRapidOperatorChangesOlderSettingsNotificationCannotSettleNewerChoice(t *testing.T) {
	sess, store := causationTestSession(t, "nht_settings_old")
	fake := startCausationCodex(t, sess)
	requested(t, sess, store, postureFieldModel, "old")
	sendCausationTurn(t, sess)
	newer := requested(t, sess, store, postureFieldModel, "new")
	emitCausation(t, sess, fake, json.RawMessage(`{"threadId":"native","threadSettings":{"model":"old","effort":"","serviceTier":"","cwd":""}}`))
	f, desired, _ := fieldStrings(t, store, sess.HostedThreadID(), postureFieldModel)
	if f.DesiredEventID == nil || *f.DesiredEventID != newer || f.DesiredDisposition != metadataDispositionRequested || desired != "new" {
		t.Fatalf("old settings settled newer choice: %+v", f)
	}
}

func TestDuplicateSettingsNotificationAddsNoEventsOrRevisionAndOtherThreadDoesNothing(t *testing.T) {
	sess, store := causationTestSession(t, "nht_settings_duplicate")
	fake := startCausationCodex(t, sess)
	requested(t, sess, store, postureFieldModel, "gpt-5")
	sendCausationTurn(t, sess)
	payload := json.RawMessage(`{"threadId":"native","threadSettings":{"model":"gpt-5","effort":"","serviceTier":"","cwd":""}}`)
	emitCausation(t, sess, fake, payload)
	var events int
	_ = store.CanonicalDatabase().QueryRow(`SELECT count(*) FROM thread_metadata_events`).Scan(&events)
	revision := func() int64 { v, _ := store.View(sess.HostedThreadID()); return v.Revision }()
	emitCausation(t, sess, fake, payload)
	fake.EmitNotification(map[string]any{"method": "thread/settings/updated", "params": map[string]any{"threadId": "other", "threadSettings": map[string]any{"model": "other"}}})
	emitCausation(t, sess, fake, payload)
	var after int
	_ = store.CanonicalDatabase().QueryRow(`SELECT count(*) FROM thread_metadata_events`).Scan(&after)
	v, _ := store.View(sess.HostedThreadID())
	if after != events || v.Revision != revision {
		t.Fatalf("duplicate/other notification mutated events %d→%d revision %d→%d", events, after, revision, v.Revision)
	}
}

func TestClaudeReplayCannotSettleNewDesiredButFreshInitSettlesOwnRequest(t *testing.T) {
	sess, store := causationTestSessionForProvider(t, "nht_claude_causation", "claude")
	old := requested(t, sess, store, postureFieldModel, "old")
	newer := requested(t, sess, store, postureFieldModel, "new")
	line := []byte(`{"type":"system","subtype":"init","session_id":"claude-native","model":"old","permissionMode":""}`)
	replayCausationClaude(t, sess, line)
	f, desired, _ := fieldStrings(t, store, sess.HostedThreadID(), postureFieldModel)
	if old == newer || f.DesiredEventID == nil || *f.DesiredEventID != newer || f.DesiredDisposition != metadataDispositionRequested || desired != "new" {
		t.Fatalf("replay settled newer desired: %+v", f)
	}
	sess.Lifecycle().Close()
	sess = newCausationClaudeAttachment(t, store, sess.HostedThreadID())
	fresh := startCausationClaude(t, sess, "new")
	fresh.emitAndDrain(t, []byte(`{"type":"system","subtype":"init","session_id":"claude-native","model":"new","permissionMode":""}`))
	f, _, effective := fieldStrings(t, store, sess.HostedThreadID(), postureFieldModel)
	if f.DesiredDisposition != metadataDispositionConfirmed || effective != "new" {
		t.Fatalf("fresh init did not settle its request: %+v effective=%q", f, effective)
	}
}

func TestCaptureInitReplayAndFreshLaunchUseImmutableSnapshots(t *testing.T) {
	sess, store := causationTestSessionForProvider(t, "nht_claude_init_surface", "claude")
	requested(t, sess, store, postureFieldModel, "old")
	newID := requested(t, sess, store, postureFieldModel, "new")
	line := []byte(`{"type":"system","subtype":"init","session_id":"claude-native","model":"old","permissionMode":""}`)
	replayCausationClaude(t, sess, line)
	f, desired, _ := fieldStrings(t, store, sess.HostedThreadID(), postureFieldModel)
	if f.DesiredEventID == nil || *f.DesiredEventID != newID || f.DesiredDisposition != metadataDispositionRequested || desired != "new" {
		t.Fatalf("replayed init settled newer desired: %+v desired=%q", f, desired)
	}
	var replayed int
	if err := store.CanonicalDatabase().QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id=? AND evidence_freshness=?`, sess.HostedThreadID(), metadataFreshnessReplayed).Scan(&replayed); err != nil || replayed == 0 {
		t.Fatalf("captureInit did not store replayed evidence: count=%d err=%v", replayed, err)
	}
	sess.Lifecycle().Close()
	sess = newCausationClaudeAttachment(t, store, sess.HostedThreadID())
	// Capture the old request at launch, then create the new decision before its response.
	requested(t, sess, store, postureFieldModel, "old")
	fresh := startCausationClaude(t, sess, "old")
	newID = requested(t, sess, store, postureFieldModel, "new")
	fresh.emitAndDrain(t, line)
	f, desired, effective := fieldStrings(t, store, sess.HostedThreadID(), postureFieldModel)
	if f.DesiredEventID == nil || *f.DesiredEventID != newID || f.DesiredDisposition != metadataDispositionRequested || desired != "new" || effective != "old" {
		t.Fatalf("fresh child borrowed newer desired causation: %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestClaudeReplayLiveBoundaryKeepsCanonicalMetadataReadable(t *testing.T) {
	sess, store := causationTestSessionForProvider(t, "nht_claude_replay_live_boundary", "claude")
	line := []byte(`{"type":"system","subtype":"init","session_id":"claude-native","model":"claude-fable-5","permissionMode":"bypassPermissions"}`)
	f := replayCausationClaude(t, sess, line)
	f.emitAndDrain(t, line)
	if summary := sess.View().Summary(); summary.MetadataUnavailable || summary.Metadata == nil {
		t.Fatalf("healthy canonical state hidden after replay/live boundary: %+v", summary)
	}
	var count int
	if err := store.CanonicalDatabase().QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id=? AND field=? AND source=?`, sess.HostedThreadID(), postureFieldModel, postureSourceProvider).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("model evidence events = %d, want replayed and live occurrences", count)
	}
	view, err := store.View(sess.HostedThreadID())
	if err != nil {
		t.Fatal(err)
	}
	if current := view.Fields[postureFieldModel]; current.DisplayFreshness != metadataFreshnessLive {
		t.Fatalf("display freshness = %q, want live; current=%+v", current.DisplayFreshness, current)
	}
}

func newCausationClaudeAttachment(t *testing.T, store *hostedstore.MetadataStore, hosted string) *sessions.Session {
	t.Helper()
	generation, err := store.BeginTransportBinding(hosted)
	if err != nil {
		t.Fatal(err)
	}
	sess := newSession(runtimeFixtureConfig(t, sessions.Config{ID: "causation-clone", Provider: "claude", HostedThreadID: hosted, Metadata: store, Projection: newLiveProjection("claude", hosted)}))
	if err := sess.InitializeBinding(generation); err != nil {
		t.Fatal(err)
	}
	sess.ReserveNativeID("claude-native")
	t.Cleanup(sess.Lifecycle().Close)
	return sess
}

func startCausationClaude(t *testing.T, sess *sessions.Session, model string) *providerRuntimeFixture {
	t.Helper()
	connected := make(chan *claudeshim.Conn, 1)
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		start, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(start, map[string]any{"shimSessionId": "shim-1"})
		connected <- c
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			c.Reply(call, map[string]any{})
		}
	})
	if err := sess.Runtime().BeginProviderSpawn(sessions.ProviderRequest{Provider: "claude", Model: model, ClaudeShimWS: shim.WSURL(), ClaudeBin: "/fake/claude", Cwd: t.TempDir()}, nil, func() {}); err != nil {
		t.Fatal(err)
	}
	f := &providerRuntimeFixture{session: sess, claude: <-connected}
	waitForClaudeStatus(t, sess, "ready")
	return f
}

func replayCausationClaude(t *testing.T, sess *sessions.Session, line []byte) *providerRuntimeFixture {
	t.Helper()
	sess.ReserveNativeID("claude-native")
	root := t.TempDir()
	writeClaudeTestSpool(t, root, sess.HostedThreadID())
	connected := make(chan *claudeshim.Conn, 1)
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		attach, ok := c.Next()
		if !ok {
			return
		}
		// The init arrives after Nano's disk scan, so the attach must replay it.
		writeClaudeTestSpool(t, root, sess.HostedThreadID(), claudeshim.Frame{Seq: 1, Dir: "recv", Line: string(line)})
		c.Reply(attach, map[string]any{"shimSessionId": "shim-1", "nativeSessionId": "claude-native", "state": "running", "turnActive": false, "lastSeq": 1})
		c.Line(1, "recv", string(line))
		c.Notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 1})
		connected <- c
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			c.Reply(call, map[string]any{})
		}
	})
	if err := sess.Runtime().BeginProviderSpawn(sessions.ProviderRequest{Provider: "claude", Mode: "resume", ThreadID: sess.State().ThreadID(), ClaudeShimWS: shim.WSURL(), ClaudeShimSpools: root, ClaudeBin: "/fake/claude", ClaudeAnchor: hostedstore.Attachment{Provider: "claude", NativeThreadID: sess.State().ThreadID(), ShimSessionID: "shim-1"}}, nil, func() {}); err != nil {
		t.Fatal(err)
	}
	f := &providerRuntimeFixture{session: sess, claude: <-connected, seq: 1}
	waitForClaudeStatus(t, sess, "ready")
	return f
}

func TestCoreSubmitFailedRPCRemovesCausationBeforeLaterSettings(t *testing.T) {
	sess, store := causationTestSession(t, "nht_turn_failed")
	fake := startCausationCodex(t, sess)
	requested(t, sess, store, postureFieldServiceTier, "priority")
	fake.SetTurnStartError("rejected by provider")
	if response := postTurn(t, sess.Delivery(), context.Background(), "failed", "hello"); response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"status":"parked"`) {
		t.Fatalf("failed turn/start status = %d, body = %s", response.Code, response.Body.String())
	}
	requested(t, sess, store, postureFieldServiceTier, "default")
	fake.SetTurnStartError("")
	fake.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "turn-success"}})
	if response := postTurn(t, sess.Delivery(), context.Background(), "succeeds", "hello again"); response.Code != http.StatusOK {
		t.Fatalf("successful turn/start status = %d, body = %s", response.Code, response.Body.String())
	}
	emitCausation(t, sess, fake, json.RawMessage(`{"threadId":"native","threadSettings":{"model":"","effort":"","serviceTier":"default","cwd":""}}`))
	f, desired, effective := fieldStrings(t, store, sess.HostedThreadID(), postureFieldServiceTier)
	if desired != "default" || effective != "default" || f.DesiredDisposition != metadataDispositionConfirmed {
		t.Fatalf("later settings used failed request causation: %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestSettingsDedupeDistinguishesCausalRequestsAndCanonicalizesJSON(t *testing.T) {
	sess, store := causationTestSession(t, "nht_turn_dedupe")
	fake := startCausationCodex(t, sess)
	fake.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "turn-first"}})
	// Two independent operator requests with identical values must each be
	// eligible for causation; the second is not hidden by the first's retry.
	first := requested(t, sess, store, postureFieldEffort, "high")
	if response := postTurn(t, sess.Delivery(), context.Background(), "first", "first prompt"); response.Code != http.StatusOK {
		t.Fatalf("first turn/start status = %d, body = %s", response.Code, response.Body.String())
	}
	payloadA := json.RawMessage(`{"threadId":"native","threadSettings":{"model":"","effort":"high","serviceTier":"","cwd":""}}`)
	emitCausation(t, sess, fake, payloadA)
	var firstCount int
	_ = store.CanonicalDatabase().QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id=? AND field=?`, sess.HostedThreadID(), postureFieldEffort).Scan(&firstCount)
	// The provider can retry before a second turn is submitted. Different JSON
	// formatting must reuse the first causal identity rather than add evidence.
	payloadB := json.RawMessage(` { "threadSettings" : { "cwd":"", "serviceTier":"", "effort":"high", "model":"" }, "threadId":"native" } `)
	emitCausation(t, sess, fake, payloadB)
	var retryCount int
	_ = store.CanonicalDatabase().QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id=? AND field=?`, sess.HostedThreadID(), postureFieldEffort).Scan(&retryCount)
	if retryCount != firstCount {
		t.Fatalf("pre-second duplicate added evidence %d→%d", firstCount, retryCount)
	}
	finishCausationTurn(t, sess, fake, "first")
	second := requested(t, sess, store, postureFieldEffort, "high")
	fake.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "turn-second"}})
	if response := postTurn(t, sess.Delivery(), context.Background(), "second", "second prompt"); response.Code != http.StatusOK {
		t.Fatalf("second turn/start status = %d, body = %s", response.Code, response.Body.String())
	}
	emitCausation(t, sess, fake, payloadB)
	f, _, _ := fieldStrings(t, store, sess.HostedThreadID(), postureFieldEffort)
	if first == second || f.DesiredEventID == nil || *f.DesiredEventID != second || f.DesiredDisposition != metadataDispositionConfirmed {
		t.Fatalf("second identical causal request was suppressed: %+v", f)
	}
	var count int
	if err := store.CanonicalDatabase().QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id=? AND field=?`, sess.HostedThreadID(), postureFieldEffort).Scan(&count); err != nil {
		t.Fatal(err)
	}
	// A reordered encoding with no newly queued request is a retry of the
	// second causal entry. It must not add an unlinked observation.
	emitCausation(t, sess, fake, payloadA)
	var after int
	_ = store.CanonicalDatabase().QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id=? AND field=?`, sess.HostedThreadID(), postureFieldEffort).Scan(&after)
	if after != count {
		t.Fatalf("reordered duplicate added evidence %d→%d", count, after)
	}
}

// Live captures on both eligible models emitted this exact settings shape.
// Boundary: canonical metadata receives the provider's Ultrafast setting.
func TestUltrafastSettingEvidenceSettlesOnlyTheRequestedTier(t *testing.T) {
	sess, store := causationTestSession(t, "nht_speed_evidence")
	fake := startCausationCodex(t, sess)
	fake.Mu.Lock()
	fake.ModelList = map[string]any{"data": []any{map[string]any{"model": "gpt-6.1-sol", "serviceTiers": []any{map[string]any{"id": "ultrafast"}}}}}
	fake.Mu.Unlock()
	response := httptest.NewRecorder()
	sess.State().HandleTurnOptionsUpdate(response, httptest.NewRequest(http.MethodPatch, "/turn-options", strings.NewReader(`{"model":"gpt-6.1-sol","serviceTier":"ultrafast"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("provider-advertised Ultrafast must be valid canonical intent: %d %s", response.Code, response.Body.String())
	}
	sendCausationTurn(t, sess)
	payload, _ := json.Marshal(map[string]any{"threadId": sess.State().ThreadID(), "threadSettings": map[string]any{"serviceTier": "ultrafast"}})
	emitCausation(t, sess, fake, payload)
	f, desired, effective := fieldStrings(t, store, sess.HostedThreadID(), postureFieldServiceTier)
	if desired != "ultrafast" || effective != "ultrafast" || f.DesiredDisposition != metadataDispositionConfirmed {
		t.Fatalf("Ultrafast confirmation = %+v desired=%s effective=%s", f, desired, effective)
	}
}

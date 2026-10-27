package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coresession "github.com/dkroshow/nano/internal/core/session"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// replayingClaudeTransport delivers lines as a shim replay would.
type replayingClaudeTransport struct{ stubClaudeTransport }

func (replayingClaudeTransport) isReplaying() bool { return true }

func causationTestSession(t *testing.T, hosted string) (*session, *controlMetadataStore) {
	return causationTestSessionForProvider(t, hosted, "codex")
}

func causationTestSessionForProvider(t *testing.T, hosted, provider string) (*session, *controlMetadataStore) {
	t.Helper()
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread(hosted, provider, presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	generation, err := store.beginTransportBinding(hosted)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "causation-test")
	if err != nil {
		t.Fatal(err)
	}
	sess := readySendTestSession(t, attempts, nil)
	sess.id = "sess_" + hosted
	sess.hostedThreadID = hosted
	sess.provider = provider
	sess.projection = newLiveProjection(provider, hosted)
	sess.metadata = store
	sess.attachmentGeneration = generation
	sess.state = map[string]json.RawMessage{}
	t.Cleanup(func() {
		if sess.core != nil {
			sess.core.Close()
		}
	})
	return sess, store
}

func requested(t *testing.T, sess *session, field, value string) int64 {
	t.Helper()
	sess.recordPosture([]postureEntry{{Field: field, Value: value, Source: postureSourceOperator, Evidence: "test"}}, metadataFreshnessLive)
	view, err := sess.metadata.view(sess.hostedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	return *view.Fields[field].DesiredEventID
}

func fieldStrings(t *testing.T, store *controlMetadataStore, hosted, field string) (metadataCurrentView, string, string) {
	t.Helper()
	v, err := store.view(hosted)
	if err != nil {
		t.Fatal(err)
	}
	f := v.Fields[field]
	var desired, effective string
	_ = json.Unmarshal(f.DesiredValue, &desired)
	_ = json.Unmarshal(f.EffectiveValue, &effective)
	return f, desired, effective
}

func finishCausationTurn(t *testing.T, sess *session, actionID string) {
	t.Helper()
	accepted, ok := sess.queuedTurns.get(actionID)
	if !ok || accepted.ProviderTurnID == "" {
		t.Fatalf("accepted turn %q has no provider identity: %#v", actionID, accepted)
	}
	line, err := json.Marshal(map[string]any{
		"method": "turn/completed",
		"params": map[string]any{
			"threadId": sess.currentThreadID(),
			"turn":     map[string]any{"id": accepted.ProviderTurnID, "status": "completed"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sess.recordWire(protocol.Recv, line)
	if err := sess.core.Observe(context.Background(), coresession.AdapterEvent{}); err != nil {
		t.Fatal(err)
	}
}

func TestCreationRequestPriorityExistsBeforeImmediateThreadStartResponse(t *testing.T) {
	sess, store := causationTestSession(t, "nht_fast_start")
	requestID := requested(t, sess, postureFieldServiceTier, "priority")
	snapshot := sess.snapshotProviderCausation(map[string]string{postureFieldServiceTier: "priority"})
	if snapshot[postureFieldServiceTier].EventID != requestID {
		t.Fatalf("snapshot = %+v, want creation event %d before response", snapshot, requestID)
	}
	sess.recordProviderEvidence("thread_start_response", map[string]string{postureFieldServiceTier: "priority"}, metadataFreshnessLive, snapshot)
	f, desired, effective := fieldStrings(t, store, sess.hostedThreadID, postureFieldServiceTier)
	if desired != "priority" || effective != "priority" || f.DesiredDisposition != metadataDispositionConfirmed || f.DesiredEventID == nil || *f.DesiredEventID != requestID {
		t.Fatalf("immediate response current = %+v desired=%q effective=%q", f, desired, effective)
	}
}

// This runs the actual asynchronous New Window spawn and runCodexHandshake
// path. The fake inspects control.sqlite at request receipt, before replying.
func TestSpawnSessionCommitsPriorityBeforeImmediateThreadStartAndSettles(t *testing.T) {
	control, executions := newTestHostedControl(t)
	fake := newFakeAppServer(t)
	fake.setStartResult(map[string]any{"model": "gpt-test", "serviceTier": "priority", "thread": map[string]any{"id": "thread-priority", "cwd": t.TempDir()}})
	received := make(chan struct{}, 1)
	fake.setOnStart(func(_ json.RawMessage) {
		var count int
		if err := control.db.QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE field=? AND value_json=? AND disposition=?`, postureFieldServiceTier, `"priority"`, metadataDispositionRequested).Scan(&count); err != nil || count != 1 {
			t.Errorf("Priority requested event at thread/start receipt = %d, err=%v", count, err)
		}
		received <- struct{}{}
	})
	s := &server{defaults: sessionDefaults{cwd: t.TempDir()}, codex: fake.daemon(), spoolDir: t.TempDir(), sessions: map[string]*session{}, agentExecutions: executions, controlMetadata: control}
	sess, err := s.spawnSession(spawnRequest{Provider: "codex", Mode: "start", Cwd: s.defaults.cwd, ServiceTier: "priority"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("fake never received thread/start")
	}
	if got := waitForSessionStatus(t, s, sess.id, "ready"); got.ThreadID != "thread-priority" {
		t.Fatalf("ready summary = %#v", got)
	}
	f, desired, effective := fieldStrings(t, control, sess.hostedThreadID, postureFieldServiceTier)
	if desired != "priority" || effective != "priority" || f.DesiredDisposition != metadataDispositionConfirmed {
		t.Fatalf("Priority current = %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestRunCodexHandshakeAcceptsGranularApprovalPolicy(t *testing.T) {
	control, executions := newTestHostedControl(t)
	fake := newFakeAppServer(t)
	fake.setStartResult(map[string]any{"model": "gpt-test", "approvalPolicy": map[string]any{"granular": map[string]any{"filesystem": "on-request"}}, "thread": map[string]any{"id": "thread-granular", "cwd": t.TempDir()}})
	s := &server{defaults: sessionDefaults{cwd: t.TempDir()}, codex: fake.daemon(), spoolDir: t.TempDir(), sessions: map[string]*session{}, agentExecutions: executions, controlMetadata: control}
	sess, err := s.spawnSession(spawnRequest{Provider: "codex", Mode: "start", Cwd: s.defaults.cwd})
	if err != nil {
		t.Fatal(err)
	}
	waitForSessionStatus(t, s, sess.id, "ready")
	view, err := control.view(sess.hostedThreadID)
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
	fake.setStartError("policy rejected start")
	s := &server{defaults: sessionDefaults{cwd: t.TempDir()}, codex: fake.daemon(), spoolDir: t.TempDir(), sessions: map[string]*session{}, agentExecutions: executions, controlMetadata: control}
	sess, err := s.spawnSession(spawnRequest{Provider: "codex", Mode: "start", Cwd: s.defaults.cwd, ServiceTier: "priority"})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitForSessionStatus(t, s, sess.id, "exited"); got.Error == "" {
		t.Fatalf("typed rejection was not surfaced: %+v", got)
	}
	f, desired, effective := fieldStrings(t, control, sess.hostedThreadID, postureFieldServiceTier)
	if desired != "priority" || effective != "" || f.DesiredDisposition != metadataDispositionRejected || f.EffectiveEventID != nil {
		t.Fatalf("typed rejection current = %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestCodexTransportFailureLeavesCreationPending(t *testing.T) {
	control, executions := newTestHostedControl(t)
	fake := newFakeAppServer(t)
	fake.setDropStart(true)
	s := &server{defaults: sessionDefaults{cwd: t.TempDir()}, codex: fake.daemon(), spoolDir: t.TempDir(), sessions: map[string]*session{}, agentExecutions: executions, controlMetadata: control}
	sess, err := s.spawnSession(spawnRequest{Provider: "codex", Mode: "start", Cwd: s.defaults.cwd, ServiceTier: "priority"})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitForSessionStatus(t, s, sess.id, "exited"); got.Error == "" {
		t.Fatalf("transport failure was not surfaced: %+v", got)
	}
	f, desired, effective := fieldStrings(t, control, sess.hostedThreadID, postureFieldServiceTier)
	if desired != "priority" || effective != "" || f.DesiredDisposition != metadataDispositionRequested || f.EffectiveEventID != nil {
		t.Fatalf("transport failure settled creation: %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestMatchingStartResponseSettlesItsEvent(t *testing.T) {
	sess, store := causationTestSession(t, "nht_match_start")
	id := requested(t, sess, postureFieldModel, "gpt-5")
	sess.recordProviderEvidence("thread_start_response", map[string]string{postureFieldModel: "gpt-5"}, metadataFreshnessLive, sess.snapshotProviderCausation(map[string]string{postureFieldModel: "gpt-5"}))
	f, desired, effective := fieldStrings(t, store, sess.hostedThreadID, postureFieldModel)
	if f.DesiredEventID == nil || *f.DesiredEventID != id || f.DesiredSettlementEventID == nil || f.DesiredDisposition != metadataDispositionConfirmed || desired != "gpt-5" || effective != "gpt-5" {
		t.Fatalf("matching settlement = %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestMismatchingStartResponseRejectsRequestedAndConfirmsActual(t *testing.T) {
	sess, store := causationTestSession(t, "nht_mismatch_start")
	id := requested(t, sess, postureFieldServiceTier, "priority")
	sess.recordProviderEvidence("thread_start_response", map[string]string{postureFieldServiceTier: "default"}, metadataFreshnessLive, sess.snapshotProviderCausation(map[string]string{postureFieldServiceTier: "priority"}))
	f, desired, effective := fieldStrings(t, store, sess.hostedThreadID, postureFieldServiceTier)
	if f.DesiredEventID == nil || *f.DesiredEventID != id || f.DesiredDisposition != metadataDispositionRejected || desired != "priority" || effective != "default" || f.EffectiveDisposition != metadataDispositionConfirmed {
		t.Fatalf("mismatch current = %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestOlderStartResponseCannotSettleNewerOperatorChoice(t *testing.T) {
	sess, store := causationTestSession(t, "nht_old_response")
	old := requested(t, sess, postureFieldServiceTier, "priority")
	snapshot := sess.snapshotProviderCausation(map[string]string{postureFieldServiceTier: "priority"})
	newer := requested(t, sess, postureFieldServiceTier, "default")
	sess.recordProviderEvidence("thread_start_response", map[string]string{postureFieldServiceTier: "priority"}, metadataFreshnessLive, snapshot)
	f, desired, _ := fieldStrings(t, store, sess.hostedThreadID, postureFieldServiceTier)
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
	id := requested(t, sess, postureFieldEffort, "high")
	sess.beginTurnCausation(map[string]string{postureFieldEffort: "high"})
	d := &codexDaemon{sessions: map[string]*session{"native": sess}}
	d.handleNotification("thread/settings/updated", json.RawMessage(`{"threadId":"native","threadSettings":{"model":"","effort":"high","serviceTier":"","cwd":"","approvalPolicy":"never"}}`))
	f, _, effective := fieldStrings(t, store, sess.hostedThreadID, postureFieldEffort)
	if f.DesiredEventID == nil || *f.DesiredEventID != id || f.DesiredDisposition != metadataDispositionConfirmed || effective != "high" {
		t.Fatalf("settings confirmation = %+v effective=%q", f, effective)
	}
}

func TestThreadSettingsMismatchRejectsSentOverrideAndRecordsActual(t *testing.T) {
	sess, store := causationTestSession(t, "nht_settings_mismatch")
	requested(t, sess, postureFieldServiceTier, "priority")
	sess.beginTurnCausation(map[string]string{postureFieldServiceTier: "priority"})
	d := &codexDaemon{sessions: map[string]*session{"native": sess}}
	d.handleNotification("thread/settings/updated", json.RawMessage(`{"threadId":"native","threadSettings":{"model":"","effort":"","serviceTier":"default","cwd":"","approvalPolicy":{"granular":{}}}}`))
	f, desired, effective := fieldStrings(t, store, sess.hostedThreadID, postureFieldServiceTier)
	if f.DesiredDisposition != metadataDispositionRejected || desired != "priority" || effective != "default" {
		t.Fatalf("settings mismatch = %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestRapidOperatorChangesOlderSettingsNotificationCannotSettleNewerChoice(t *testing.T) {
	sess, store := causationTestSession(t, "nht_settings_old")
	requested(t, sess, postureFieldModel, "old")
	sess.beginTurnCausation(map[string]string{postureFieldModel: "old"})
	newer := requested(t, sess, postureFieldModel, "new")
	d := &codexDaemon{sessions: map[string]*session{"native": sess}}
	d.handleNotification("thread/settings/updated", json.RawMessage(`{"threadId":"native","threadSettings":{"model":"old","effort":"","serviceTier":"","cwd":""}}`))
	f, desired, _ := fieldStrings(t, store, sess.hostedThreadID, postureFieldModel)
	if f.DesiredEventID == nil || *f.DesiredEventID != newer || f.DesiredDisposition != metadataDispositionRequested || desired != "new" {
		t.Fatalf("old settings settled newer choice: %+v", f)
	}
}

func TestDuplicateSettingsNotificationAddsNoEventsOrRevisionAndOtherThreadDoesNothing(t *testing.T) {
	sess, store := causationTestSession(t, "nht_settings_duplicate")
	requested(t, sess, postureFieldModel, "gpt-5")
	sess.beginTurnCausation(map[string]string{postureFieldModel: "gpt-5"})
	d := &codexDaemon{sessions: map[string]*session{"native": sess}}
	payload := json.RawMessage(`{"threadId":"native","threadSettings":{"model":"gpt-5","effort":"","serviceTier":"","cwd":""}}`)
	d.handleNotification("thread/settings/updated", payload)
	var events int
	_ = store.db.QueryRow(`SELECT count(*) FROM thread_metadata_events`).Scan(&events)
	revision := func() int64 { v, _ := store.view(sess.hostedThreadID); return v.Revision }()
	d.handleNotification("thread/settings/updated", payload)
	d.handleNotification("thread/settings/updated", json.RawMessage(`{"threadId":"other","threadSettings":{"model":"other"}}`))
	var after int
	_ = store.db.QueryRow(`SELECT count(*) FROM thread_metadata_events`).Scan(&after)
	v, _ := store.view(sess.hostedThreadID)
	if after != events || v.Revision != revision {
		t.Fatalf("duplicate/other notification mutated events %d→%d revision %d→%d", events, after, revision, v.Revision)
	}
}

func TestClaudeReplayCannotSettleNewDesiredButFreshInitSettlesOwnRequest(t *testing.T) {
	sess, store := causationTestSession(t, "nht_claude_causation")
	old := requested(t, sess, postureFieldModel, "old")
	snapshot := sess.snapshotProviderCausation(map[string]string{postureFieldModel: "old"})
	newer := requested(t, sess, postureFieldModel, "new")
	sess.recordProviderEvidence("claude_system_init", map[string]string{postureFieldModel: "old"}, metadataFreshnessReplayed, snapshot)
	f, desired, _ := fieldStrings(t, store, sess.hostedThreadID, postureFieldModel)
	if old == newer || f.DesiredEventID == nil || *f.DesiredEventID != newer || f.DesiredDisposition != metadataDispositionRequested || desired != "new" {
		t.Fatalf("replay settled newer desired: %+v", f)
	}
	fresh := sess.snapshotProviderCausation(map[string]string{postureFieldModel: "new"})
	sess.recordProviderEvidence("claude_system_init", map[string]string{postureFieldModel: "new"}, metadataFreshnessLive, fresh)
	f, _, effective := fieldStrings(t, store, sess.hostedThreadID, postureFieldModel)
	if f.DesiredDisposition != metadataDispositionConfirmed || effective != "new" {
		t.Fatalf("fresh init did not settle its request: %+v effective=%q", f, effective)
	}
}

func TestCaptureInitReplayAndFreshLaunchUseImmutableSnapshots(t *testing.T) {
	sess, store := causationTestSessionForProvider(t, "nht_claude_init_surface", "claude")
	// The test uses captureState, which parses the actual system.init wire line
	// and dispatches captureInit rather than calling the evidence helper.
	requested(t, sess, postureFieldModel, "old")
	oldSnapshot := sess.snapshotProviderCausation(map[string]string{postureFieldModel: "old"})
	newID := requested(t, sess, postureFieldModel, "new")
	line := []byte(`{"type":"system","subtype":"init","session_id":"claude-native","model":"old","permissionMode":""}`)
	newClaudeSession(sess, "", oldSnapshot).captureState(replayingClaudeTransport{}, line)
	f, desired, _ := fieldStrings(t, store, sess.hostedThreadID, postureFieldModel)
	if f.DesiredEventID == nil || *f.DesiredEventID != newID || f.DesiredDisposition != metadataDispositionRequested || desired != "new" {
		t.Fatalf("replayed init settled newer desired: %+v desired=%q", f, desired)
	}
	var replayed int
	if err := store.db.QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id=? AND evidence_freshness=?`, sess.hostedThreadID, metadataFreshnessReplayed).Scan(&replayed); err != nil || replayed == 0 {
		t.Fatalf("captureInit did not store replayed evidence: count=%d err=%v", replayed, err)
	}
	// A newly opened child still carries oldSnapshot, not the current desired
	// row discovered at response time. Its live init cannot settle the newer
	// request.
	newClaudeSession(sess, "", oldSnapshot).captureState(stubClaudeTransport{}, line)
	f, desired, effective := fieldStrings(t, store, sess.hostedThreadID, postureFieldModel)
	if f.DesiredEventID == nil || *f.DesiredEventID != newID || f.DesiredDisposition != metadataDispositionRequested || desired != "new" || effective != "old" {
		t.Fatalf("fresh child borrowed newer desired causation: %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestClaudeReplayLiveBoundaryKeepsCanonicalMetadataReadable(t *testing.T) {
	sess, store := causationTestSessionForProvider(t, "nht_claude_replay_live_boundary", "claude")
	line := []byte(`{"type":"system","subtype":"init","session_id":"claude-native","model":"claude-fable-5","permissionMode":"bypassPermissions"}`)
	claude := newClaudeSession(sess, "", nil)

	// The shim can end replay and then deliver the same semantic init on its
	// live side. Freshness is part of the evidence occurrence: this is neither
	// a corrupt source-key retry nor a reason to hide the last committed state.
	claude.captureState(replayingClaudeTransport{}, line)
	claude.captureState(stubClaudeTransport{}, line)

	if summary := sess.summary(); summary.MetadataUnavailable || summary.Metadata == nil {
		t.Fatalf("healthy canonical state hidden after replay/live boundary: %+v", summary)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id=? AND field=? AND source=?`, sess.hostedThreadID, postureFieldModel, postureSourceProvider).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("model evidence events = %d, want replayed and live occurrences", count)
	}
	view, err := store.view(sess.hostedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	current := view.Fields[postureFieldModel]
	if current.DisplayFreshness != metadataFreshnessLive {
		t.Fatalf("display freshness = %q, want live; current=%+v", current.DisplayFreshness, current)
	}
}

func TestCoreSubmitFailedRPCRemovesCausationBeforeLaterSettings(t *testing.T) {
	sess, store := causationTestSession(t, "nht_turn_failed")
	sess.threadID = "native"
	sess.turnServiceTier = "priority"
	requested(t, sess, postureFieldServiceTier, "priority")
	fake := newFakeAppServer(t)
	fake.setTurnStartError("rejected by provider")
	sess.codex = fake.daemon()
	if response := postTurn(t, sess, context.Background(), "failed", "hello"); response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"status":"parked"`) {
		t.Fatalf("failed turn/start status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(sess.pendingTurnCausation) != 0 {
		t.Fatalf("failed turn left causation queue %#v", sess.pendingTurnCausation)
	}
	requested(t, sess, postureFieldServiceTier, "default")
	sess.turnServiceTier = "default"
	fake.setTurnStartError("")
	fake.setTurnStartResult(map[string]any{"turn": map[string]any{"id": "turn-success"}})
	if response := postTurn(t, sess, context.Background(), "succeeds", "hello again"); response.Code != http.StatusOK {
		t.Fatalf("successful turn/start status = %d, body = %s", response.Code, response.Body.String())
	}
	d := &codexDaemon{sessions: map[string]*session{"native": sess}}
	d.handleNotification("thread/settings/updated", json.RawMessage(`{"threadId":"native","threadSettings":{"model":"","effort":"","serviceTier":"default","cwd":""}}`))
	f, desired, effective := fieldStrings(t, store, sess.hostedThreadID, postureFieldServiceTier)
	if desired != "default" || effective != "default" || f.DesiredDisposition != metadataDispositionConfirmed {
		t.Fatalf("later settings used failed request causation: %+v desired=%q effective=%q", f, desired, effective)
	}
}

func TestSettingsDedupeDistinguishesCausalRequestsAndCanonicalizesJSON(t *testing.T) {
	sess, store := causationTestSession(t, "nht_turn_dedupe")
	sess.threadID = "native"
	fake := newFakeAppServer(t)
	fake.setTurnStartResult(map[string]any{"turn": map[string]any{"id": "turn-first"}})
	sess.codex = fake.daemon()
	d := &codexDaemon{sessions: map[string]*session{"native": sess}}
	// Two independent operator requests with identical values must each be
	// eligible for causation; the second is not hidden by the first's retry.
	first := requested(t, sess, postureFieldEffort, "high")
	sess.turnEffort = "high"
	if response := postTurn(t, sess, context.Background(), "first", "first prompt"); response.Code != http.StatusOK {
		t.Fatalf("first turn/start status = %d, body = %s", response.Code, response.Body.String())
	}
	payloadA := json.RawMessage(`{"threadId":"native","threadSettings":{"model":"","effort":"high","serviceTier":"","cwd":""}}`)
	d.handleNotification("thread/settings/updated", payloadA)
	var firstCount int
	_ = store.db.QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id=? AND field=?`, sess.hostedThreadID, postureFieldEffort).Scan(&firstCount)
	// The provider can retry before a second turn is submitted. Different JSON
	// formatting must reuse the first causal identity rather than add evidence.
	payloadB := json.RawMessage(` { "threadSettings" : { "cwd":"", "serviceTier":"", "effort":"high", "model":"" }, "threadId":"native" } `)
	d.handleNotification("thread/settings/updated", payloadB)
	var retryCount int
	_ = store.db.QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id=? AND field=?`, sess.hostedThreadID, postureFieldEffort).Scan(&retryCount)
	if retryCount != firstCount {
		t.Fatalf("pre-second duplicate added evidence %d→%d", firstCount, retryCount)
	}
	finishCausationTurn(t, sess, "first")
	second := requested(t, sess, postureFieldEffort, "high")
	fake.setTurnStartResult(map[string]any{"turn": map[string]any{"id": "turn-second"}})
	if response := postTurn(t, sess, context.Background(), "second", "second prompt"); response.Code != http.StatusOK {
		t.Fatalf("second turn/start status = %d, body = %s", response.Code, response.Body.String())
	}
	d.handleNotification("thread/settings/updated", payloadB)
	f, _, _ := fieldStrings(t, store, sess.hostedThreadID, postureFieldEffort)
	if first == second || f.DesiredEventID == nil || *f.DesiredEventID != second || f.DesiredDisposition != metadataDispositionConfirmed {
		t.Fatalf("second identical causal request was suppressed: %+v", f)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id=? AND field=?`, sess.hostedThreadID, postureFieldEffort).Scan(&count); err != nil {
		t.Fatal(err)
	}
	// A reordered encoding with no newly queued request is a retry of the
	// second causal entry. It must not add an unlinked observation.
	d.handleNotification("thread/settings/updated", payloadA)
	var after int
	_ = store.db.QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id=? AND field=?`, sess.hostedThreadID, postureFieldEffort).Scan(&after)
	if after != count {
		t.Fatalf("reordered duplicate added evidence %d→%d", count, after)
	}
}

// Live captures on both eligible models emitted this exact settings shape.
// Boundary: canonical metadata receives the provider's Ultrafast setting.
func TestUltrafastSettingEvidenceSettlesOnlyTheRequestedTier(t *testing.T) {
	sess, store := causationTestSession(t, "nht_speed_evidence")
	if _, err := store.appendEvent(metadataEvent{HostedThreadID: sess.hostedThreadID, Field: postureFieldServiceTier, Value: "ultrafast", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive}); err != nil {
		t.Fatalf("provider-advertised Ultrafast must be valid canonical intent: %v", err)
	}
	sess.beginTurnCausation(map[string]string{postureFieldServiceTier: "ultrafast"})
	d := newCodexDaemon("", supervisedDaemonLabel)
	d.sessions = map[string]*session{sess.currentThreadID(): sess}
	payload, _ := json.Marshal(map[string]any{"threadId": sess.currentThreadID(), "threadSettings": map[string]any{"serviceTier": "ultrafast"}})
	d.handleNotification("thread/settings/updated", payload)
	f, desired, effective := fieldStrings(t, store, sess.hostedThreadID, postureFieldServiceTier)
	if desired != "ultrafast" || effective != "ultrafast" || f.DesiredDisposition != metadataDispositionConfirmed {
		t.Fatalf("Ultrafast confirmation = %+v desired=%s effective=%s", f, desired, effective)
	}
}

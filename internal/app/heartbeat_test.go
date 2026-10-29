package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	featureheartbeat "github.com/dkroshow/nano/internal/features/heartbeat"
	"github.com/dkroshow/nano/internal/interact"
)

type heartbeatTestClient struct {
	lifecycle      interact.LifecycleState
	turn           interact.Turn
	prompts        []interact.SubmitPromptRequest
	completed      interact.CompletedTurnPage
	lifecycleErr   error
	submitErr      error
	lifecycleReads int
}

func (c *heartbeatTestClient) ReadAvailability(context.Context, string) (interact.LifecycleState, error) {
	c.lifecycleReads++
	return c.lifecycle, c.lifecycleErr
}

func (c *heartbeatTestClient) SubmitPrompt(_ context.Context, request interact.SubmitPromptRequest) (interact.Turn, error) {
	c.prompts = append(c.prompts, request)
	if c.turn.ID == "" {
		c.turn.ID = "aop_wake"
	}
	return c.turn, c.submitErr
}

func (c *heartbeatTestClient) SubscribeCompletedTurns(context.Context, interact.PageRequest) (interact.CompletedTurnPage, error) {
	return c.completed, nil
}

func heartbeatTestService(t *testing.T, client *heartbeatTestClient) (*featureheartbeat.Service, featureheartbeat.Attachment) {
	t.Helper()
	store := openTestHeartbeatStore(t)
	h := featureheartbeat.NewService(store, interact.NewHTTPClient((&server{}).defaults.APIOrigin, nil), true, nil)
	h.Client = client
	rec, err := store.CreateAttachment(testHeartbeatInput("nht_target"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return h, rec
}

func TestHeartbeatUsesInteractAndSkipsUnavailableTargets(t *testing.T) {
	client := &heartbeatTestClient{lifecycle: interact.LifecycleState{Availability: "available", AttachmentState: "live"}}
	h, rec := heartbeatTestService(t, client)
	now := time.Now().UTC()
	h.EvaluateDue(context.Background(), rec, 100, now)
	if len(client.prompts) != 1 || !client.prompts[0].RequireIdle || client.prompts[0].Origin != interact.SystemOrigin || client.prompts[0].Prompt != rec.Prompt {
		t.Fatalf("interact prompts = %#v", client.prompts)
	}
	rows, err := h.Store.ListWakeLog(rec.ID, 10)
	if err != nil || len(rows) != 1 || rows[0].Outcome != featureheartbeat.OutcomeDelivered || rows[0].UsageState != featureheartbeat.UsagePending {
		t.Fatalf("wake log = %#v, %v", rows, err)
	}

	client.lifecycle.Availability = "awaiting-human"
	client.lifecycle.AvailabilityReasons = []string{"pending_interaction"}
	h.EvaluateDue(context.Background(), rec, 101, now.Add(time.Minute))
	if len(client.prompts) != 1 {
		t.Fatalf("waiting target received prompt: %#v", client.prompts)
	}
	rows, _ = h.Store.ListWakeLog(rec.ID, 10)
	if rows[0].Gate != featureheartbeat.GateHardFacts || rows[0].AvailabilityState != "awaiting-human" {
		t.Fatalf("waiting decision = %#v", rows[0])
	}
}

func TestHeartbeatWakeIsIdempotentPerDueMinute(t *testing.T) {
	client := &heartbeatTestClient{lifecycle: interact.LifecycleState{Availability: "available", AttachmentState: "live"}}
	h, rec := heartbeatTestService(t, client)
	now := time.Now().UTC()
	h.EvaluateDue(context.Background(), rec, 150, now)
	h.EvaluateDue(context.Background(), rec, 150, now)
	if len(client.prompts) != 1 || client.lifecycleReads != 1 {
		t.Fatalf("duplicate minute: prompts=%d lifecycle reads=%d", len(client.prompts), client.lifecycleReads)
	}
}

func TestManualPauseSkipsBeforeInteractRead(t *testing.T) {
	client := &heartbeatTestClient{lifecycle: interact.LifecycleState{Availability: "available", AttachmentState: "live"}}
	h, rec := heartbeatTestService(t, client)
	paused, err := h.Store.PauseAttachment(rec.ID, "operator review", featureheartbeat.Actor{Kind: "operator", Name: "Operator"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h.EvaluateDue(context.Background(), paused, 151, time.Now())
	if len(client.prompts) != 0 || client.lifecycleReads != 0 {
		t.Fatalf("manual pause: prompts=%d lifecycle reads=%d", len(client.prompts), client.lifecycleReads)
	}
}

func TestHeartbeatGlobalCapAndTargetGonePause(t *testing.T) {
	client := &heartbeatTestClient{lifecycle: interact.LifecycleState{Availability: "available", AttachmentState: "live"}}
	h, rec := heartbeatTestService(t, client)
	h.MaxWakesHour = 1
	now := time.Now().UTC()
	h.EvaluateDue(context.Background(), rec, 200, now)
	h.EvaluateDue(context.Background(), rec, 201, now.Add(time.Minute))
	if len(client.prompts) != 1 {
		t.Fatalf("cap admitted %d prompts", len(client.prompts))
	}
	rows, _ := h.Store.ListWakeLog(rec.ID, 10)
	if rows[0].Gate != featureheartbeat.GateGlobalCap || rows[0].Outcome != featureheartbeat.OutcomeSkipped {
		t.Fatalf("cap decision = %#v", rows[0])
	}

	client.lifecycle = interact.LifecycleState{Availability: "unreachable", AvailabilityReasons: []string{"exited"}, AttachmentState: "exited"}
	h.EvaluateDue(context.Background(), rec, 202, now.Add(2*time.Minute))
	paused, err := h.Store.Attachment(rec.ID)
	if err != nil || paused.State != featureheartbeat.StatePaused || !strings.Contains(paused.PauseReason, "target is gone") {
		t.Fatalf("gone pause = %#v, %v", paused, err)
	}
}

func TestPausedHeartbeatWakesOnlyForPrePingArrival(t *testing.T) {
	client := &heartbeatTestClient{lifecycle: interact.LifecycleState{Availability: "available", AttachmentState: "live"}}
	h, rec := heartbeatTestService(t, client)
	now := time.Now().UTC()
	revisionInput := featureheartbeat.ScriptRevisionInput{Name: "test attachment copy", Source: "process.exit(0)", DefaultSettings: json.RawMessage(`{}`), Actor: featureheartbeat.Actor{Kind: "operator"}}
	_, revision, err := h.Store.CreateScript(revisionInput, now)
	if err != nil {
		t.Fatal(err)
	}
	rec, err = h.Store.UpdateAttachment(rec.ID, featureheartbeat.AttachmentPatch{PrePingSet: true, PrePing: &featureheartbeat.PrePingReference{ScriptID: revision.ScriptID, Revision: 1, Settings: json.RawMessage(`{}`)}}, "add inbox template", featureheartbeat.Actor{Kind: "operator"}, now)
	if err != nil {
		t.Fatal(err)
	}
	rec, err = h.Store.PauseAttachment(rec.ID, "waiting for reviewer", featureheartbeat.Actor{Kind: "operator"}, now)
	if err != nil {
		t.Fatal(err)
	}
	h.PrePing = func(context.Context, featureheartbeat.ScriptRevision, featureheartbeat.PrePingInput) (featureheartbeat.PrePingResult, featureheartbeat.PrePingAudit, error) {
		return featureheartbeat.PrePingResult{Decision: featureheartbeat.PrePingWake, Prompt: "review arrived"}, featureheartbeat.PrePingAudit{ScriptID: revision.ScriptID, Revision: 1, Result: featureheartbeat.PrePingWake, PromptChanged: true}, nil
	}
	h.EvaluateDue(context.Background(), rec, 300, now)
	if len(client.prompts) != 1 || !strings.Contains(client.prompts[0].Prompt, "waiting for reviewer") || !strings.Contains(client.prompts[0].Prompt, "review arrived") || !strings.Contains(client.prompts[0].Prompt, "restate the outstanding wait") {
		t.Fatalf("paused arrival prompt = %#v", client.prompts)
	}
}

func TestPrePingFailurePausesAndRunsOnlyOnce(t *testing.T) {
	client := &heartbeatTestClient{lifecycle: interact.LifecycleState{Availability: "available", AttachmentState: "live"}}
	h, rec := heartbeatTestService(t, client)
	now := time.Now().UTC()
	_, revision, err := h.Store.CreateScript(featureheartbeat.ScriptRevisionInput{Name: "broken copy", Source: "process.exit(1)", Actor: featureheartbeat.Actor{Kind: "operator"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	rec, err = h.Store.UpdateAttachment(rec.ID, featureheartbeat.AttachmentPatch{PrePingSet: true, PrePing: &featureheartbeat.PrePingReference{ScriptID: revision.ScriptID, Revision: 1, Settings: json.RawMessage(`{}`)}}, "add template", featureheartbeat.Actor{Kind: "operator"}, now)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	h.PrePing = func(context.Context, featureheartbeat.ScriptRevision, featureheartbeat.PrePingInput) (featureheartbeat.PrePingResult, featureheartbeat.PrePingAudit, error) {
		runs++
		return featureheartbeat.PrePingResult{Decision: featureheartbeat.PrePingFail, Reason: "broken"}, featureheartbeat.PrePingAudit{ScriptID: revision.ScriptID, Revision: 1, Result: featureheartbeat.PrePingFail, Reason: "broken"}, nil
	}
	h.EvaluateDue(context.Background(), rec, 350, now)
	paused, err := h.Store.Attachment(rec.ID)
	if err != nil || paused.State != featureheartbeat.StatePaused || paused.PrePingFailureRevision != 1 || len(client.prompts) != 1 {
		t.Fatalf("failure latch = %#v prompts=%d err=%v", paused, len(client.prompts), err)
	}
	h.EvaluateDue(context.Background(), paused, 351, now.Add(time.Minute))
	if runs != 1 || len(client.prompts) != 1 {
		t.Fatalf("failed configuration reran: runs=%d prompts=%d", runs, len(client.prompts))
	}
}

func TestPausedHeartbeatLatchesPrePingFailureWithoutLosingPauseReason(t *testing.T) {
	client := &heartbeatTestClient{lifecycle: interact.LifecycleState{Availability: "available", AttachmentState: "live"}}
	h, rec := heartbeatTestService(t, client)
	now := time.Now().UTC()
	_, revision, err := h.Store.CreateScript(featureheartbeat.ScriptRevisionInput{Name: "broken copy", Source: "process.exit(1)", Actor: featureheartbeat.Actor{Kind: "operator"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	rec, err = h.Store.UpdateAttachment(rec.ID, featureheartbeat.AttachmentPatch{PrePingSet: true, PrePing: &featureheartbeat.PrePingReference{ScriptID: revision.ScriptID, Revision: 1, Settings: json.RawMessage(`{}`)}}, "add template", featureheartbeat.Actor{Kind: "operator"}, now)
	if err != nil {
		t.Fatal(err)
	}
	rec, err = h.Store.PauseAttachment(rec.ID, "waiting for reviewer", featureheartbeat.Actor{Kind: "operator", Name: "Operator"}, now)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	h.PrePing = func(context.Context, featureheartbeat.ScriptRevision, featureheartbeat.PrePingInput) (featureheartbeat.PrePingResult, featureheartbeat.PrePingAudit, error) {
		runs++
		return featureheartbeat.PrePingResult{Decision: featureheartbeat.PrePingFail, Reason: "broken"}, featureheartbeat.PrePingAudit{ScriptID: revision.ScriptID, Revision: 1, Result: featureheartbeat.PrePingFail, Reason: "broken"}, nil
	}
	h.EvaluateDue(context.Background(), rec, 360, now)
	loaded, err := h.Store.Attachment(rec.ID)
	if err != nil || loaded.PauseReason != "waiting for reviewer" || loaded.PrePingFailureRevision != 1 || len(client.prompts) != 0 {
		t.Fatalf("paused failure latch = %#v prompts=%d err=%v", loaded, len(client.prompts), err)
	}
	h.EvaluateDue(context.Background(), loaded, 361, now.Add(time.Minute))
	if runs != 1 || len(client.prompts) != 0 {
		t.Fatalf("paused failure reran: runs=%d prompts=%d", runs, len(client.prompts))
	}
}

func TestHeartbeatReconcilesProviderUsage(t *testing.T) {
	client := &heartbeatTestClient{lifecycle: interact.LifecycleState{Availability: "available", AttachmentState: "live"}}
	h, rec := heartbeatTestService(t, client)
	now := time.Now().UTC()
	h.EvaluateDue(context.Background(), rec, 400, now)
	input, output := int64(21), int64(9)
	client.completed = interact.CompletedTurnPage{Items: []interact.CompletedTurn{{OperationID: "aop_wake", Outcome: "completed", Usage: &interact.TokenUsage{InputTokens: &input, OutputTokens: &output}}}}
	h.ReconcileUsage(context.Background())
	loaded, err := h.Store.Attachment(rec.ID)
	if err != nil || loaded.Usage.PendingWakes != 0 || loaded.Usage.InputTokens != 21 || loaded.Usage.OutputTokens != 9 {
		t.Fatalf("reconciled usage = %#v, %v", loaded.Usage, err)
	}
}

func TestHeartbeatPublicSubmissionAddsOnlyHeartbeatFooter(t *testing.T) {
	s, _, _, _, target, prompts, _ := stage0Server(t)
	body, _ := json.Marshal(map[string]any{
		"prompt": "continue", "clientActionId": "nano:heartbeat:hba_test:500", "origin": "system", "requireIdle": true,
	})
	recorder := httptest.NewRecorder()
	s.handleAgentTurnSubmit(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/"+target.ID+"/turns", bytes.NewReader(body)), target.ID)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", recorder.Code, recorder.Body.String())
	}
	select {
	case delivered := <-prompts:
		if delivered != "continue\n\n"+featureheartbeat.Footer {
			t.Fatalf("delivered = %q", delivered)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat prompt was not delivered")
	}
}

func TestAnyHostedAgentCanPauseAndResumeWithAuditedReasons(t *testing.T) {
	s, _, _, manager, target, _, _ := stage0Server(t)
	store := openTestHeartbeatStore(t)
	s.heartbeat = featureheartbeat.NewService(store, interact.NewHTTPClient(s.defaults.APIOrigin, nil), true, nil)
	attachment, err := store.CreateAttachment(testHeartbeatInput(target.ID), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"pause", "resume"} {
		body, _ := json.Marshal(map[string]any{
			"reason": action + " for review",
			"actor":  map[string]any{"kind": "hosted-thread", "hostedThreadId": manager.ID},
		})
		recorder := httptest.NewRecorder()
		applicationHandler(s).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/heartbeat/attachments/"+attachment.ID+"/"+action, bytes.NewReader(body)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", action, recorder.Code, recorder.Body.String())
		}
	}
	var count int
	if err := store.DB.QueryRow(`SELECT count(*) FROM heartbeat_attachment_events WHERE attachment_id=? AND actor_hosted_thread_id=? AND event_type IN ('paused','resumed')`, attachment.ID, manager.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("agent audit count = %d, %v", count, err)
	}
}

func TestHeartbeatAttachmentAPIRequiresReasonAndRejectsClosedTarget(t *testing.T) {
	s, _, _, manager, target, _, _ := stage0Server(t)
	store := openTestHeartbeatStore(t)
	s.heartbeat = featureheartbeat.NewService(store, interact.NewHTTPClient(s.defaults.APIOrigin, nil), true, nil)
	attachment, err := store.CreateAttachment(testHeartbeatInput(target.ID), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	missingReason := `{"actor":{"kind":"hosted-thread","hostedThreadId":"` + manager.ID + `"}}`
	recorder := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/heartbeat/attachments/"+attachment.ID+"/pause", strings.NewReader(missingReason)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("pause without reason = %d %s", recorder.Code, recorder.Body.String())
	}
	if err := s.agentExecutions.UpdateSession(target.ID, target.NanoSessionID, target.NativeThreadID, sessionStatusExited, "closed"); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"targetHostedThreadId": target.ID, "cadenceMinutes": 1, "prompt": "continue",
		"actor": map[string]any{"kind": "hosted-thread", "hostedThreadId": manager.ID},
	})
	recorder = httptest.NewRecorder()
	s.agentController().HandleHeartbeatAttachmentCreate(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/heartbeat/attachments", bytes.NewReader(body)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("closed target attach = %d %s", recorder.Code, recorder.Body.String())
	}
}

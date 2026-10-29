package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreturns "github.com/dkroshow/nano/internal/core/turns"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/observability"
	"github.com/dkroshow/nano/internal/sessions"
)

func TestInteractSystemPromptQueuesIdempotentlyWithNotice(t *testing.T) {
	s, target := interactTestServer(t, true)
	if err := s.notices.Set(target, "mail", " unread line exactly "); err != nil {
		t.Fatal(err)
	}
	client := interactTestClient(t, s)
	request := interact.SubmitPromptRequest{ThreadRef: target, Prompt: "wake", ClientID: "nano:feature:1", Origin: interact.SystemOrigin}
	first, err := client.SubmitPrompt(context.Background(), request)
	if err != nil || first.Status != agentTurnQueued || first.Origin != turnOriginSystem {
		t.Fatalf("first submit = %#v, %v", first, err)
	}
	second, err := client.SubmitPrompt(context.Background(), request)
	if err != nil || second.ID != first.ID {
		t.Fatalf("idempotent submit = %#v, %v; first %#v", second, err, first)
	}
	record, ok := s.agentExecutions.Turn(first.ID)
	if !ok {
		t.Fatal("submitted operation missing")
	}
	queued, ok := s.queuedTurns.Get(record.TransportActionID)
	if !ok || queued.Text != "wake\n\n[from nano]\n\n unread line exactly " || s.queuedTurns.QueuedCount("codex", "native-target", target) != 1 {
		t.Fatalf("queued prompt = %#v", queued)
	}
}

func TestInteractTurnSubmitRejectsInvalidOriginBoundaries(t *testing.T) {
	s, target := interactTestServer(t, false)
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "bad origin", body: `{"prompt":"wake","clientActionId":"feature:1","origin":"operator"}`, want: "origin must be agent or system"},
		{name: "system with sender", body: `{"prompt":"wake","clientActionId":"nano:feature:1","origin":"system","senderThreadRef":"sender"}`, want: "senderThreadRef is not used for system-origin turn submissions"},
		{name: "system without reserved id", body: `{"prompt":"wake","clientActionId":"feature:1","origin":"system"}`, want: "system-origin clientActionId must use the reserved nano: prefix"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/"+target+"/turns", strings.NewReader(test.body))
			response := httptest.NewRecorder()
			s.hostedController().HandleTurnSubmit(response, request, target)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("response = %d %q, want 400 containing %q", response.Code, response.Body.String(), test.want)
			}
		})
	}
}

func TestInteractCompletedTurnFeedAdvancesDurableCursor(t *testing.T) {
	s, target := interactTestServer(t, false)
	completed := mustCompletedStore(t)
	s.completedTurns = hostedmodel.NewCompletedTurnSource(completed)
	if _, _, err := recordTestCompletedTurn(completed, coreturns.Finish{HostedThreadID: target, Provider: "codex", TurnID: "turn-1", FinalMessageID: "final-1", Outcome: "completed", At: time.Now(), Source: "adapter"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := recordTestCompletedTurn(completed, coreturns.Finish{HostedThreadID: target, Provider: "codex", TurnID: "turn-2", FinalMessageID: "final-2", Outcome: "completed", At: time.Now(), Source: "adapter"}); err != nil {
		t.Fatal(err)
	}

	page, err := interactTestClient(t, s).SubscribeCompletedTurns(context.Background(), interact.PageRequest{ThreadRef: target, Cursor: "turn-1", Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Provider != "codex" || page.Items[0].TurnID != "turn-2" || page.NextCursor != "turn-2" {
		t.Fatalf("completed page = %#v, %v", page, err)
	}
	empty, err := interactTestClient(t, s).SubscribeCompletedTurns(context.Background(), interact.PageRequest{ThreadRef: target, Cursor: page.NextCursor, Limit: 10})
	if err != nil || len(empty.Items) != 0 || empty.NextCursor != "turn-2" {
		t.Fatalf("empty completed page = %#v, %v", empty, err)
	}
}

// Failure caught: the feed hashed the action a prompt was delivered under as
// if it were the caller's own, so every notice named an operation Nano does
// not have (GET /turn-operations answered 404). A turn names the operation
// delivered under its action, and a turn no operation sent, such as a
// person's prompt, names none. Boundary: the public completed-turn feed.
func TestInteractCompletedTurnFeedCarriesTerminalOutcomeOperationAndUsage(t *testing.T) {
	s, target := interactTestServer(t, false)
	completed := hostedmodel.NewCompletedTurnStore(s.queuedTurns.Database())
	s.completedTurns = hostedmodel.NewCompletedTurnSource(completed)
	op, _, err := fixtureBeginTurn(s.agentExecutions, target, "action-failed", "hash-failed")
	if err != nil {
		t.Fatal(err)
	}
	input, cached, output := int64(13), int64(5), int64(8)
	if _, created, err := recordTestCompletedTurn(completed, coreturns.Finish{HostedThreadID: target, Provider: "codex", TurnID: "turn-failed", ActionID: op.TransportActionID, Outcome: agentTurnFailed, At: time.Now(), Source: "adapter", Usage: coreturns.Usage{InputTokens: &input, CachedInputTokens: &cached, OutputTokens: &output}}); err != nil || !created {
		t.Fatalf("publish failed turn: created=%v err=%v", created, err)
	}
	if _, created, err := recordTestCompletedTurn(completed, coreturns.Finish{HostedThreadID: target, Provider: "codex", TurnID: "turn-person", ActionID: "person-action", Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter"}); err != nil || !created {
		t.Fatalf("publish person's turn: created=%v err=%v", created, err)
	}
	page, err := interactTestClient(t, s).SubscribeCompletedTurns(context.Background(), interact.PageRequest{ThreadRef: target})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("terminal page = %#v, %v", page, err)
	}
	item := page.Items[0]
	if item.Outcome != agentTurnFailed || item.FinalMessageID != "" || item.OperationID != op.ID || item.Usage == nil ||
		item.Usage.InputTokens == nil || *item.Usage.InputTokens != input || item.Usage.CachedInputTokens == nil || *item.Usage.CachedInputTokens != cached || item.Usage.OutputTokens == nil || *item.Usage.OutputTokens != output {
		t.Fatalf("terminal item = %#v; want operation %s", item, op.ID)
	}
	if person := page.Items[1]; person.TurnID != "turn-person" || person.OperationID != "" {
		t.Fatalf("person's turn = %#v; want no operation", person)
	}
}

func TestInteractLifecycleReportsCoordinatorFacts(t *testing.T) {
	var projection *sessions.LiveProjection
	s, target := interactTestServer(t, true, func(cfg *sessionConfig) { projection = cfg.Projection })
	projection.RecordDiagnostic(observability.ThreadDiagnostic{Code: "boot_incomplete_outbox", Message: "synthetic parked prompt", Source: "turn-store-boot", ActionID: "parked"}, true)
	state, err := interactTestClient(t, s).ReadLifecycle(context.Background(), target)
	if err != nil || !state.Attached || !state.Ready || !state.Working || !state.TurnActive || state.Availability != availabilityBusy || state.AttachmentState != "live" {
		t.Fatalf("lifecycle state = %#v, %v", state, err)
	}
	if len(state.ThreadDiagnostics) != 1 || state.ThreadDiagnostics[0].Code != "boot_incomplete_outbox" || state.ThreadDiagnostics[0].Source != "turn-store-boot" {
		t.Fatalf("lifecycle diagnostics = %#v", state.ThreadDiagnostics)
	}
}

func interactTestClient(t *testing.T, s *server) *interact.HTTPClient {
	t.Helper()
	mux := http.NewServeMux()
	mountRoutes(mux, hostedThreadRoutes(s, "/api/v1/hosted-threads"))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	return interact.NewHTTPClient(httpServer.URL, httpServer.Client())
}

// interactTestServer holds one hosted thread over the server's stores, which
// share one threads database as they do in Nano.
func interactTestServer(t *testing.T, working bool, configure ...func(*sessionConfig)) (*server, string) {
	t.Helper()
	directory := t.TempDir()
	stores, err := openThreadRuntimeStores(context.Background(), directory, filepath.Join(directory, "turn-completed.sqlite"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Control.Close() })
	executions, queue, attempts, turnStore := stores.Executions, stores.Queue, stores.Attempts, stores.Turns
	record, _, err := fixtureBeginSession(executions, "target", "target-hash", "codex", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession(record.ID, "nano-target", "native-target", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}
	notices := interact.NewNotices()
	cfg := sessionConfig{ID: "nano-target", Provider: "codex", HostedThreadID: record.ID, CreatedAt: time.Now(), Projection: newLiveProjection("codex", record.ID, "nano-target"), AgentExecutions: executions, QueuedTurns: queue, SendAttempts: attempts, Turns: turnStore, AppendUnread: notices.Append}
	for _, apply := range configure {
		apply(&cfg)
	}
	sess := configuredFixtureSession(t, cfg, "native-target", "ready", "", working)
	return testServerWithSessions(&server{
		Registry: newSessionRegistry(), agentExecutions: executions,
		queuedTurns: queue, sendAttempts: attempts, turns: turnStore, notices: notices,
	}, map[string]*session{sess.ID(): sess}), record.ID
}

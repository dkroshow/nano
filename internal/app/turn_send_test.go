package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/history"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
	"github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcript"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func BenchmarkSendAttemptStoreListThreadWithUnrelatedHistory(b *testing.B) {
	directory := b.TempDir()
	stores, err := openThreadRuntimeStores(context.Background(), directory, filepath.Join(directory, "turn-completed.sqlite"), "benchmark")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = stores.Control.Close() })

	attempts := make([]sendAttemptRecord, 0, 10_651)
	for i := 0; i < 10_648; i++ {
		attempts = append(attempts, sendAttemptRecord{
			ClientActionID: fmt.Sprintf("unrelated-%05d", i), Provider: "codex", NativeThreadID: "unrelated-thread",
			Status: sendAttemptFailed, UpdatedAt: time.Unix(int64(i), 0).UTC().Format(time.RFC3339Nano),
		})
	}
	for i := 0; i < 3; i++ {
		attempts = append(attempts, sendAttemptRecord{
			ClientActionID: fmt.Sprintf("matching-%d", i), Provider: "codex", NativeThreadID: "target-thread",
			Status: sendAttemptAccepted, UpdatedAt: time.Unix(int64(20_000+i), 0).UTC().Format(time.RFC3339Nano),
		})
	}
	if err := prompts.SaveSendAttempts(context.Background(), stores.Control.CanonicalDatabase(), attempts); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := stores.Attempts.ListThread("codex", "target-thread"); len(got) != 3 {
			b.Fatalf("matching attempts = %d, want 3", len(got))
		}
	}
}

// The browser's delivery journal read through the one writer connection, so
// at a restart every open tab's journal reads queued in front of
// registrations and durable writes (99 s of waits in a boot's first 30 s).
// It reads through the control store's read pool, and an attempt committed
// before the read is still listed. Boundary: the session's journal route over
// the real control store, with a transaction holding the writer.
func TestTurnDeliveryListDoesNotWaitForTheWriter(t *testing.T) {
	directory := t.TempDir()
	stores, err := openThreadRuntimeStores(context.Background(), directory, filepath.Join(directory, "turn-completed.sqlite"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Control.Close() })
	if err := prompts.SaveSendAttempts(context.Background(), stores.Control.CanonicalDatabase(), []sendAttemptRecord{
		{ClientActionID: "journal-action", Provider: "codex", NativeThreadID: "target", TextHash: "journal-hash", Status: sendAttemptAccepted, CreatedAt: "2026-10-02T11:00:00Z", UpdatedAt: "2026-10-02T11:00:01Z"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := prompts.SaveQueuedTurns(context.Background(), stores.Control.CanonicalDatabase(), []queuedTurnRecord{{
		ClientActionID: "journal-action", Provider: "codex", HostedThreadID: "hosted", NativeThreadID: "target", Text: "journal text",
		TextHash: "journal-hash", RequestHash: "journal-hash", State: "accepted", Origin: "human", CreatedAt: "2026-10-02T11:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}
	sess := newFixtureSession(sessionConfig{ID: "nano-session", Provider: "codex", HostedThreadID: "hosted", SendAttempts: stores.Attempts, QueuedTurns: stores.Queue, Metadata: stores.Control}, "target")
	tx, err := stores.Control.CanonicalDatabase().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	list := httptest.NewRecorder()
	listed := make(chan struct{})
	go func() {
		sess.HandleTurnDeliveryList(list, httptest.NewRequest(http.MethodGet, "/api/sessions/nano-session/turn-delivery?thread=target", nil))
		close(listed)
	}()
	select {
	case <-listed:
	case <-time.After(3 * time.Second):
		t.Fatal("the delivery journal waited for the writer connection")
	}
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"clientActionId":"journal-action"`) {
		t.Fatalf("journal = %d %s", list.Code, list.Body.String())
	}
}

func TestSendAttemptStoreListThreadPreservesStatusFieldsAndOrdering(t *testing.T) {
	directory := t.TempDir()
	stores, err := openThreadRuntimeStores(context.Background(), directory, filepath.Join(directory, "turn-completed.sqlite"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Control.Close() })
	if err := prompts.SaveSendAttempts(context.Background(), stores.Control.CanonicalDatabase(), []sendAttemptRecord{
		{ClientActionID: "older", Provider: "codex", NativeThreadID: "target", TextHash: "older-hash", Status: sendAttemptAccepted, RunID: "old-run", CreatedAt: "2026-09-15T01:00:00Z", UpdatedAt: "2026-09-15T01:01:00Z", Confirmed: true, ProviderTurnID: "older-turn"},
		{ClientActionID: "newer-orphan", Provider: "codex", NativeThreadID: "target", TextHash: "newer-hash", Status: sendAttemptRefused, RunID: "new-run", Reason: "legacy orphan", CreatedAt: "2026-09-15T02:00:00Z", UpdatedAt: "2026-09-15T02:01:00Z", CanonicalMessageID: "newer-message"},
		{ClientActionID: "parked", Provider: "codex", NativeThreadID: "target", TextHash: "parked-hash", Status: "unknown", CreatedAt: "2026-09-15T00:00:00Z", UpdatedAt: "2026-09-15T00:01:00Z"},
		{ClientActionID: "other-provider", Provider: "claude", NativeThreadID: "target", Status: sendAttemptAccepted, UpdatedAt: "2026-09-15T03:01:00Z"},
		{ClientActionID: "other-thread", Provider: "codex", NativeThreadID: "other", Status: sendAttemptAccepted, UpdatedAt: "2026-09-15T04:01:00Z"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := prompts.SaveQueuedTurns(context.Background(), stores.Control.CanonicalDatabase(), []queuedTurnRecord{{
		ClientActionID: "parked", Provider: "codex", HostedThreadID: "hosted", NativeThreadID: "target", Text: "queue text",
		PresentationText: "presentation text", TextHash: "parked-hash", RequestHash: "request", State: "parked", Origin: "human", CreatedAt: "2026-09-15T00:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}

	got := stores.Attempts.ListThread("codex", "target")
	if len(got) != 3 || got[0].ClientActionID != "newer-orphan" || got[1].ClientActionID != "older" || got[2].ClientActionID != "parked" {
		t.Fatalf("thread ordering/filter = %+v", got)
	}
	if got[0].Reason != "legacy orphan" || got[0].CanonicalMessageID != "newer-message" || got[1].TextHash != "older-hash" || !got[1].Confirmed || got[1].ProviderTurnID != "older-turn" {
		t.Fatalf("response fields changed: %+v", got)
	}
	if got[2].Status != "parked" {
		t.Fatalf("outbox status override = %q, want parked", got[2].Status)
	}
	queue, err := prompts.LoadQueuedTurns(context.Background(), stores.Control.CanonicalDatabase())
	if err != nil || len(queue) != 1 || queue[0].Text != "queue text" || queue[0].PresentationText != "presentation text" {
		t.Fatalf("queue text changed: %+v, %v", queue, err)
	}
}

// settlementFixture is one saved provider transcript indexed by a real
// catalog: an old prompted turn, three turns no Nano prompt created (typed in
// the provider's own client), a newer prompted turn, and a prompted turn still
// running. The shapes follow each provider's saved format; text and ids are
// synthetic.
type settlementFixture struct {
	native     string
	root       history.SourceRoot
	path       string            // the saved transcript file
	turns      map[string]string // stored prompt -> exact provider turn id recorded at acceptance
	savedTurns int
}

var settlementFixtureTurns = []string{"old", "native-1", "native-2", "native-3", "new", "running"}

func writeSettlementFixture(t *testing.T, provider string) settlementFixture {
	t.Helper()
	dir := t.TempDir()
	var data strings.Builder
	fixture := settlementFixture{turns: map[string]string{}, savedTurns: len(settlementFixtureTurns)}
	stored := func(name string) bool { return !strings.HasPrefix(name, "native-") }
	switch provider {
	case "codex":
		fixture.native = "codex-settle"
		fixture.root = history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: dir}
		data.WriteString(`{"type":"session_meta","payload":{"id":"codex-settle","cwd":"/disposable","timestamp":"2026-09-15T20:00:00Z"}}` + "\n")
		for _, name := range settlementFixtureTurns {
			turn := "turn-" + name
			if stored(name) {
				fixture.turns[name] = turn
			}
			fmt.Fprintf(&data, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":%q}}\n", turn)
			fmt.Fprintf(&data, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":%q}}\n", name+" prompt")
			fmt.Fprintf(&data, "{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"id\":%q,\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}}\n", name+"-answer", name+" answer")
			if name != "running" {
				fmt.Fprintf(&data, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"turn_id\":%q}}\n", turn)
			}
		}
		fixture.turns["unproven"] = "turn-absent"
		fixture.path = filepath.Join(dir, "rollout-codex-settle.jsonl")
	case "claude":
		fixture.native = "claude-settle"
		fixture.root = history.SourceRoot{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: dir}
		// Claude saved the prompt whose delivery answer was lost; its row
		// carries the UUID Nano stamped, so it settles though uncertain.
		fixture.savedTurns = len(settlementFixtureTurns) + 1
		fmt.Fprintf(&data, `{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"unanswered prompt"}}`+"\n", submissionEventID("hosted", "unanswered"))
		data.WriteString(`{"type":"assistant","uuid":"unanswered-event","sessionId":"claude-settle","message":{"id":"unanswered-message","role":"assistant","content":[{"type":"text","text":"unanswered answer"}],"stop_reason":"end_turn"}}` + "\n")
		for index, name := range settlementFixtureTurns {
			uuid := name + "-native-row"
			if stored(name) {
				fixture.turns[name] = fmt.Sprintf("claude:shim:%d", index+1)
				uuid = submissionEventID("hosted", name)
			}
			stop := `"end_turn"`
			if name == "running" {
				stop = "null"
			}
			fmt.Fprintf(&data, `{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":%q}}`+"\n", uuid, name+" prompt")
			fmt.Fprintf(&data, `{"type":"assistant","uuid":%q,"sessionId":"claude-settle","message":{"id":%q,"role":"assistant","content":[{"type":"text","text":%q}],"stop_reason":%s}}`+"\n", name+"-event", name+"-message", name+" answer", stop)
		}
		fixture.turns["unproven"] = "claude:shim:99"
		fixture.path = filepath.Join(dir, "project", "claude-settle.jsonl")
	case "grok":
		fixture.native = "22222222-2222-4222-8222-222222222222"
		fixture.root = history.SourceRoot{Provider: history.ProviderGrok, Kind: history.RootGrokSessions, Path: dir}
		session := filepath.Join(dir, "-workspace", fixture.native)
		writeSettlementFile(t, filepath.Join(session, "summary.json"), `{"info":{"id":"`+fixture.native+`","cwd":"/workspace"}}`)
		for index, name := range settlementFixtureTurns {
			prompt := "prompt-" + name
			if stored(name) {
				fixture.turns[name] = prompt
			}
			at := 1893456000 + index*10
			fmt.Fprintf(&data, `{"timestamp":%d,"method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":%q}},"_meta":{"eventId":%q}}}`+"\n", at, fixture.native, name+" prompt", name+"-user-event")
			fmt.Fprintf(&data, `{"timestamp":%d,"method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":%q}},"_meta":{"promptId":%q,"eventId":%q}}}`+"\n", at+1, fixture.native, name+" answer", prompt, name+"-agent-event")
			if name != "running" {
				fmt.Fprintf(&data, `{"timestamp":%d,"method":"_x.ai/session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"turn_completed","prompt_id":%q,"stop_reason":"end_turn"}}}`+"\n", at+2, fixture.native, prompt)
			}
		}
		fixture.turns["unproven"] = "prompt-absent"
		fixture.path = filepath.Join(session, "updates.jsonl")
	}
	writeSettlementFile(t, fixture.path, data.String())
	return fixture
}

// seedUncertainTransactionalTurn stores a prompt whose delivery got no answer.
func seedUncertainTransactionalTurn(t *testing.T, store *prompts.Store, hosted string, provider model.Provider, native, action, prompt string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := store.Submit(ctx, prompts.Submission{
		HostedThreadID: hosted, Provider: provider, NativeThreadID: native, ActionID: action,
		ProviderText: prompt, PresentationText: prompt, TextHash: sendTextHash(prompt), RequestHash: sendTextHash(prompt), Origin: prompts.HumanOrigin,
	}); err != nil {
		t.Fatal(err)
	}
	token := "fixture-claim-" + action
	if _, claimed, err := store.Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: token, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim fixture %s: claimed=%v err=%v", action, claimed, err)
	}
	if _, err := store.Uncertain(ctx, prompts.Uncertainty{ActionID: action, ClaimToken: token, AttachmentEpoch: 1, Reason: "no answer"}); err != nil {
		t.Fatal(err)
	}
}

func writeSettlementFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// settlementHarness is one provider's settlement fixture indexed by a real
// catalog, with the fixture's prompts stored: "old" and "new" are saved in
// ended turns, "running" is saved in a turn still open, "unproven" names a
// turn the provider never saved, and "unanswered" got no delivery answer.
type settlementHarness struct {
	t       *testing.T
	fixture settlementFixture
	// historyPath is the catalog's database file.
	historyPath string
	service     *sidebar.CatalogSource
	server      *server
	prompts     *prompts.Store
	queue       *prompts.LegacyQueue
	attempts    *prompts.LegacyJournal
	control     *hostedstore.MetadataStore
	// interrupts counts the turn/interrupt calls a Codex session made.
	interrupts atomic.Int32
	runtimes   map[*session]*providerRuntimeFixture
	readers    map[*session]savedPromptReader
}

func newSettlementHarness(t *testing.T, provider string) *settlementHarness {
	t.Helper()
	ctx := context.Background()
	fixture := writeSettlementFixture(t, provider)
	historyPath := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := history.Open(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	service, err := newThreadCatalogService(ctx, store, threadindex.Options{
		Roots: []history.SourceRoot{fixture.root},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	service.RequestRefresh() // the explicit discovery a new store needs
	waitForCatalogWithin(t, time.Minute, func() bool {
		id, err := service.ResolveThreadReference(ctx, fixture.native)
		if err != nil {
			return false
		}
		page, err := service.LoadTranscript(ctx, id, threadindex.TranscriptQuery{Limit: 100, HiddenSpecified: true, View: threadindex.DefaultThreadTranscriptView})
		return err == nil && len(page.Turns) == fixture.savedTurns
	})

	dir := t.TempDir()
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "attempts.json"), "run")
	if err != nil {
		t.Fatal(err)
	}
	prompts := mustBindTransactionalTurnStore(t, queue, attempts)
	seedUncertainTransactionalTurn(t, prompts, "hosted", model.Provider(provider), fixture.native, "unanswered", "unanswered prompt")
	for _, name := range []string{"old", "new", "running", "unproven"} {
		native := fixture.native
		if provider == "claude" && name == "old" {
			// A Claude thread's first prompt is accepted before Claude names
			// its session, under the shim handle that stands in for it.
			native = "cs_shim-handle"
		}
		seedAcceptedTransactionalTurn(t, prompts, "hosted", model.Provider(provider), native, name, fixture.turns[name], name+" prompt")
	}
	h := &settlementHarness{t: t, fixture: fixture, historyPath: historyPath, service: service, server: &server{catalog: service, turns: prompts},
		prompts: prompts, queue: queue, attempts: attempts}
	if provider == "claude" {
		// The child that ran under the shim handle announced the saved
		// session; its init recorded that resolution, as captureInit does.
		h.control, _ = newTestHostedControl(t)
		if err := h.control.EnsureHostedThread("hosted", "claude", presentationShared, "", "", ""); err != nil {
			t.Fatal(err)
		}
		if err := h.control.ClaimNativeID("hosted", "claude", "cs_shim-handle", true); err != nil {
			t.Fatal(err)
		}
		pair := nativeResolution{Handle: "cs_shim-handle", Native: fixture.native}
		if err := h.control.ClaimNativeIDResolving("hosted", "claude", fixture.native, false, &pair); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// attach attaches a session to the fixture thread as a restart does. sender,
// if set, is the provider's answer to a live delivery.
func (h *settlementHarness) attach(provider string, sender func(context.Context, string) (json.RawMessage, error), configure ...func(*sessionConfig)) *session {
	cfg := sessionConfig{ID: "nano-session", Provider: provider, HostedThreadID: "hosted", Projection: newLiveProjection(provider, "hosted"), Turns: h.prompts, QueuedTurns: h.queue, SendAttempts: h.attempts}
	if h.control != nil {
		cfg.Metadata = h.control
	}
	for _, fn := range configure {
		fn(&cfg)
	}
	f := newProviderRuntimeFixture(h.t, cfg, h.fixture.native, "", func(f *providerRuntimeFixture) {
		f.send = sender
		f.onInterrupt = func() { h.interrupts.Add(1) }
		f.session.ConfigureSavedReaders(nil, h.server.savedPromptsFor(f.session), h.server.hostedCatalog().SavedPromptRows, h.server.hostedCatalog().SavedTurnFinal, nil)
	})
	if h.runtimes == nil {
		h.runtimes = map[*session]*providerRuntimeFixture{}
	}
	h.runtimes[f.session] = f
	if _, err := f.session.SettlePrompts(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	return f.session
}

// appendSaved appends rows to the saved transcript, as the provider does,
// and asks the catalog to index them.
func (h *settlementHarness) appendSaved(t *testing.T, rows string) {
	t.Helper()
	file, err := os.OpenFile(h.fixture.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(rows); err != nil {
		t.Fatal(err)
	}
	file.Close()
	h.service.FileChanged(h.fixture.path)
}

func (h *settlementHarness) state(t *testing.T, action string) prompts.DeliveryState {
	t.Helper()
	record, _, err := h.prompts.Get(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	return record.Outbox.State
}

// Claude gives every prompt its own durable turn. A prompt it takes in while a
// turn works is saved inside that turn, and every prompt that turn consumed
// finishes with its saved final message, whichever execution Claude's live
// result named, and whether that result is recorded before or after the
// saved commit or never. No prompt's operation finishes before its own saved
// row is proven. Usage is the live result's: a turn finished from the saved
// transcript records zeros until its own result reports counts, and a count a
// turn already holds is kept. The answered prompt can settle and finish before
// another member is even accepted. Each member owes its completion until
// then, and owes nothing after. Boundary: the saved-commit settle pass and
// the finished-turn callback's member step, on a real catalog of Claude's
// format.
func TestClaudePromptsAnsweredTogetherFinishWithTheSharedResult(t *testing.T) {
	for _, order := range []string{"result first", "saved first", "answered settles first", "absorbed turn holds usage"} {
		t.Run(order, func(t *testing.T) {
			ctx := context.Background()
			h := newSettlementHarness(t, "claude")
			native := h.fixture.native
			executions := newTestAgentExecutionStore(t, "run-1")
			if _, err := fixtureEnsureSessionWithIdentity(executions, "hosted", "claude", presentationShared, "", "", ""); err != nil {
				t.Fatal(err)
			}
			if err := executions.BindSession("hosted", "nano-session", native, "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
			completed := hostedmodel.NewCompletedTurnStore(h.queue.Database())
			core, err := completed.CoreStore(ctx)
			if err != nil {
				t.Fatal(err)
			}
			type prompt struct{ op, turn, transport string }
			sent := map[string]*prompt{"starter": {turn: "claude:shim:200"}, "correction": {turn: "claude:shim:201"}, "late": {turn: "claude:shim:202"}}
			// An accepted Claude prompt's binding records its own stamped event,
			// as the provider runtime's acceptance does.
			ownBinding := func(p *prompt) prompts.Binding {
				return prompts.Binding{EventID: submissionEventID("hosted", p.transport), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &p.turn}}
			}
			accept := func(p *prompt, text string) {
				t.Helper()
				if _, _, err := h.prompts.Submit(ctx, prompts.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: p.transport,
					ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: prompts.HumanOrigin}); err != nil {
					t.Fatal(err)
				}
				token := "fixture-claim-" + p.transport
				if _, claimed, err := h.prompts.Claim(ctx, prompts.ClaimRequest{HostedThreadID: "hosted", Token: token, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
					t.Fatalf("claim %s: %v %v", p.transport, claimed, err)
				}
				if _, err := h.prompts.Accepted(ctx, prompts.Acceptance{ActionID: p.transport, ClaimToken: token, AttachmentEpoch: 1, Binding: ownBinding(p)}); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"starter", "correction", "late"} {
				op, _, err := fixtureBeginTurn(executions, "hosted", name, "hash-"+name)
				if err != nil {
					t.Fatal(err)
				}
				p := sent[name]
				p.op, p.transport = op.ID, op.TransportActionID
				if order != "answered settles first" || name != "starter" {
					accept(p, name+" prompt")
				}
				if err := executions.MarkTurnDelivery(op.ID, p.turn, agentTurnRunning, nil); err != nil {
					t.Fatal(err)
				}
				if _, _, err := core.Started(ctx, coreturns.Start{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: p.turn, At: time.Now(), Source: "adapter"}); err != nil {
					t.Fatal(err)
				}
			}
			sess := h.attach("claude", nil, func(cfg *sessionConfig) { cfg.AgentExecutions = executions; cfg.CompletedTurns = completed.CoreStore })
			if order == "answered settles first" {
				// The starter is delivered but not yet accepted when the others
				// settle; claimed after attach, so it holds the only claim.
				if _, _, err := h.prompts.Submit(ctx, prompts.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: sent["starter"].transport,
					ProviderText: "starter prompt", PresentationText: "starter prompt", TextHash: sendTextHash("starter prompt"), RequestHash: sendTextHash("starter prompt"), Origin: prompts.HumanOrigin}); err != nil {
					t.Fatal(err)
				}
				if record, claimed, err := h.prompts.Claim(ctx, prompts.ClaimRequest{HostedThreadID: "hosted", Token: "late-claim", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed || record.Outbox.ActionID != sent["starter"].transport {
					t.Fatalf("claim starter: %+v %v %v", record, claimed, err)
				}
			}
			// The live results as the gated result path leaves them: each turn's
			// execution recorded, its answer kept on the open operation.
			input, output := int64(10), int64(5)
			results := func() {
				for name, final := range map[string]string{"correction": "shared", "late": "late"} {
					p := sent[name]
					if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: p.turn,
						FinalMessageID: final + "-final:0", Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter", Usage: coreturns.Usage{InputTokens: &input, OutputTokens: &output}}); err != nil {
						t.Fatal(err)
					}
					if err := executions.ObserveCandidate("claude", native, p.turn, final+"-final:0", "final_answer", final+" answer"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if order != "saved first" {
				results()
			}
			held := int64(7)
			if order == "absorbed turn holds usage" {
				// A retained saved-source finish of the absorbed turn that
				// already counts usage of its own: never overwritten, and no
				// member completion while it does.
				if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: sent["starter"].turn,
					FinalMessageID: "shared-final:0", Outcome: agentTurnCompleted, At: time.Now(), Source: "saved", Usage: coreturns.Usage{InputTokens: &held}}); err != nil {
					t.Fatal(err)
				}
			}
			event := func(name string) string { return submissionEventID("hosted", sent[name].transport) }
			h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"starter prompt"}}
{"type":"assistant","uuid":"tool-event","sessionId":"claude-settle","message":{"id":"tool-message","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"sleep 8"}}],"stop_reason":"tool_use"}}
{"type":"user","uuid":"tool-result-event","sessionId":"claude-settle","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}}
{"type":"attachment","uuid":"queued-attachment","sessionId":"claude-settle","attachment":{"type":"queued_command","commandMode":"prompt","prompt":[{"type":"text","text":"correction prompt"}],"source_uuid":%q}}
{"type":"assistant","uuid":"shared-event","sessionId":"claude-settle","message":{"id":"shared-final","role":"assistant","content":[{"type":"text","text":"shared answer"}],"stop_reason":"end_turn"}}
{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"late prompt"}}
{"type":"assistant","uuid":"late-event","sessionId":"claude-settle","message":{"id":"late-final","role":"assistant","content":[{"type":"text","text":"late answer"}],"stop_reason":"end_turn"}}
`, event("starter"), event("correction"), event("late")))
			settled := func(names ...string) {
				waitForCatalogWithin(t, time.Minute, func() bool {
					if _, err := sess.SettlePrompts(ctx); err != nil {
						return false
					}
					for _, name := range names {
						if h.state(t, sent[name].transport) != prompts.Confirmed {
							return false
						}
					}
					return true
				})
			}
			if order == "answered settles first" {
				settled("correction", "late")
				if op, _ := executions.Turn(sent["correction"].op); op.Status != agentTurnCompleted {
					t.Fatalf("answered operation = %+v; want it finished on its own saved proof", op)
				}
				if op, _ := executions.Turn(sent["starter"].op); threads.TurnTerminal(op.Status) {
					t.Fatalf("starter operation finished before its prompt was accepted: %+v", op)
				}
				starter := sent["starter"]
				if _, err := h.prompts.Accepted(ctx, prompts.Acceptance{ActionID: starter.transport, ClaimToken: "late-claim", AttachmentEpoch: 1, Binding: ownBinding(starter)}); err != nil {
					t.Fatal(err)
				}
			}
			settled("starter", "correction", "late")
			if order == "saved first" {
				// The saved transcript alone decides every answer.
				for name, want := range map[string]string{"starter": "shared", "correction": "shared", "late": "late"} {
					if op, _ := executions.Turn(sent[name].op); op.Status != agentTurnCompleted || op.FinalItemID != want+"-final:0" {
						t.Fatalf("%s operation before any result = %+v; want completed with its saved %s answer", name, op, want)
					}
				}
				results()
				// What the finished-turn callback does once a Claude result
				// persists: ask for the owed member step.
				sess.RequestMemberStep()
			}

			for name, want := range map[string]string{"starter": "shared", "correction": "shared", "late": "late"} {
				p := sent[name]
				if order == "absorbed turn holds usage" && name == "starter" {
					record, err := core.Get(ctx, "hosted", p.turn)
					if err != nil || record.UsageInputTokens == nil || *record.UsageInputTokens != held {
						t.Fatalf("absorbed turn usage = %+v, %v; want its own %d kept", record, err, held)
					}
					if op, _ := executions.Turn(p.op); op.Status != agentTurnCompleted || op.FinalItemID != "shared-final:0" {
						t.Fatalf("absorbed operation = %+v; want the shared answer", op)
					}
					continue
				}
				record, err := core.Get(ctx, "hosted", p.turn)
				if err != nil || record.Outcome != agentTurnCompleted || record.FinalMessageID != want+"-final:0" {
					t.Fatalf("%s turn = %+v err=%v; want completed with %s-final", name, record, err, want)
				}
				// A shared reply's usage counts once, on the turn its result named;
				// the absorbed turn records an explicit zero, not a missing value.
				usage := map[bool][3]int64{true: {0, 0, 0}, false: {input, 0, output}}[name == "starter"]
				got := []*int64{record.UsageInputTokens, record.UsageCachedInputTokens, record.UsageOutputTokens}
				for i, want := range usage {
					// The named turn records no cached count (NULL), unless the
					// saved finish came first and recorded zero; the absorbed
					// turn records explicit zeros in all three.
					if i == 1 && name != "starter" && order != "saved first" {
						if got[i] != nil {
							t.Fatalf("%s turn cached usage = %d; want unrecorded", name, *got[i])
						}
						continue
					}
					if got[i] == nil || *got[i] != want {
						t.Fatalf("%s turn usage[%d] = %v; want %d", name, i, got[i], want)
					}
				}
				op, _ := executions.Turn(p.op)
				if op.Status != agentTurnCompleted || op.FinalItemID != want+"-final:0" || op.FinalResponse != want+" answer" {
					t.Fatalf("%s operation = status %s final %q %q; want the %s answer", name, op.Status, op.FinalItemID, op.FinalResponse, want)
				}
			}
			wantOwed := map[string]bool{}
			if owing, err := h.prompts.Owing(ctx, "hosted", []string{sent["starter"].transport, sent["correction"].transport, sent["late"].transport}); err != nil || !reflect.DeepEqual(owing, wantOwed) {
				t.Fatalf("owed members = %v, %v; want %v", owing, err, wantOwed)
			}
		})
	}
}

// Two prompts proven in one saved turn that both claim the same absorbed
// durable turn: saved membership is not execution identity, so that shared
// turn is neither finished nor counted, neither prompt's operation completes,
// and both stay owed. The answering prompt, the one claim of its own turn,
// still finishes. Boundary: the saved-commit settle pass and its member step,
// on a real catalog of Claude's format.
func TestClaudeMembersClaimingOneDurableTurnLeaveItUnfinished(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := h.fixture.native
	executions := newTestAgentExecutionStore(t, "run-1")
	if _, err := fixtureEnsureSessionWithIdentity(executions, "hosted", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession("hosted", "nano-session", native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	completed := hostedmodel.NewCompletedTurnStore(h.queue.Database())
	core, err := completed.CoreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type prompt struct{ op, turn, transport string }
	sent := map[string]*prompt{"answerer": {turn: "claude:shim:401"}, "twin-a": {turn: "claude:shim:400"}, "twin-b": {turn: "claude:shim:400"}}
	for _, name := range []string{"answerer", "twin-a", "twin-b"} {
		op, _, err := fixtureBeginTurn(executions, "hosted", name, "hash-"+name)
		if err != nil {
			t.Fatal(err)
		}
		p := sent[name]
		p.op, p.transport = op.ID, op.TransportActionID
		text := name + " prompt"
		if _, _, err := h.prompts.Submit(ctx, prompts.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: p.transport,
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: prompts.HumanOrigin}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := h.prompts.Claim(ctx, prompts.ClaimRequest{HostedThreadID: "hosted", Token: "claim-" + name, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", name, claimed, err)
		}
		binding := prompts.Binding{EventID: submissionEventID("hosted", p.transport), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &p.turn}}
		if _, err := h.prompts.Accepted(ctx, prompts.Acceptance{ActionID: p.transport, ClaimToken: "claim-" + name, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if err := executions.MarkTurnDelivery(op.ID, p.turn, agentTurnRunning, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, turn := range []string{"claude:shim:400", "claude:shim:401"} {
		if _, _, err := core.Started(ctx, coreturns.Start{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: turn, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
	}
	input, output := int64(10), int64(5)
	if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: "claude:shim:401",
		FinalMessageID: "twin-final", Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter", Usage: coreturns.Usage{InputTokens: &input, OutputTokens: &output}}); err != nil {
		t.Fatal(err)
	}
	if err := executions.ObserveCandidate("claude", native, "claude:shim:401", "twin-final", "final_answer", "twin answer"); err != nil {
		t.Fatal(err)
	}
	event := func(name string) string { return submissionEventID("hosted", sent[name].transport) }
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"answerer prompt"}}
{"type":"attachment","uuid":"twin-a-queued","sessionId":"claude-settle","attachment":{"type":"queued_command","commandMode":"prompt","prompt":[{"type":"text","text":"twin-a prompt"}],"source_uuid":%q}}
{"type":"attachment","uuid":"twin-b-queued","sessionId":"claude-settle","attachment":{"type":"queued_command","commandMode":"prompt","prompt":[{"type":"text","text":"twin-b prompt"}],"source_uuid":%q}}
{"type":"assistant","uuid":"twin-shared","sessionId":"claude-settle","message":{"id":"twin-final","role":"assistant","content":[{"type":"text","text":"twin answer"}],"stop_reason":"end_turn"}}
`, event("answerer"), event("twin-a"), event("twin-b")))
	sess := h.attach("claude", nil, func(cfg *sessionConfig) { cfg.AgentExecutions = executions; cfg.CompletedTurns = completed.CoreStore })
	waitForCatalogWithin(t, time.Minute, func() bool {
		if _, err := sess.SettlePrompts(ctx); err != nil {
			return false
		}
		op, _ := executions.Turn(sent["answerer"].op)
		return threads.TurnTerminal(op.Status)
	})
	if shared, err := core.Get(ctx, "hosted", "claude:shim:400"); err != nil || shared.Outcome != "" || shared.UsageInputTokens != nil {
		t.Fatalf("the twice-claimed turn = %+v, %v; want it unfinished and uncounted", shared, err)
	}
	for _, name := range []string{"twin-a", "twin-b"} {
		if op, _ := executions.Turn(sent[name].op); threads.TurnTerminal(op.Status) {
			t.Fatalf("%s operation completed on a shared turn: %+v", name, op)
		}
	}
	owing, err := h.prompts.Owing(ctx, "hosted", []string{sent["answerer"].transport, sent["twin-a"].transport, sent["twin-b"].transport})
	if err != nil || !reflect.DeepEqual(owing, map[string]bool{sent["twin-a"].transport: true, sent["twin-b"].transport: true}) {
		t.Fatalf("owed = %v, %v; want only the twins", owing, err)
	}
}

// Failure caught: two prompts saved in one turn whose executions each held a
// live result ("two answering executions") stayed owed for good, and every
// later saved commit read them again and logged it (4,089 lines from four
// turns in one log; 128 Claude prompts owed on 2026-10-21). The saved turn
// alone decides: both finish with its final message, nothing more is logged,
// and nothing stays owed. Boundary: the saved-commit settle pass and its
// member step on a real catalog of Claude's format.
func TestClaudePromptsWithTheirOwnLiveResultsInOneSavedTurnBothFinish(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := h.fixture.native
	executions := newTestAgentExecutionStore(t, "run-1")
	if _, err := fixtureEnsureSessionWithIdentity(executions, "hosted", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession("hosted", "nano-session", native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	completed := hostedmodel.NewCompletedTurnStore(h.queue.Database())
	core, err := completed.CoreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	transports := map[string]string{}
	turnsByName := map[string]string{"first": "claude:shim:500", "second": "claude:shim:501"}
	for _, name := range []string{"first", "second"} {
		op, _, err := fixtureBeginTurn(executions, "hosted", name, "hash-"+name)
		if err != nil {
			t.Fatal(err)
		}
		transports[name] = op.TransportActionID
		turn, text := turnsByName[name], name+" prompt"
		if _, _, err := h.prompts.Submit(ctx, prompts.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: op.TransportActionID,
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: prompts.HumanOrigin}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := h.prompts.Claim(ctx, prompts.ClaimRequest{HostedThreadID: "hosted", Token: "claim-" + name, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", name, claimed, err)
		}
		binding := prompts.Binding{EventID: submissionEventID("hosted", op.TransportActionID), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}
		if _, err := h.prompts.Accepted(ctx, prompts.Acceptance{ActionID: op.TransportActionID, ClaimToken: "claim-" + name, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if err := executions.MarkTurnDelivery(op.ID, turn, agentTurnRunning, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.Started(ctx, coreturns.Start{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: turn, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
		// Each member turn holds its own provider result: two answering executions.
		if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: turn,
			FinalMessageID: "shared-final", Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := executions.ObserveCandidate("claude", native, turnsByName["first"], "shared-final", "final_answer", "one answer"); err != nil {
		t.Fatal(err)
	}
	event := func(name string) string { return submissionEventID("hosted", transports[name]) }
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"first prompt"}}
{"type":"attachment","uuid":"second-queued","sessionId":"claude-settle","attachment":{"type":"queued_command","commandMode":"prompt","prompt":[{"type":"text","text":"second prompt"}],"source_uuid":%q}}
{"type":"assistant","uuid":"shared","sessionId":"claude-settle","message":{"id":"shared-final","role":"assistant","content":[{"type":"text","text":"one answer"}],"stop_reason":"end_turn"}}
`, event("first"), event("second")))
	var logs syncBuffer
	priorLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(priorLog) })
	sess := h.attach("claude", nil, func(cfg *sessionConfig) { cfg.AgentExecutions = executions; cfg.CompletedTurns = completed.CoreStore })
	// Owing is empty before saved proof confirms anything, so the wait needs
	// positive evidence: both operations finished.
	waitForCatalogWithin(t, time.Minute, func() bool {
		if _, err := sess.SettlePrompts(ctx); err != nil {
			return false
		}
		for _, transport := range transports {
			if op, ok, err := executions.PersistedTurnForTransportAction(ctx, "hosted", transport); err != nil || !ok || op.Status != agentTurnCompleted {
				return false
			}
		}
		return true
	})
	if owing, err := h.prompts.Owing(ctx, "hosted", []string{transports["first"], transports["second"]}); err != nil || len(owing) != 0 {
		t.Fatalf("owed = %v, %v; want none", owing, err)
	}
	for range 3 { // later saved commits on the thread
		if _, err := sess.SettlePrompts(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for name, transport := range transports {
		persisted, ok, err := executions.PersistedTurnForTransportAction(ctx, "hosted", transport)
		if err != nil || !ok {
			t.Fatalf("%s operation unreadable: %v", name, err)
		}
		if op, _ := executions.Turn(persisted.ID); op.Status != agentTurnCompleted || op.FinalItemID != "shared-final:0" || op.FinalResponse != "one answer" {
			t.Fatalf("%s operation = %+v; want the saved turn's answer", name, op)
		}
	}
	if strings.Contains(logs.String(), "Claude") {
		t.Fatalf("the settled turn was logged:\n%s", logs.String())
	}
}

// Failure caught: a reboot killed Claude mid-turn (2026-10-22), and four
// prompts read "accepted" for good. The cut-off turn never ends in the saved
// file, so the three prompts it took never settled and their operations stayed
// running. The fourth was still queued when the process died, so no saved row
// carries it; Nano refused both Resend and Discard. testdata/claude-process-death
// is a scrubbed copy of that file from the cut-off turn's prompt to the first
// turn after the restart, which the person stopped. A lost prompt accepted
// under a fresh session's shim handle stayed accepted too, until its session
// was read through the handle's recorded resolution (found in review).
// Boundary: the settlement pass and owed pass over a real saved file and the
// prompt and operation stores.
func TestClaudePromptsAProcessDeathCutOffSettleAndTheOneNeverTakenWaitsForThePerson(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := "claude-process-death"
	executions := newTestAgentExecutionStore(t, "run-1")
	if _, err := fixtureEnsureSessionWithIdentity(executions, "hosted", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession("hosted", "nano-session", native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	completed := hostedmodel.NewCompletedTurnStore(h.queue.Database())
	core, err := completed.CoreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The shim's spool position when Nano handed each prompt over, as stored.
	// neverTakenUnderHandle stands for a prompt a fresh session accepts under
	// its shim handle before Claude names the session; the handle's recorded
	// resolution names the same session as the others.
	names := []string{"opener", "absorbed", "absorbedLater", "neverTaken", "neverTakenUnderHandle", "afterRestart"}
	seqs := map[string]int{"opener": 44236, "absorbed": 44469, "absorbedLater": 44923, "neverTaken": 45015, "neverTakenUnderHandle": 45017, "afterRestart": 45020}
	handle := "cs_process-death-handle"
	if err := h.control.ClaimNativeID("hosted", "claude", handle, true); err != nil {
		t.Fatal(err)
	}
	if err := h.control.ClaimNativeIDResolving("hosted", "claude", native, false, &nativeResolution{Handle: handle, Native: native}); err != nil {
		t.Fatal(err)
	}
	transports := map[string]string{}
	for _, name := range names {
		native := native
		if name == "neverTakenUnderHandle" {
			native = handle
		}
		op, _, err := fixtureBeginTurn(executions, "hosted", name, "hash-"+name)
		if err != nil {
			t.Fatal(err)
		}
		transports[name] = op.TransportActionID
		turn, text := fmt.Sprintf("claude:hosted:%d", seqs[name]), name+" prompt"
		if _, _, err := h.prompts.Submit(ctx, prompts.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: op.TransportActionID,
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: prompts.AgentOrigin}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := h.prompts.Claim(ctx, prompts.ClaimRequest{HostedThreadID: "hosted", Token: "claim-" + name, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", name, claimed, err)
		}
		binding := prompts.Binding{EventID: submissionEventID("hosted", op.TransportActionID), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}
		if _, err := h.prompts.Accepted(ctx, prompts.Acceptance{ActionID: op.TransportActionID, ClaimToken: "claim-" + name, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if err := executions.MarkTurnDelivery(op.ID, turn, agentTurnRunning, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.Started(ctx, coreturns.Start{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: turn, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile("testdata/claude-process-death/saved.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.NewReplacer("{{NATIVE}}", native,
		"{{OPENER}}", submissionEventID("hosted", transports["opener"]),
		"{{ABSORBED}}", submissionEventID("hosted", transports["absorbed"]),
		"{{ABSORBED_LATER}}", submissionEventID("hosted", transports["absorbedLater"]),
		"{{AFTER_RESTART}}", submissionEventID("hosted", transports["afterRestart"])).Replace(string(raw))
	if strings.Contains(rows, "{{") {
		t.Fatal("fixture placeholder left unreplaced")
	}
	path := filepath.Join(filepath.Dir(h.fixture.path), native+".jsonl")
	if err := os.WriteFile(path, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	h.service.RequestRefresh()
	waitForCatalogWithin(t, time.Minute, func() bool {
		id, err := h.service.ResolveThreadReference(ctx, native)
		if err != nil {
			return false
		}
		page, err := h.service.LoadTranscript(ctx, id, threadindex.TranscriptQuery{Limit: 100, HiddenSpecified: true, View: threadindex.DefaultThreadTranscriptView})
		return err == nil && len(page.Turns) == 2
	})
	sess := h.attach("claude", nil, func(cfg *sessionConfig) { cfg.AgentExecutions = executions; cfg.CompletedTurns = completed.CoreStore })
	outcomes := func() string {
		var got []string
		for _, name := range names {
			op := ""
			if persisted, ok, err := executions.PersistedTurnForTransportAction(ctx, "hosted", transports[name]); err == nil && ok {
				turn, _ := executions.Turn(persisted.ID)
				op = turn.Status
			}
			got = append(got, fmt.Sprintf("%s: prompt %s, operation %s", name, h.state(t, transports[name]), op))
		}
		return strings.Join(got, "; ")
	}
	want := "opener: prompt confirmed, operation interrupted; absorbed: prompt confirmed, operation interrupted; " +
		"absorbedLater: prompt confirmed, operation interrupted; neverTaken: prompt uncertain, operation running; " +
		"neverTakenUnderHandle: prompt uncertain, operation running; afterRestart: prompt confirmed, operation interrupted"
	for deadline := time.Now().Add(time.Minute); outcomes() != want; time.Sleep(50 * time.Millisecond) {
		if _, err := sess.SettlePrompts(ctx); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("outcomes:\n%s\nwant:\n%s", outcomes(), want)
		}
	}
	never, _, err := h.prompts.Get(ctx, transports["neverTaken"])
	if err != nil {
		t.Fatal(err)
	}
	actions, _ := promptPresentation("claude", never.Outbox)
	for _, action := range []string{promptResend, promptDiscard} {
		if !slices.Contains(actions, action) {
			t.Fatalf("%s of the prompt Claude never took refused: actions=%v", action, actions)
		}
	}
	if never.Outbox.Reason != prompts.ReasonTurnEnded {
		t.Fatalf("never-taken reason = %q; want %q", never.Outbox.Reason, prompts.ReasonTurnEnded)
	}
}

// Failure caught: an owed prompt on a thread no session holds was never
// revisited. The member step ran only in an attached session, so an exited
// thread's prompts stayed owed and their operations open for good (two of the
// six threads named for repair on 2026-10-21 had exited). One catalog commit
// of the thread's file finishes them by the same rule, with no provider: the
// operation completes then, not in a later saved-result read no pass would
// revisit, and the parent subscribed to the thread is told, though no
// finished-turn hook ran. Boundary: one catalog commit on a server with no
// session, the production saved-result reader installed, on a real catalog
// of Claude's format.
func TestClaudeOwedPromptOnAThreadWithNoSessionFinishesOnASavedCommit(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := h.fixture.native
	executions := newTestAgentExecutionStore(t, "run-1")
	parent, _, err := fixtureBeginSession(executions, "parent", "parent-hash", "claude", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := fixtureBeginSessionWithSubscriptions(executions, "child", "child-hash", "claude", presentationBackground, parent, subscriptions.Choices{Inbox: subscriptions.Send})
	if err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession(child.ID, "nano-session", native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	if err := executions.Control().ClaimNativeID(child.ID, "claude", native, false); err != nil {
		t.Fatal(err)
	}
	completed := hostedmodel.NewCompletedTurnStore(h.queue.Database())
	h.server.turns, h.server.agentExecutions, h.server.completedTurns, h.server.controlMetadata = h.prompts, executions, hostedmodel.NewCompletedTurnSource(completed), executions.Control()
	fixtureOwners(h.server).attachmentCoordinator().Turns = h.server.turns
	fixtureOwners(h.server).attachmentCoordinator().Executions = h.server.agentExecutions
	fixtureOwners(h.server).attachmentCoordinator().CompletedTurns = h.server.completedTurns
	fixtureOwners(h.server).attachmentCoordinator().Metadata = h.server.controlMetadata
	executions.UseSavedResults(fixtureOwners(h.server).attachmentCoordinator().SavedOperationResult, fixtureOwners(h.server).attachmentCoordinator().SavedOperationResultFinished)
	client := &feedSubscriptionClient{s: h.server}
	dispatcher := subscriptions.NewDispatcher(executions.Control().CanonicalDatabase(), client, nil, nil)
	fixtureOwners(h.server).agentController().StartSubscriptions(dispatcher)
	op, _, err := fixtureBeginTurn(executions, child.ID, "exited", "hash-exited")
	if err != nil {
		t.Fatal(err)
	}
	turn, text := "claude:shim:600", "exited prompt"
	if _, _, err := h.prompts.Submit(ctx, prompts.Submission{HostedThreadID: child.ID, Provider: model.ProviderClaude, NativeThreadID: native, ActionID: op.TransportActionID,
		ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: prompts.AgentOrigin}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := h.prompts.Claim(ctx, prompts.ClaimRequest{HostedThreadID: child.ID, Token: "claim-exited", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	binding := prompts.Binding{EventID: submissionEventID(child.ID, op.TransportActionID), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}
	if _, err := h.prompts.Accepted(ctx, prompts.Acceptance{ActionID: op.TransportActionID, ClaimToken: "claim-exited", AttachmentEpoch: 1, Binding: binding}); err != nil {
		t.Fatal(err)
	}
	if err := executions.MarkTurnDelivery(op.ID, turn, agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	// Confirmed and owed before the thread's session ended.
	if _, settled, err := h.prompts.Settle(ctx, op.TransportActionID); err != nil || !settled {
		t.Fatalf("settle: %v %v", settled, err)
	}
	dispatcher.Sweep("") // the parent's cursor, as a running Nano holds it
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"exited prompt"}}
{"type":"assistant","uuid":"exited-answer","sessionId":"claude-settle","message":{"id":"exited-final","role":"assistant","content":[{"type":"text","text":"answered after all"}],"stop_reason":"end_turn"}}
`, submissionEventID(child.ID, op.TransportActionID)))
	waitForCatalogWithin(t, time.Minute, func() bool {
		rows, err := h.server.hostedCatalog().SavedPromptRows(ctx, native, history.SavedUserKeys{NativeEventIDs: []string{submissionEventID(child.ID, op.TransportActionID)}})
		return err == nil && len(rows) == 1
	})
	threadID, err := h.service.ResolveThreadReference(ctx, native)
	if err != nil {
		t.Fatal(err)
	}
	fixtureOwners(h.server).attachmentCoordinator().ConfirmDurableThread(ctx, threadindex.TranscriptChange{ThreadID: threadID, Provider: "claude", NativeThreadID: native})
	if finished, _ := executions.Turn(op.ID); finished.Status != agentTurnCompleted || finished.FinalItemID != "exited-final:0" || finished.FinalResponse != "answered after all" {
		t.Fatalf("operation after the commit = %+v; want its saved answer", finished)
	}
	if owing, err := h.prompts.Owing(ctx, child.ID, []string{op.TransportActionID}); err != nil || len(owing) != 0 {
		t.Fatalf("owed after the commit = %v, %v; want none", owing, err)
	}
	waitForCatalogWithin(t, time.Minute, func() bool {
		_, messages := client.counts()
		return messages == 1
	})
}

// Failure caught: the completion feed published a Claude execution's stored
// live final, which before 2026-10-19 could be another prompt's answer, and
// the observer could not find any Claude answer by the execution's turn id.
// The feed publishes the rule's answer and the saved turn that holds it,
// never the stored final in its place: it holds its cursor before a completed
// row whose prompt Claude has not saved yet, and publishes it once saved; an
// execution two prompts claim publishes no answer.
// Boundary: the public completed-turn feed on a real catalog of Claude's
// format.
func TestCompletedTurnFeedPublishesTheClaudeAnswerItsSavedTurnHolds(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := h.fixture.native
	completed := hostedmodel.NewCompletedTurnStore(h.queue.Database())
	core, err := completed.CoreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.server.turns, h.server.completedTurns, h.server.controlMetadata = h.prompts, hostedmodel.NewCompletedTurnSource(completed), h.control
	h.server.agentExecutions = newTestAgentExecutionStore(t, "run")
	fixtureOwners(h.server).attachmentCoordinator().Turns = h.server.turns
	fixtureOwners(h.server).attachmentCoordinator().Executions = h.server.agentExecutions
	fixtureOwners(h.server).attachmentCoordinator().CompletedTurns = h.server.completedTurns
	fixtureOwners(h.server).attachmentCoordinator().Metadata = h.server.controlMetadata
	turnOf := map[string]string{"answered": "claude:shim:701", "twin-a": "claude:shim:703", "twin-b": "claude:shim:703", "unsaved": "claude:shim:702"}
	for _, action := range []string{"answered", "twin-a", "twin-b", "unsaved"} {
		turn := turnOf[action]
		if _, _, err := h.prompts.Submit(ctx, prompts.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: action,
			ProviderText: action, PresentationText: action, TextHash: sendTextHash(action), RequestHash: sendTextHash(action), Origin: prompts.AgentOrigin}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := h.prompts.Claim(ctx, prompts.ClaimRequest{HostedThreadID: "hosted", Token: "claim-" + action, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", action, claimed, err)
		}
		binding := prompts.Binding{EventID: submissionEventID("hosted", action), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}
		if _, err := h.prompts.Accepted(ctx, prompts.Acceptance{ActionID: action, ClaimToken: "claim-" + action, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if action == "twin-b" {
			continue // its twin's execution is its own too
		}
		// The live result as a wrong binding left it for the first.
		final := map[string]string{"answered": "earlier-answer:0", "twin-a": "retained:0", "unsaved": "unsaved-final:0"}[action]
		if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: turn, ActionID: action,
			FinalMessageID: final, Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
	}
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"answered"}}
{"type":"assistant","uuid":"answered-final","sessionId":"claude-settle","message":{"id":"right-final","role":"assistant","content":[{"type":"text","text":"the right answer"}],"stop_reason":"end_turn"}}
`, submissionEventID("hosted", "answered")))
	feed := func(cursor string) interact.CompletedTurnPage {
		t.Helper()
		rec := httptest.NewRecorder()
		fixtureOwners(h.server).attachmentCoordinator().CompletedFeed().HandleCompletedTurns(rec, httptest.NewRequest(http.MethodGet, "/completed-turns?after="+cursor, nil), "hosted")
		var page interact.CompletedTurnPage
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &page) != nil {
			t.Fatalf("feed = %d %s", rec.Code, rec.Body.String())
		}
		return page
	}
	var page interact.CompletedTurnPage
	waitForCatalogWithin(t, time.Minute, func() bool {
		page = feed("")
		return len(page.Items) == 2
	})
	rows, err := h.server.hostedCatalog().SavedPromptRows(ctx, native, history.SavedUserKeys{NativeEventIDs: []string{submissionEventID("hosted", "answered")}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("saved rows = %+v, %v", rows, err)
	}
	if got := page.Items[0]; got.TurnID != turnOf["answered"] || got.FinalMessageID != "right-final:0" || got.SavedTurnID != rows[0].Item.TurnKey || got.NativeThreadID != native {
		t.Fatalf("feed = %+v; want the saved turn's right-final:0 in turn %s", page, rows[0].Item.TurnKey)
	}
	if got := page.Items[1]; got.TurnID != turnOf["twin-a"] || got.FinalMessageID != "" || page.NextCursor != turnOf["twin-a"] {
		t.Fatalf("feed = %+v; want the twice-claimed execution with no answer, the cursor held at %s", page, turnOf["twin-a"])
	}
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"unsaved"}}
{"type":"assistant","uuid":"unsaved-answer","sessionId":"claude-settle","message":{"id":"unsaved-final","role":"assistant","content":[{"type":"text","text":"saved now"}],"stop_reason":"end_turn"}}
`, submissionEventID("hosted", "unsaved")))
	waitForCatalogWithin(t, time.Minute, func() bool {
		page = feed(turnOf["twin-a"])
		return len(page.Items) == 1
	})
	if got := page.Items[0]; got.TurnID != turnOf["unsaved"] || got.FinalMessageID != "unsaved-final:0" {
		t.Fatalf("feed after the save = %+v", page)
	}
}

// Failure caught (design audit): three operations that failed with another
// prompt's live answer (aop_f697b23a, aop_868a30b2, aop_78827850) resolve,
// on a projection the older reader built, to a later turn's answer. Their
// late read runs at startup, before the upgrade re-reads the file, and once it
// no longer required the live final it would complete them with that wrong
// answer for good. The read waits for the current reader's projection, then
// takes the answer of the saved turn that consumed the prompt, whatever final
// the live result named. A reader upgrade can also make the older projection
// the current reader's without a re-read, by proving its file unaffected; that
// proof must wake the waiting read as a re-read's commit does, or the
// operation stays failed until its file next changes. Boundary: the operation
// store's startup and commit-driven late reads through the server's saved
// answer, on a real catalog of Claude's format, with no session.
func TestClaudeLateAnswerWaitsForTheCurrentReaderThenTakesItsSavedTurns(t *testing.T) {
	for _, upgrade := range []string{"re-read", "proved unaffected"} {
		t.Run(upgrade, func(t *testing.T) { testClaudeLateAnswerAfterReaderUpgrade(t, upgrade) })
	}
}

func testClaudeLateAnswerAfterReaderUpgrade(t *testing.T, upgrade string) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := h.fixture.native
	first := newTestAgentExecutionStore(t, "run-1")
	if _, err := fixtureEnsureSessionWithIdentity(first, "hosted", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := first.BindSession("hosted", "nano-session", native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	op, _, err := fixtureBeginTurn(first, "hosted", "late", "hash-late")
	if err != nil {
		t.Fatal(err)
	}
	turn := "claude:shim:800"
	if _, _, err := h.prompts.Submit(ctx, prompts.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: op.TransportActionID,
		ProviderText: "late prompt", PresentationText: "late prompt", TextHash: sendTextHash("late prompt"), RequestHash: sendTextHash("late prompt"), Origin: prompts.AgentOrigin}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := h.prompts.Claim(ctx, prompts.ClaimRequest{HostedThreadID: "hosted", Token: "claim-late", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	binding := prompts.Binding{EventID: submissionEventID("hosted", op.TransportActionID), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}
	if _, err := h.prompts.Accepted(ctx, prompts.Acceptance{ActionID: op.TransportActionID, ClaimToken: "claim-late", AttachmentEpoch: 1, Binding: binding}); err != nil {
		t.Fatal(err)
	}
	// Failed after its wait, holding the live final a wrong binding gave it.
	rec, _ := first.Turn(op.ID)
	rec.Status, rec.ProviderTurnID, rec.NativeThreadID, rec.FinalItemID = agentTurnFailed, turn, native, "earlier-answer:0"
	rec.Error = &agentExecutionError{Source: "saved", Code: "result_not_saved", Message: "not saved"}
	rec.Revision++
	err = fixtureWriteTurn(first, rec)
	if err != nil {
		t.Fatal(err)
	}
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"late prompt"}}
{"type":"assistant","uuid":"late-answer","sessionId":"claude-settle","message":{"id":"right-final","role":"assistant","content":[{"type":"text","text":"the right answer"}],"stop_reason":"end_turn"}}
`, submissionEventID("hosted", op.TransportActionID)))
	threadID, err := h.service.ResolveThreadReference(ctx, native)
	if err != nil {
		t.Fatal(err)
	}
	waitForCatalogWithin(t, time.Minute, func() bool {
		rows, err := h.server.hostedCatalog().SavedPromptRows(ctx, native, history.SavedUserKeys{NativeEventIDs: []string{submissionEventID("hosted", op.TransportActionID)}})
		return err == nil && len(rows) == 1
	})
	db, err := sql.Open("sqlite", h.historyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reader := func(version string) {
		t.Helper()
		if _, err := db.Exec(`UPDATE conversation_projections SET parser_version=? WHERE thread_id=? AND state='active'`, version, threadID); err != nil {
			t.Fatal(err)
		}
	}
	current := history.CurrentTranscriptParserVersion(history.ProviderClaude)
	if upgrade == "re-read" {
		reader("claude-reader-v34")
	} else {
		// One reader behind, under an upgrade whose marker this file lacks.
		reader(strings.Replace(current, "claude-reader-v36", "claude-reader-v35", 1))
	}

	// Nano starts: the operation store reloads and reads not-saved answers.
	h.server.turns, h.server.controlMetadata = h.prompts, h.control
	store, err := newControlAgentExecutionStore(first.Control(), "run-2")
	if err != nil {
		t.Fatal(err)
	}
	h.server.agentExecutions = store
	coordinator := fixtureOwners(h.server).attachmentCoordinator()
	coordinator.Turns, coordinator.Metadata, coordinator.Executions = h.prompts, h.control, store
	if upgrade != "re-read" {
		h.service.SetTranscriptChanged(coordinator.ConfirmDurableThread)
	}
	var reads atomic.Int32
	store.UseSavedResults(func(ctx context.Context, rec threads.TurnRecord) (string, string, error) {
		defer reads.Add(1)
		return fixtureOwners(h.server).attachmentCoordinator().SavedOperationResult(ctx, rec)
	}, nil)
	waitForCatalogWithin(t, time.Minute, func() bool { return reads.Load() >= 1 })
	if got, _ := store.Turn(op.ID); got.Status != agentTurnFailed {
		t.Fatalf("operation on the older reader's projection = %+v; want it still waiting", got)
	}
	if upgrade == "re-read" {
		reader(current)
		waitForCatalogWithin(t, time.Minute, func() bool {
			fixtureOwners(h.server).attachmentCoordinator().ConfirmDurableThread(ctx, threadindex.TranscriptChange{ThreadID: threadID, Provider: "claude", NativeThreadID: native})
			got, _ := store.Turn(op.ID)
			return got.Status == agentTurnCompleted
		})
	} else {
		// The read has finished and parked, and no earlier commit's
		// notification is still due, so only the proof can wake it.
		waitForCatalogWithin(t, time.Minute, func() bool {
			status, err := h.service.IndexStatus(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			backlog, inFlight := status.TranscriptChangeBacklog, status.TranscriptChangeInFlight
			return backlog == 0 && inFlight == 0 && reads.Load() >= 1
		})
		if got, _ := store.Turn(op.ID); got.Status != agentTurnFailed {
			t.Fatalf("operation before the proof = %+v; want it still waiting", got)
		}
		// Restart the real background backfill at the proof boundary. Its
		// queued commit notification waits for the callback to be installed.
		h.service.Close()
		reopened, err := history.Open(h.historyPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { reopened.Close() })
		service, err := newThreadCatalogService(ctx, reopened, threadindex.Options{
			Roots: []history.SourceRoot{h.fixture.root}, EnableUsageBackfillScheduler: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(service.Close)
		h.service, h.server.catalog = service, service
		coordinator.Catalog = h.server.hostedCatalog()
		service.SetTranscriptChanged(coordinator.ConfirmDurableThread)
		waitForCatalogWithin(t, time.Minute, func() bool {
			got, _ := store.Turn(op.ID)
			return got.Status == agentTurnCompleted
		})
	}
	if got, _ := store.Turn(op.ID); got.FinalItemID != "right-final:0" || got.FinalResponse != "the right answer" {
		t.Fatalf("operation = %+v; want the answer of the saved turn that consumed its prompt", got)
	}
}

// An agent prompt answered with another's result whose own operation row
// retention has since removed: a missing row is not proof it never had one,
// so its durable turn may finish but its owed completion stays, while the
// answering prompt, whose row is retained and exact, clears. Boundary: the
// saved-commit settle pass and its member step on a real catalog of Claude's
// format, with the operation owner's committed rows.
func TestClaudeAgentMemberWithoutItsRetainedOperationStaysOwed(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := h.fixture.native
	executions := newTestAgentExecutionStore(t, "run-1")
	if _, err := fixtureEnsureSessionWithIdentity(executions, "hosted", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession("hosted", "nano-session", native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	completed := hostedmodel.NewCompletedTurnStore(h.queue.Database())
	core, err := completed.CoreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type prompt struct{ op, turn, transport string }
	sent := map[string]*prompt{"answerer": {turn: "claude:shim:501"}, "absorbed": {turn: "claude:shim:500"}}
	for _, name := range []string{"answerer", "absorbed"} {
		op, _, err := fixtureBeginTurn(executions, "hosted", name, "hash-"+name)
		if err != nil {
			t.Fatal(err)
		}
		p := sent[name]
		p.op, p.transport = op.ID, op.TransportActionID
		text := name + " prompt"
		if _, _, err := h.prompts.Submit(ctx, prompts.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: p.transport,
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: prompts.AgentOrigin}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := h.prompts.Claim(ctx, prompts.ClaimRequest{HostedThreadID: "hosted", Token: "claim-" + name, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", name, claimed, err)
		}
		binding := prompts.Binding{EventID: submissionEventID("hosted", p.transport), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &p.turn}}
		if _, err := h.prompts.Accepted(ctx, prompts.Acceptance{ActionID: p.transport, ClaimToken: "claim-" + name, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if err := executions.MarkTurnDelivery(op.ID, p.turn, agentTurnRunning, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.Started(ctx, coreturns.Start{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: p.turn, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
	}
	input, output := int64(10), int64(5)
	if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: sent["answerer"].turn,
		FinalMessageID: "kept-final", Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter", Usage: coreturns.Usage{InputTokens: &input, OutputTokens: &output}}); err != nil {
		t.Fatal(err)
	}
	if err := executions.ObserveCandidate("claude", native, sent["answerer"].turn, "kept-final", "final_answer", "kept answer"); err != nil {
		t.Fatal(err)
	}
	// Retention removes the absorbed prompt's operation from the committed
	// rows and, on its reload, from the cache.
	if _, err := executions.Control().CanonicalDatabase().Exec(`DELETE FROM hosted_turn_operations WHERE operation_id=?`, sent["absorbed"].op); err != nil {
		t.Fatal(err)
	}
	executions, reloadErr := newControlAgentExecutionStore(executions.Control(), "retained-run")
	if reloadErr != nil {
		t.Fatal(reloadErr)
	}
	event := func(name string) string { return submissionEventID("hosted", sent[name].transport) }
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"answerer prompt"}}
{"type":"attachment","uuid":"absorbed-queued","sessionId":"claude-settle","attachment":{"type":"queued_command","commandMode":"prompt","prompt":[{"type":"text","text":"absorbed prompt"}],"source_uuid":%q}}
{"type":"assistant","uuid":"kept-shared","sessionId":"claude-settle","message":{"id":"kept-final","role":"assistant","content":[{"type":"text","text":"kept answer"}],"stop_reason":"end_turn"}}
`, event("answerer"), event("absorbed")))
	sess := h.attach("claude", nil, func(cfg *sessionConfig) { cfg.AgentExecutions = executions; cfg.CompletedTurns = completed.CoreStore })
	// Owing is empty before saved proof confirms anything, so the wait needs
	// positive evidence: both prompts confirmed and the absorbed turn finished.
	waitForCatalogWithin(t, time.Minute, func() bool {
		if _, err := sess.SettlePrompts(ctx); err != nil {
			return false
		}
		if h.state(t, sent["answerer"].transport) != prompts.Confirmed || h.state(t, sent["absorbed"].transport) != prompts.Confirmed {
			return false
		}
		absorbed, err := core.Get(ctx, "hosted", sent["absorbed"].turn)
		return err == nil && absorbed.Outcome != ""
	})
	if owing, err := h.prompts.Owing(ctx, "hosted", []string{sent["answerer"].transport}); err != nil || len(owing) != 0 {
		t.Fatalf("the answering prompt with its retained exact operation is still owed: %v, %v", owing, err)
	}
	if absorbed, err := core.Get(ctx, "hosted", sent["absorbed"].turn); err != nil || absorbed.Outcome != agentTurnCompleted || absorbed.FinalMessageID != "kept-final:0" || !explicitZeroUsage(absorbed) {
		t.Fatalf("absorbed turn = %+v, %v; want the saved turn's kept-final:0 with explicit zeros", absorbed, err)
	}
	if owing, err := h.prompts.Owing(ctx, "hosted", []string{sent["absorbed"].transport}); err != nil || !owing[sent["absorbed"].transport] {
		t.Fatalf("an agent member without its retained operation cleared: owing=%v, %v", owing, err)
	}
}

// Two people's prompts answered together, with no agent operation: the saved
// commit confirmed both, owing their member completion, and Nano stopped
// before its member step ran. The new session learns from the owed prompts it
// reads at attach that a later saved commit must still run the member step,
// and that commit finishes the absorbed prompt's own turn with the shared
// answer and explicit zero usage, read from the saved turn's own final row.
// Boundary: attach and the saved-commit settle pass on a real catalog of
// Claude's format.
func TestClaudeOwedPersonPromptFinishesAfterARestartOnALaterSavedCommit(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := h.fixture.native
	completed := hostedmodel.NewCompletedTurnStore(h.queue.Database())
	core, err := completed.CoreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	attach := func() *session {
		sess := newFixtureSession(sessionConfig{ID: "nano-session", Provider: "claude", HostedThreadID: "hosted", Projection: newLiveProjection("claude", "hosted"), Turns: h.prompts, QueuedTurns: h.queue, SendAttempts: h.attempts, Metadata: h.control, AgentExecutions: newTestAgentExecutionStore(t, "run-1"), CompletedTurns: completed.CoreStore}, native)
		if err := sess.InitializeBinding(1); err != nil {
			t.Fatal(err)
		}
		sess.ConfigureSavedReaders(nil, h.server.savedPromptsFor(sess), h.server.hostedCatalog().SavedPromptRows, h.server.hostedCatalog().SavedTurnFinal, nil)
		return sess
	}
	turnOf := map[string]string{"person-starter": "claude:shim:300", "person-correction": "claude:shim:301"}
	for _, action := range []string{"person-starter", "person-correction"} {
		turn, text := turnOf[action], action+" prompt"
		if _, _, err := h.prompts.Submit(ctx, prompts.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: action,
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: prompts.HumanOrigin}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := h.prompts.Claim(ctx, prompts.ClaimRequest{HostedThreadID: "hosted", Token: "claim-" + action, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", action, claimed, err)
		}
		binding := prompts.Binding{EventID: submissionEventID("hosted", action), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}
		if _, err := h.prompts.Accepted(ctx, prompts.Acceptance{ActionID: action, ClaimToken: "claim-" + action, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.Started(ctx, coreturns.Start{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: turn, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
	}
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"person-starter prompt"}}
{"type":"attachment","uuid":"person-queued","sessionId":"claude-settle","attachment":{"type":"queued_command","commandMode":"prompt","prompt":[{"type":"text","text":"person-correction prompt"}],"source_uuid":%q}}
{"type":"assistant","uuid":"person-shared","sessionId":"claude-settle","message":{"id":"person-final","role":"assistant","content":[{"type":"text","text":"person answer"}],"stop_reason":"end_turn"}}
`, submissionEventID("hosted", "person-starter"), submissionEventID("hosted", "person-correction")))
	// The saved commit confirmed both, owing their member completion, when
	// Nano stopped before its member step ran.
	for _, action := range []string{"person-starter", "person-correction"} {
		if _, settled, err := h.prompts.Settle(ctx, action); err != nil || !settled {
			t.Fatalf("settle %s: %v %v", action, settled, err)
		}
	}
	waitForCatalogWithin(t, time.Minute, func() bool {
		rows, err := h.server.hostedCatalog().SavedPromptRows(ctx, native, history.SavedUserKeys{NativeEventIDs: []string{submissionEventID("hosted", "person-correction")}})
		return err == nil && len(rows) == 1
	})

	// Restart: a new session with nothing waiting. Its attach pass reads the
	// owed prompts, and the answer is recorded with no live session to tell.
	after := attach()
	after.SettlePrompts(ctx)

	input, output := int64(10), int64(5)
	if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: turnOf["person-correction"],
		FinalMessageID: "person-final:0", Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter", Usage: coreturns.Usage{InputTokens: &input, OutputTokens: &output}}); err != nil {
		t.Fatal(err)
	}
	waitForCatalogWithin(t, time.Minute, func() bool {
		after.SettleWaitingPrompts(ctx) // what a later saved commit runs
		record, err := core.Get(ctx, "hosted", turnOf["person-starter"])
		return err == nil && record.Outcome != ""
	})
	record, err := core.Get(ctx, "hosted", turnOf["person-starter"])
	if err != nil || record.Outcome != agentTurnCompleted || record.FinalMessageID != "person-final:0" || !explicitZeroUsage(record) {
		t.Fatalf("absorbed person turn = %+v, %v; want the shared result with explicit zero usage", record, err)
	}
	if owing, err := h.prompts.Owing(ctx, "hosted", []string{"person-starter", "person-correction"}); err != nil || len(owing) != 0 {
		t.Fatalf("person prompts still owed: %v, %v", owing, err)
	}
}

// The historical accepted-forever duplicate: every attach republished every
// delivered prompt. The loaded saved tail grows with the number of those
// prompts, not with the turns the thread gained without one, so an old prompt
// whose saved row sat before the tail came back as a new last row. Attach must settle what the saved transcript
// proves, publish only the rest, and keep that true across a restart.
// Boundary: attach on a real catalog of each provider's saved format.
func TestReattachSettlesSavedPromptsAndPublishesOnlyTheRest(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			h := newSettlementHarness(t, provider)
			// Only Claude can prove an uncertain delivery: Codex and Grok record
			// no turn id without an answer, so that prompt stays visible.
			unanswered := prompts.Uncertain
			if provider == "claude" {
				unanswered = prompts.Confirmed
			}
			for restart := 0; restart < 2; restart++ {
				sess := h.attach(provider, nil)
				// What a pane opened now shows: the live text a new loop seeds
				// from the outbox. A settled prompt is never published again,
				// so no prompt is shown twice; unsettled prompts show once.
				// With no catalog source, joining the real pane cannot settle
				// a stale prompt and conceal an attach bug. Its wire tail still
				// exposes each seeded prompt's action identity and text.
				sess.AttachLiveText(sessions.NewLiveTextLoop(sessions.LiveTextSources{}, sess))
				pane := newLiveTextPane(threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40})
				liveTextOf(sess).subscribe(pane)
				select {
				case <-pane.ready:
				case <-time.After(3 * time.Second):
					t.Fatal("outbox pane did not publish its initial tail")
				}
				rawMessages := pane.take()
				liveTextOf(sess).unsubscribe(pane)
				actions, users := map[string]int{}, map[string]int{}
				for _, raw := range rawMessages {
					var message liveTextMessage
					if err := json.Unmarshal(raw, &message); err != nil {
						t.Fatal(err)
					}
					if message.Live == nil {
						continue
					}
					for _, entry := range *message.Live {
						if entry.Kind == "prompt" {
							actions[entry.ActionID]++
							users[strings.SplitN(entry.Text, "\n\n", 2)[0]]++
						}
					}
				}
				duplicated := false
				for _, count := range users {
					duplicated = duplicated || count > 1
				}
				if actions["old"] != 0 || actions["new"] != 0 || actions["running"] != 1 || actions["unproven"] != 1 ||
					users["running prompt"] != 1 || users["unproven prompt"] != 1 || duplicated ||
					unanswered == prompts.Uncertain && (actions["unanswered"] != 1 || users["unanswered prompt"] != 1) ||
					unanswered == prompts.Confirmed && actions["unanswered"] != 0 {
					t.Fatalf("attach %d published settled prompts or lost unsettled ones: actions=%v users=%v", restart, actions, users)
				}
				for name, want := range map[string]prompts.DeliveryState{"old": prompts.Confirmed, "new": prompts.Confirmed, "running": prompts.Accepted, "unproven": prompts.Accepted, "unanswered": unanswered} {
					record, _, err := h.prompts.Get(ctx, name)
					if err != nil || record.Outbox.State != want || record.ProviderText != name+" prompt" {
						t.Fatalf("attach %d: %s state=%q text kept=%v err=%v, want %q", restart, name, record.Outbox.State, record.ProviderText != "", err, want)
					}
				}
			}
			if provider == "claude" {
				// Confirmed by the submission id derived from its action, which
				// its stored binding lacks: its answered turn decides it too, so
				// the person's prompt owes nothing more.
				sess := h.attach(provider, nil, func(cfg *sessionConfig) {
					cfg.AgentExecutions = newTestAgentExecutionStore(t, "run-1")
					cfg.CompletedTurns = (hostedmodel.NewCompletedTurnStore(h.queue.Database())).CoreStore
				})
				waitForCatalogWithin(t, time.Minute, func() bool {
					if _, err := sess.SettlePrompts(ctx); err != nil {
						return false
					}
					owing, err := h.prompts.Owing(ctx, "hosted", []string{"unanswered"})
					return err == nil && len(owing) == 0
				})
			}
		})
	}
}

// savedTurnRows is one ended turn as each provider saves it. A prompted turn
// carries the identity its delivery receipt names (receiptFor).
func savedTurnRows(provider, name string, prompted bool, at int) string {
	switch provider {
	case "codex":
		rows := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-%[1]s"}}
{"type":"event_msg","payload":{"type":"user_message","message":"%[1]s prompt"}}
{"type":"response_item","payload":{"type":"message","id":"%[1]s-answer","role":"assistant","content":[{"type":"output_text","text":"%[1]s answer"}]}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-%[1]s"}}
`, name)
		if prompted {
			rows = strings.Replace(rows, `"type":"user_message",`, `"type":"user_message","client_id":`+strconv.Quote(submissionEventID("hosted", name))+`,`, 1)
		}
		return rows
	case "claude":
		uuid := name + "-native-row"
		if prompted {
			uuid = submissionEventID("hosted", name)
		}
		return fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"%[2]s prompt"}}
{"type":"assistant","uuid":"%[2]s-event","sessionId":"claude-settle","message":{"id":"%[2]s-message","role":"assistant","content":[{"type":"text","text":"%[2]s answer"}],"stop_reason":"end_turn"}}
`, uuid, name)
	default:
		return fmt.Sprintf(`{"timestamp":%[2]d,"method":"session/update","params":{"sessionId":"22222222-2222-4222-8222-222222222222","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"%[1]s prompt"}},"_meta":{"eventId":"%[1]s-user-event"}}}
{"timestamp":%[3]d,"method":"session/update","params":{"sessionId":"22222222-2222-4222-8222-222222222222","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"%[1]s answer"}},"_meta":{"promptId":"prompt-%[1]s","eventId":"%[1]s-agent-event"}}}
{"timestamp":%[4]d,"method":"_x.ai/session/update","params":{"sessionId":"22222222-2222-4222-8222-222222222222","update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-%[1]s","stop_reason":"end_turn"}}}
`, name, at, at+1, at+2)
	}
}

// receiptFor is the provider's answer to delivering the prompt name.
func receiptFor(provider, name string) json.RawMessage {
	switch provider {
	case "claude":
		return json.RawMessage(`{"turn":{"id":"claude:shim:` + name + `"},"event":{"id":"` + submissionEventID("hosted", name) + `"}}`)
	case "grok":
		return json.RawMessage(`{"turn":{"id":"prompt-` + name + `"}}`)
	default:
		return json.RawMessage(`{"turn":{"id":"turn-` + name + `"}}`)
	}
}

// The live-settlement regression: settlement ran only at attach, reconcile
// and repair, so a prompt answered while its session stayed attached stayed
// accepted, and shown, until the next restart. With no restart, each prompt
// must settle by exact identity: one whose turn was open at attach, once the
// turn ends; one delivered live, once its turn is saved; and one whose whole
// turn the provider saved before its delivery receipt returned, when no
// later commit comes. A prompt the saved transcript cannot prove
// ("unproven") must cost later commits no saved read.
// Boundary: the catalog's transcript-changed callback, on a real catalog of
// each provider's saved format, and delivery through the send path.
func TestSavedCommitSettlesPromptsWithoutReattach(t *testing.T) {
	for _, test := range []struct {
		provider string
		ends     string // the provider ends the turn open at attach
	}{
		{provider: "codex", ends: `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-running"}}` + "\n"},
		{provider: "claude", ends: `{"type":"assistant","uuid":"running-end","sessionId":"claude-settle","message":{"id":"running-message","role":"assistant","content":[{"type":"text","text":"running answer"}],"stop_reason":"end_turn"}}` + "\n"},
		{provider: "grok", ends: `{"timestamp":1893456100,"method":"_x.ai/session/update","params":{"sessionId":"22222222-2222-4222-8222-222222222222","update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-running","stop_reason":"end_turn"}}}` + "\n"},
	} {
		t.Run(test.provider, func(t *testing.T) {
			h := newSettlementHarness(t, test.provider)
			var running, finished atomic.Int32
			// commit appends saved rows and returns once the catalog's callback
			// for them has reached the session, and, if finish, has finished.
			commit := func(rows string, finish bool) {
				t.Helper()
				for running.Load() != 0 {
					time.Sleep(time.Millisecond)
				}
				before := finished.Load()
				h.appendSaved(t, rows)
				deadline := time.Now().Add(30 * time.Second)
				for finished.Load() == before && (finish || running.Load() == 0) || finish && running.Load() != 0 {
					if time.Now().After(deadline) {
						t.Fatal("the saved commit did not reach the session")
					}
					time.Sleep(time.Millisecond)
				}
			}
			var answer func(name string) json.RawMessage
			sess := h.attach(test.provider, func(_ context.Context, text string) (json.RawMessage, error) {
				return answer(strings.TrimSuffix(text, " prompt")), nil
			})
			var reads atomic.Int32
			savedPrompts := h.savedReader(sess)
			h.setSavedReader(sess, func(ctx context.Context, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
				reads.Add(1)
				return savedPrompts(ctx, keys)
			})
			seedTestSessionRegistry(&h.server.Registry, map[string]*session{sess.ID(): sess})
			h.service.SetTranscriptChanged(func(ctx context.Context, change threadindex.TranscriptChange) {
				running.Add(1)
				defer running.Add(-1)
				fixtureOwners(h.server).attachmentCoordinator().ConfirmDurableThread(ctx, change)
				finished.Add(1)
			})
			settles := func(action string) {
				t.Helper()
				deadline := time.Now().Add(30 * time.Second)
				for h.state(t, action) != prompts.Confirmed {
					if time.Now().After(deadline) {
						t.Fatalf("%q stayed %s", action, h.state(t, action))
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			deliver := func(name string) {
				t.Helper()
				if rec := postTurn(t, sess, context.Background(), name, name+" prompt"); rec.Code != http.StatusOK {
					t.Fatalf("%s delivery = %d %s", name, rec.Code, rec.Body.String())
				}
			}

			commit(test.ends, true)
			settles("running")

			answer = func(name string) json.RawMessage { return receiptFor(test.provider, name) }
			deliver("live")
			commit(savedTurnRows(test.provider, "live", true, 1893456110), true)
			settles("live")
			var turn struct {
				Turn struct{ ID string } `json:"turn"`
			}
			if err := json.Unmarshal(receiptFor(test.provider, "live"), &turn); err != nil {
				t.Fatal(err)
			}
			// The provider ends the live turn, so the next prompt is sent now.
			waitTurnSendDone(t, sess, "live")
			h.runtimes[sess].completeTurn(t, turn.Turn.ID, "live")

			// The provider saves the whole turn, and the catalog commits it,
			// before the receipt returns. Codex and Grok callbacks wait for the
			// delivery; Claude's finishes first, and no later commit comes.
			answer = func(name string) json.RawMessage {
				commit(savedTurnRows(test.provider, name, true, 1893456120), false)
				return receiptFor(test.provider, name)
			}
			deliver("early")
			settles("early")

			if state := h.state(t, "unproven"); state != prompts.Accepted {
				t.Fatalf("unproven prompt state = %q, want accepted", state)
			}
			before := reads.Load()
			commit(savedTurnRows(test.provider, "later", false, 1893456130), true)
			if test.provider == "claude" {
				// Each settled Claude prompt owes its member completion, which
				// these fixture turns never finish, so commits keep reading for
				// it. TestOverlappingSettlementPassesLeaveNothingWaiting proves
				// the reads stop once owed work drains.
				if owing, err := h.prompts.Owing(context.Background(), "hosted", []string{"running", "live", "early"}); err != nil || len(owing) != 3 {
					t.Fatalf("owed after settlement = %v, %v; want all three", owing, err)
				}
			} else if got := reads.Load(); got != before {
				t.Fatalf("a commit with no prompt left to settle read saved prompts %d times", got-before)
			}
		})
	}
}

// A person's Codex prompt with no saved row is resent only on proof that its
// turn dropped it: this attachment saw the turn from its start end
// interrupted with no echo of the prompt's client id, and the saved
// transcript holds no row of it. Codex records the prompt a turn starts with
// only once the turn begins work, after any context compaction, and holds a
// steered one until the turn's next step; an interruption before then drops
// it. Anything less leaves the prompt as it is, for the person to resend or
// discard, and identity decides which prompt was dropped, never text. A turn
// the operator stopped is proof too, but Stop means stop: the dropped prompt
// is held for the person, never sent again, also when the Stop lands while a
// pass is already reading saved rows. Boundary: the live Codex wire and the
// Stop route, through the settlement pass of a real catalog of the saved
// transcript, to the provider sender.
func TestCodexPromptIsResentOnlyOnProofItsTurnDroppedIt(t *testing.T) {
	type frame string
	const (
		started     frame = "started"
		echoed      frame = "echo of the prompt"
		neighbour   frame = "echo of an equal-text prompt"
		interrupted frame = "interrupted"
		completed   frame = "completed"
		stop        frame = "Stop"
		reading     frame = "a pass starts reading saved rows"
		read        frame = "that pass finishes"
	)
	for _, test := range []struct {
		name       string
		watched    bool    // this attachment delivered the prompt
		frames     []frame // what this attachment saw of the prompt's turn
		savedRow   bool    // the saved transcript holds the prompt's row
		savedEnded bool    // ... and the turn's end
		resent     bool
		wantState  prompts.DeliveryState
	}{
		{name: "delivered before a restart", frames: []frame{started, interrupted}, wantState: prompts.Accepted},
		{name: "turn not seen from its start", watched: true, frames: []frame{interrupted}, wantState: prompts.Accepted},
		{name: "turn still open", watched: true, frames: []frame{started}, wantState: prompts.Accepted},
		{name: "turn completed", watched: true, frames: []frame{started, completed}, wantState: prompts.Accepted},
		{name: "echoed, row not saved yet", watched: true, frames: []frame{started, echoed, interrupted}, wantState: prompts.Accepted},
		{name: "saved in an interrupted turn the wire did not echo", watched: true, frames: []frame{started, interrupted}, savedRow: true, savedEnded: true, wantState: prompts.Confirmed},
		{name: "saved in a turn still open", watched: true, frames: []frame{started}, savedRow: true, wantState: prompts.Accepted},
		{name: "dropped beside an echoed equal-text prompt", watched: true, frames: []frame{started, neighbour, interrupted}, resent: true, wantState: prompts.Expired},
		{name: "dropped by a Stop", watched: true, frames: []frame{started, stop, interrupted}, wantState: prompts.Uncertain},
		{name: "dropped by a Stop pressed while a pass reads", watched: true, frames: []frame{started, reading, stop, interrupted, read}, wantState: prompts.Uncertain},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			h := newSettlementHarness(t, "codex")
			native, turn, text := h.fixture.native, "turn-case", "same prompt"
			event := submissionEventID("hosted", "prompt")
			if !test.watched {
				if _, _, err := h.prompts.Submit(ctx, prompts.Submission{HostedThreadID: "hosted", Provider: model.ProviderCodex, NativeThreadID: native, ActionID: "prompt",
					ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: prompts.HumanOrigin}); err != nil {
					t.Fatal(err)
				}
				if _, claimed, err := h.prompts.Claim(ctx, prompts.ClaimRequest{HostedThreadID: "hosted", Token: "case", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
					t.Fatalf("claim = %v, %v", claimed, err)
				}
				binding := prompts.Binding{EventID: event, Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}}
				if _, err := h.prompts.Accepted(ctx, prompts.Acceptance{ActionID: "prompt", ClaimToken: "case", AttachmentEpoch: 1, Binding: binding}); err != nil {
					t.Fatal(err)
				}
			}

			sent := make(chan string, 4)
			var initial atomic.Bool
			initial.Store(test.watched)
			projection := newLiveProjection("codex", "hosted")
			sess := h.attach("codex", func(_ context.Context, text string) (json.RawMessage, error) {
				if initial.Swap(false) {
					return json.RawMessage(`{"turn":{"id":"turn-case"}}`), nil
				}
				sent <- text
				return json.RawMessage(`{"turn":{"id":"turn-next"}}`), nil
			}, func(cfg *sessionConfig) { cfg.Projection = projection })
			runtime := h.runtimes[sess]
			if test.watched {
				if response := postTurn(t, sess, ctx, "prompt", text); response.Code != http.StatusOK {
					t.Fatalf("initial prompt = %d %s", response.Code, response.Body.String())
				}
			}

			// Saved rows are indexed first: a row the saved transcript holds
			// vetoes a resend, whatever the wire showed.
			if test.savedRow {
				rows := fmt.Sprintf("{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":%[1]q}}\n"+
					"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"id\":\"case-message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":%[2]q}],\"internal_chat_message_metadata_passthrough\":{\"turn_id\":%[1]q}}}\n"+
					"{\"type\":\"event_msg\",\"payload\":{\"type\":\"item_completed\",\"turn_id\":%[1]q,\"item\":{\"type\":\"UserMessage\",\"id\":\"case-item\",\"client_id\":%[3]q,\"content\":[{\"type\":\"text\",\"text\":%[2]q}]}}}\n", turn, text, event)
				if test.savedEnded {
					rows += fmt.Sprintf("{\"type\":\"event_msg\",\"payload\":{\"type\":\"turn_aborted\",\"turn_id\":%q,\"reason\":\"interrupted\"}}\n", turn)
				}
				h.appendSaved(t, rows)
				waitForCatalogWithin(t, time.Minute, func() bool {
					rows, err := h.savedReader(sess)(ctx, history.SavedUserKeys{NativeTurnIDs: []string{turn}})
					return err == nil && len(rows) > 0 && (!test.savedEnded || rows[0].TurnStatus == history.TurnInterrupted)
				})
			}
			echo := func(client string) {
				runtime.emitAndWait(t, []byte(`{"method":"item/completed","params":{"threadId":"`+native+`","turnId":"`+turn+`","item":{"id":"`+client+`-item","type":"userMessage","clientId":"`+client+`","content":[{"type":"text","text":"`+text+`"}]}}}`))
			}
			var release, passed chan struct{}
			var play func(frame)
			play = func(f frame) {
				switch f {
				case stop:
					// The person stops the turn the pane shows working.
					waitForCatalogWithin(t, 10*time.Second, func() bool {
						return sess.Summary().TurnActive && liveStatusForTest(t, runtime.session).Runtime.TurnID == turn
					})
					rec := httptest.NewRecorder()
					sess.HandleInterrupt(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/x/interrupt", nil))
					if rec.Code != http.StatusOK || h.interrupts.Load() != 1 {
						t.Fatalf("Stop = %d %s, %d interrupts", rec.Code, rec.Body.String(), h.interrupts.Load())
					}
				case reading:
					entered, read := make(chan struct{}), h.savedReader(sess)
					release, passed = make(chan struct{}), make(chan struct{})
					var hold sync.Once
					h.setSavedReader(sess, func(ctx context.Context, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
						hold.Do(func() { close(entered); <-release })
						return read(ctx, keys)
					})
					go func() { sess.SettleWaitingPrompts(ctx); close(passed) }()
					<-entered
				case read:
					close(release)
					<-passed
				case started:
					runtime.emitAndWait(t, []byte(`{"method":"turn/started","params":{"threadId":"`+native+`","turn":{"id":"`+turn+`","status":"inProgress","items":[]}}}`))
				case echoed:
					echo(event)
				case neighbour:
					echo(submissionEventID("hosted", "neighbour"))
				case interrupted, completed:
					status := map[frame]string{interrupted: "interrupted", completed: "completed"}[f]
					runtime.emitAndWait(t, []byte(`{"method":"turn/completed","params":{"threadId":"`+native+`","turn":{"id":"`+turn+`","status":"`+status+`"}}}`))
				}
			}
			for _, f := range test.frames {
				play(f)
			}
			sess.SettleWaitingPrompts(ctx) // the pass a saved commit runs

			select {
			case got := <-sent:
				if !test.resent || got != text {
					t.Fatalf("sent %q again; want resent=%v", got, test.resent)
				}
			case <-time.After(time.Second):
				if test.resent {
					t.Fatal("the dropped prompt was never sent again")
				}
			}
			select {
			case got := <-sent:
				t.Fatalf("sent a second time: %q", got)
			case <-time.After(200 * time.Millisecond):
			}
			if state := h.state(t, "prompt"); state != test.wantState {
				t.Fatalf("prompt = %s; want %s", state, test.wantState)
			}
			if test.wantState == prompts.Uncertain {
				if view, code := readPrompt(t, sess, "prompt"); code != http.StatusOK || view.State != string(prompts.Uncertain) || view.Label != "Stopped before Codex took it" {
					t.Fatalf("held read = %d %+v; want held for the person", code, view)
				}
				// Codex saves it after all: the hold ends like any prompt's wait.
				if _, settled, err := h.prompts.Settle(ctx, "prompt"); err != nil || !settled {
					t.Fatalf("settle = %v, %v", settled, err)
				}
				if view, code := readPrompt(t, sess, "prompt"); code != http.StatusOK || view.State != string(prompts.Confirmed) || view.AllowedActions != nil {
					t.Fatalf("read once saved = %d %+v; want confirmed", code, view)
				}
			}
		})
	}
}

// Two settlement passes can overlap, such as a delivery's catch-up and a
// catalog commit. If the one that finishes last restores the waiting mark of
// a prompt the other settled, every later idle commit reads saved prompts
// again. Boundary: the catalog callback's session step on a real catalog,
// with the session's saved reads held so that, unserialized, both passes read
// the accepted prompt before either settles it.
func TestOverlappingSettlementPassesLeaveNothingWaiting(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	// Nothing may be left to do once the overlap settles, owed member
	// completions included: a settled Claude prompt owes its own completion,
	// and while it does, commits keep reading. The fixture's identity-less
	// uncertain prompt could never finish, so it is expired up front, and each
	// settled prompt's own turn carries its result.
	if _, expired, err := h.prompts.Expire(ctx, "unanswered", time.Now()); err != nil || !expired {
		t.Fatalf("expire unanswered = %v, %v", expired, err)
	}
	completed := hostedmodel.NewCompletedTurnStore(h.queue.Database())
	core, err := completed.CoreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old", "new", "running"} {
		turn := h.fixture.turns[name]
		if _, _, err := core.Started(ctx, coreturns.Start{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: h.fixture.native, TurnID: turn, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: h.fixture.native, TurnID: turn, Outcome: "failed", At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
	}
	sess := h.attach("claude", nil, func(cfg *sessionConfig) {
		cfg.AgentExecutions = newTestAgentExecutionStore(t, "run-1")
		cfg.CompletedTurns = completed.CoreStore
	}) // "running" waits: its saved turn is open
	h.appendSaved(t, `{"type":"assistant","uuid":"running-end","sessionId":"claude-settle","message":{"id":"running-message","role":"assistant","content":[{"type":"text","text":"running answer"}],"stop_reason":"end_turn"}}`+"\n")
	saved := h.savedReader(sess)
	running := history.SavedUserKeys{NativeEventIDs: []string{submissionEventID("hosted", "running")}}
	waitForCatalogWithin(t, time.Minute, func() bool {
		rows, err := saved(context.Background(), running)
		return err == nil && len(rows) > 0 && liveview.DurableTerminalStatus(string(rows[0].TurnStatus))
	})
	// Only each competing pass's first settlement read is held, found by the
	// pass's own context; the member step's reads run under a context of
	// their own and are never held, however many a pass makes.
	type passKey struct{}
	var reads atomic.Int32
	var held [3]atomic.Bool
	entered := make(chan struct{}, 2)
	gates := [3]chan struct{}{nil, make(chan struct{}), make(chan struct{})}
	first, second := gates[1], gates[2]
	h.setSavedReader(sess, func(ctx context.Context, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
		reads.Add(1)
		rows, err := saved(ctx, keys)
		if n, ok := ctx.Value(passKey{}).(int); ok && held[n].CompareAndSwap(false, true) {
			entered <- struct{}{}
			<-gates[n]
		}
		return rows, err
	})
	passes := 0
	pass := func() chan struct{} {
		passes++
		passCtx := context.WithValue(context.Background(), passKey{}, passes)
		done := make(chan struct{})
		go func() { sessions.ReconcileSavedTranscript(passCtx, sess); close(done) }()
		return done
	}
	done1 := pass()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the first pass did not read saved prompts")
	}
	done2 := pass()
	select { // unserialized, the second pass reads too
	case <-entered:
	case <-time.After(200 * time.Millisecond):
	}
	close(first)
	<-done1
	close(second)
	<-done2
	if state := h.state(t, "running"); state != prompts.Confirmed {
		t.Fatalf("running = %s, want confirmed", state)
	}
	// The regression this case owns: the pass that finishes last must not
	// restore the waiting mark of the prompt the other settled. Checked before
	// any later commit, which would otherwise drop a restored mark again.
	sess.RequestMemberStep() // clear the completed member hint without altering prompt waits
	beforeWaitingRead := reads.Load()
	sessions.ReconcileSavedTranscript(ctx, sess)
	if got := reads.Load(); got != beforeWaitingRead {
		t.Fatalf("the overlapping passes left prompt waits: the next idle commit read saved prompts %d times", got-beforeWaitingRead)
	}
	// The pass that finished each owed completion still saw it; one more
	// commit reads that none is left. After that, nothing waits.
	sessions.ReconcileSavedTranscript(ctx, sess)
	if owing, err := h.prompts.Owing(ctx, "hosted", []string{"old", "new", "running"}); err != nil || len(owing) != 0 {
		t.Fatalf("owed after the overlap = %v, %v; want every completion finished", owing, err)
	}
	before := reads.Load()
	sessions.ReconcileSavedTranscript(context.Background(), sess)
	if got := reads.Load(); got != before {
		t.Fatalf("an idle commit after the overlap read saved prompts %d times", got-before)
	}
}

// Steering puts several prompts in one Codex turn, so the turn no longer
// identifies a prompt: each must settle only when the saved transcript holds
// its own client id. A prompt Codex acknowledged but never saved (a hook
// rejected it) stays accepted rather than settling with its neighbours.
// Boundary: settlement on a real catalog of Codex's saved format, where the
// client id is on the completed copy of each user row.
func TestCodexPromptsSharingATurnSettleByTheirOwnEvents(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "codex")
	native, turn := h.fixture.native, "turn-shared"
	for _, name := range []string{"starter", "steered", "rejected"} {
		prompt := name + " prompt"
		if _, _, err := h.prompts.Submit(ctx, prompts.Submission{
			HostedThreadID: "hosted", Provider: model.ProviderCodex, NativeThreadID: native, ActionID: name,
			ProviderText: prompt, PresentationText: prompt, TextHash: sendTextHash(prompt), RequestHash: sendTextHash(prompt), Origin: prompts.AgentOrigin,
		}); err != nil {
			t.Fatal(err)
		}
		token := "claim-" + name
		if _, claimed, err := h.prompts.Claim(ctx, prompts.ClaimRequest{HostedThreadID: "hosted", Token: token, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: claimed=%v err=%v", name, claimed, err)
		}
		binding := prompts.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}, EventID: submissionEventID("hosted", name)}
		if _, err := h.prompts.Accepted(ctx, prompts.Acceptance{ActionID: name, ClaimToken: token, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatalf("accept %s into the shared turn: %v", name, err)
		}
	}
	var rows strings.Builder
	fmt.Fprintf(&rows, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":%q}}\n", turn)
	for _, name := range []string{"starter", "steered"} {
		fmt.Fprintf(&rows, "{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"id\":%q,\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":%q}],\"internal_chat_message_metadata_passthrough\":{\"turn_id\":%q}}}\n", name+"-message", name+" prompt", turn)
		fmt.Fprintf(&rows, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"item_completed\",\"turn_id\":%q,\"item\":{\"type\":\"UserMessage\",\"id\":%q,\"client_id\":%q,\"content\":[{\"type\":\"text\",\"text\":%q}]}}}\n", turn, name+"-item", submissionEventID("hosted", name), name+" prompt")
	}
	fmt.Fprintf(&rows, "{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"id\":\"shared-answer\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"shared answer\"}]}}\n")
	fmt.Fprintf(&rows, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"turn_id\":%q}}\n", turn)
	h.appendSaved(t, rows.String())

	sess := h.attach("codex", nil)
	var results map[string]transcript.Settlement
	waitForCatalogWithin(t, time.Minute, func() bool {
		var err error
		results, err = settleThreadPrompts(ctx, h.prompts, h.savedReader(sess), "hosted", "codex", native)
		return err == nil && h.state(t, "steered") == prompts.Confirmed
	})
	for name, want := range map[string]prompts.DeliveryState{"starter": prompts.Confirmed, "steered": prompts.Confirmed, "rejected": prompts.Accepted} {
		if got := h.state(t, name); got != want {
			t.Fatalf("%s state=%s; want %s (results=%v)", name, got, want, results)
		}
	}
}

// Claude can save a prompt in a form other than the text Nano sent; the row
// still carries the send's stamped id and must settle it. Before, each case
// left the prompt pending for good although Claude had run it:
//   - Prompts that queued together merge into one row: its uuid is one send's
//     id, and mergedSendUuids names the others' (Claude 2.1.300).
//   - A prompt that is a slash command is saved as Claude's expansion of it
//     (<command-message>, <command-name>, <command-args>), which read as
//     injected context, not as the prompt (Claude 2.1.310). It is read as the
//     command sent, "/X A".
//
// Fixtures are real rows' shapes, scrubbed, with ids stamped for this
// thread. Boundary: settlement on a real catalog of Claude's saved format.
func TestClaudePromptsSettleWithTheirSavedRow(t *testing.T) {
	for _, test := range []struct {
		name, fixture string
		actions       []string
		sent          string // the prompt's text as sent, when the row shows it
	}{
		{name: "merged prompts", fixture: "claude-merged-prompts.jsonl", actions: []string{"merged-first", "merged-second", "merged-third"}},
		{name: "a slash command", fixture: "claude-sent-command.jsonl", actions: []string{"sent-command"}, sent: "/prepare-repo /tmp/project\n\nCheck it is ready."},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			h := newSettlementHarness(t, "claude")
			native := h.fixture.native
			for i, action := range test.actions {
				prompt := action + " prompt"
				if test.sent != "" {
					prompt = test.sent
				}
				seedAcceptedTransactionalTurn(t, h.prompts, "hosted", model.ProviderClaude, native, action, fmt.Sprintf("claude:shim:%d", 300+i), prompt)
			}
			var rows strings.Builder
			for _, line := range fixtureLines(t, test.fixture) {
				rows.WriteString(strings.ReplaceAll(string(line), liveTextFixtureSession, native) + "\n")
			}
			for _, action := range test.actions {
				if !strings.Contains(rows.String(), submissionEventID("hosted", action)) {
					t.Fatalf("fixture lacks %s's stamped id", action)
				}
			}
			h.appendSaved(t, rows.String())

			sess := h.attach("claude", nil)
			waitForCatalogWithin(t, time.Minute, func() bool {
				_, err := sess.SettlePrompts(ctx)
				return err == nil && h.state(t, test.actions[0]) == prompts.Confirmed
			})
			for _, action := range test.actions {
				if got := h.state(t, action); got != prompts.Confirmed {
					t.Fatalf("%s state=%s; want confirmed by its saved row", action, got)
				}
			}
			if test.sent == "" {
				return
			}
			threadID, err := h.service.ResolveThreadReference(ctx, native)
			if err != nil {
				t.Fatal(err)
			}
			page, err := h.service.LoadTranscript(ctx, threadID, threadindex.TranscriptQuery{Limit: 100, View: threadindex.DefaultThreadTranscriptView, HiddenSpecified: true})
			if err != nil {
				t.Fatal(err)
			}
			shown := 0
			for _, item := range page.Items {
				if item.Type == "user_message" && item.Text == test.sent {
					shown++
				}
			}
			if shown != 1 {
				t.Fatalf("the transcript shows the prompt %d times as sent; want once, as %q", shown, test.sent)
			}
		})
	}
}

// Codex can take a prompt after Nano stopped waiting for its turn/start reply.
// The prompt is then uncertain, with no binding, and its saved row alone could
// never settle it, so the pane would show it pending while the agent answers.
// Codex's echo of the prompt's own client id binds it to the turn that took
// it: before or after Nano records the uncertain outcome, in a turn this
// attachment did not see start, after a reconnect, or after the turn was
// saved and ended. A prompt that waited in the queue first is not parked. The
// saved row then confirms it, and nothing is sent again. Boundary: the HTTP
// send and the live Codex wire, settled against a real catalog.
func TestCodexPromptTakenPastTheDeadlineIsBoundByItsEcho(t *testing.T) {
	for _, test := range []struct {
		name        string
		queuedFirst bool // sent while a turn worked, delivered when it ended
		echoFirst   bool // Codex echoes it, in a turn already running, before Nano records the outcome
		reconnect   bool // the link is re-established before the echo
		savedFirst  bool // the turn is saved and ended, and its commit settled, before the echo
	}{
		{name: "echoed after the deadline"},
		{name: "queued first, echoed after the deadline", queuedFirst: true},
		{name: "echoed after a reconnect", reconnect: true},
		{name: "echoed after its turn was saved and ended", savedFirst: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			h := newSettlementHarness(t, "codex")
			// The fixture's other unsettled prompts would keep every saved
			// commit's pass running; only this prompt may.
			for _, action := range []string{"unanswered", "old", "new", "running", "unproven"} {
				if _, expired, err := h.prompts.Expire(ctx, action, time.Now()); err != nil || !expired {
					t.Fatalf("expire %s = %v, %v", action, expired, err)
				}
			}
			native, turn, text := h.fixture.native, "turn-late", "late prompt"
			event := submissionEventID("hosted", "late")
			started := `{"method":"turn/started","params":{"threadId":"` + native + `","turn":{"id":"` + turn + `","status":"inProgress","items":[]}}}`
			echo := `{"method":"item/started","params":{"threadId":"` + native + `","turnId":"` + turn + `","item":{"id":"late-item","type":"userMessage","clientId":"` + event + `","content":[{"type":"text","text":"` + text + `"}]}}}`
			var sess *session
			var sent atomic.Int32
			sess = h.attach("codex", func(context.Context, string) (json.RawMessage, error) {
				sent.Add(1)
				if test.echoFirst {
					// Frames that arrive during the call wait in the session's
					// mailbox and may be read before Nano records the outcome;
					// this reads the echo first, deterministically.
					h.runtimes[sess].emitAndWait(t, []byte(echo))
				}
				return nil, nil // The scripted peer withholds this reply until Nano times out.
			})
			runtime := h.runtimes[sess]
			runtime.codex.Mu.Lock()
			runtime.codex.WithholdTurnStart = true
			runtime.codex.Mu.Unlock()
			if test.queuedFirst {
				runtime.activate(t)
			}
			if rec := postTurn(t, sess, ctx, "late", text); rec.Code != http.StatusAccepted {
				t.Fatalf("send = %d %s; want 202", rec.Code, rec.Body.String())
			}
			if test.queuedFirst {
				runtime.finishTurn(t)
			}
			// The real delivery deadline is 30 seconds; wait for its journal result.
			waitForCatalogWithin(t, 35*time.Second, func() bool {
				attempt, err := sess.PromptStore().GetAttempt(ctx, "late")
				return err == nil && attempt.State != prompts.JournalQueued && attempt.State != prompts.JournalSending
			})
			waitTurnSendDone(t, sess, "late")

			saved := func() {
				h.appendSaved(t, fmt.Sprintf("{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":%[1]q}}\n"+
					"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"id\":\"late-message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":%[2]q}],\"internal_chat_message_metadata_passthrough\":{\"turn_id\":%[1]q}}}\n"+
					"{\"type\":\"event_msg\",\"payload\":{\"type\":\"item_completed\",\"turn_id\":%[1]q,\"item\":{\"type\":\"UserMessage\",\"id\":\"late-item\",\"client_id\":%[3]q,\"content\":[{\"type\":\"text\",\"text\":%[2]q}]}}}\n"+
					"{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"turn_id\":%[1]q}}\n", turn, text, event))
			}
			if test.savedFirst {
				saved()
				waitForCatalogWithin(t, time.Minute, func() bool {
					rows, err := h.savedReader(sess)(ctx, history.SavedUserKeys{NativeTurnIDs: []string{turn}})
					return err == nil && len(rows) > 0 && rows[0].TurnStatus == history.TurnCompleted
				})
				sess.SettleWaitingPrompts(ctx) // the commit's pass, while the prompt is still unbound
			}
			if test.reconnect {
				before := runtime.codex.ResumeCount()
				runtime.codex.DropConnections()
				waitForClaude(t, "echo after reconnect", func() bool { return runtime.codex.ResumeCount() > before && sess.Summary().Status == "ready" })
			}
			if !test.echoFirst {
				for _, frame := range []string{started, echo} {
					runtime.emitAndWait(t, []byte(frame))
				}
			}
			if test.savedFirst {
				// No later saved commit comes: the bind itself must settle it.
				waitForCatalogWithin(t, 5*time.Second, func() bool { return h.state(t, "late") == prompts.Confirmed })
			} else {
				waitForCatalogWithin(t, 5*time.Second, func() bool { return h.state(t, "late") == prompts.Accepted })
				saved()
				waitForCatalogWithin(t, time.Minute, func() bool {
					sess.SettleWaitingPrompts(ctx) // the pass a saved commit runs
					return h.state(t, "late") == prompts.Confirmed
				})
			}
			record, _, err := h.prompts.Get(ctx, "late")
			if err != nil || record.ProviderTurnID != turn || record.Outbox.Binding == nil || record.Outbox.Binding.EventID != event {
				t.Fatalf("late prompt = %+v, %v; want it bound to %s by its own event", record.Outbox, err, turn)
			}
			if got := sent.Load(); got != 1 {
				t.Fatalf("sent %d times; want once", got)
			}
		})
	}
}

// Prompts steered into one Codex turn share its reply. When that turn ends
// while Nano is down, no live item reached their operations, so each must
// take the turn's own final answer from what resume resolves, never an empty
// or stale reply, once its prompt is proven saved. That includes a member
// bound after resume, which never observed the answer itself.
// Boundary: resume of a hosted Codex thread through the completed-turn record
// to the agent operations.
func TestSharedTurnEndedWhileDownGivesEveryProvenOperationItsFinalAnswer(t *testing.T) {
	ctx := context.Background()
	s, daemon, rec, _, _ := subscribedCodexServer(t, `[{"id":"turn-shared","status":"inProgress","startedAt":1787600000}]`)
	var ops []threads.TurnRecord
	for _, action := range []string{"starter", "correction"} {
		op, _, err := fixtureBeginTurn(s.agentExecutions, rec.HostedThreadID, action, "hash-"+action)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.agentExecutions.MarkTurnDelivery(op.ID, "turn-shared", agentTurnRunning, nil); err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op)
	}
	fixtureOwners(s).attachmentCoordinator().ReattachOnBoot()
	var sess *session
	waitForClaude(t, "the reattached session", func() bool {
		sess = s.SessionForHostedThread(rec.HostedThreadID)
		return sess != nil && sess.Summary().Status == "ready" && sess.PromptStore() != nil
	})
	native, turn := rec.NativeThreadID, "turn-shared"
	proveSaved := func(op threads.TurnRecord) {
		action, text := op.TransportActionID, "prompt "+op.ClientActionID
		if _, _, err := sess.PromptStore().Submit(ctx, prompts.Submission{HostedThreadID: rec.HostedThreadID, Provider: model.ProviderCodex, NativeThreadID: native,
			ActionID: action, ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := sess.PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: rec.HostedThreadID, Token: "claim-" + action, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", action, claimed, err)
		}
		binding := prompts.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}, EventID: submissionEventID(rec.HostedThreadID, action)}
		if _, err := sess.PromptStore().Accepted(ctx, prompts.Acceptance{ActionID: action, ClaimToken: "claim-" + action, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if _, settled, err := sess.PromptStore().Settle(ctx, action); err != nil || !settled {
			t.Fatalf("settle %s: %v %v", action, settled, err)
		}
	}
	for _, op := range ops {
		proveSaved(op)
	}

	resumed := `{"thread":{"id":"` + native + `","turns":[{"id":"turn-shared","status":"completed","completedAt":1787600100,"items":[{"id":"final-shared","type":"agentMessage","phase":"final_answer","text":"the shared answer"}]}]}}`
	var reply map[string]any
	if err := json.Unmarshal([]byte(resumed), &reply); err != nil {
		t.Fatal(err)
	}
	daemon.SetResumeResult(reply)
	beforeResume := daemon.ResumeCount()
	daemon.DropConnections()
	waitForClaude(t, "shared-turn reconnect", func() bool { return daemon.ResumeCount() > beforeResume && sess.Summary().Status == "ready" })
	waitForClaude(t, "both operations finished", func() bool {
		for _, op := range ops {
			if got, _ := s.agentExecutions.Turn(op.ID); !threads.TurnTerminal(got.Status) {
				return false
			}
		}
		return true
	})
	for _, op := range ops {
		got, _ := s.agentExecutions.Turn(op.ID)
		if got.Status != agentTurnCompleted || got.FinalItemID != "final-shared" || got.FinalResponse != "the shared answer" {
			t.Fatalf("operation %s = status %s final %q %q; want the turn's final answer", op.ClientActionID, got.Status, got.FinalItemID, got.FinalResponse)
		}
	}
	completed, err := coreturns.OpenStore(ctx, s.queuedTurns.Database())
	if err != nil {
		t.Fatal(err)
	}
	if record, err := completed.Get(ctx, rec.HostedThreadID, turn); err != nil || record.FinalMessageID != "final-shared" {
		t.Fatalf("completed turn final = %q err=%v; want the final answer resume decoded", record.FinalMessageID, err)
	}

	late, _, err := fixtureBeginTurn(s.agentExecutions, rec.HostedThreadID, "late", "hash-late")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.agentExecutions.MarkTurnDelivery(late.ID, turn, agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	proveSaved(late)
	daemon.EmitThreadNotification(native, map[string]any{"method": "item/completed", "params": map[string]any{"threadId": native, "turnId": turn, "item": map[string]any{"id": "final-shared", "type": "agentMessage", "phase": "final_answer", "text": "the shared answer"}}})
	daemon.EmitThreadNotification(native, map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": native, "turn": map[string]any{"id": turn, "status": "completed", "completedAt": 1787600100}}})
	waitForClaude(t, "late proven member", func() bool { got, _ := s.agentExecutions.Turn(late.ID); return got.Status == agentTurnCompleted })
	if got, _ := s.agentExecutions.Turn(late.ID); got.Status != agentTurnCompleted || got.FinalItemID != "final-shared" || got.FinalResponse != "the shared answer" {
		t.Fatalf("late operation = status %s final %q %q; want the turn's final answer", got.Status, got.FinalItemID, got.FinalResponse)
	}
}

// An agent's correction steered into a turn a person started is the turn's
// only operation, but the turn ending does not prove Codex consumed it: a
// hook can reject it after Codex acknowledged it, and then its own row never
// appears. Its operation stays open rather than finishing with that turn, and
// finishes with the turn's final answer once its own row is proven saved.
// Boundary: the live Codex wire through the completed-turn record to the
// agent operation.
func TestCorrectionInAPersonsTurnDoesNotFinishWithoutItsOwnRow(t *testing.T) {
	ctx := context.Background()
	s, daemon, rec, _, _ := subscribedCodexServer(t, `[{"id":"turn-human","status":"inProgress","startedAt":1787600000}]`)
	op, _, err := fixtureBeginTurn(s.agentExecutions, rec.HostedThreadID, "correction", "hash-correction")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.agentExecutions.MarkTurnDelivery(op.ID, "turn-human", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	fixtureOwners(s).attachmentCoordinator().ReattachOnBoot()
	var sess *session
	waitForClaude(t, "the reattached turn to be active", func() bool {
		sess = s.SessionForHostedThread(rec.HostedThreadID)
		if sess == nil || sess.PromptStore() == nil {
			return false
		}
		return sess.Summary().TurnActive
	})
	native, turn, action := rec.NativeThreadID, "turn-human", op.TransportActionID
	text := "prompt correction"
	if _, _, err := sess.PromptStore().Submit(ctx, prompts.Submission{HostedThreadID: rec.HostedThreadID, Provider: model.ProviderCodex, NativeThreadID: native,
		ActionID: action, ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := sess.PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: rec.HostedThreadID, Token: "claim", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	binding := prompts.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}, EventID: submissionEventID(rec.HostedThreadID, action)}
	if _, err := sess.PromptStore().Accepted(ctx, prompts.Acceptance{ActionID: action, ClaimToken: "claim", AttachmentEpoch: 1, Binding: binding}); err != nil {
		t.Fatal(err)
	}

	daemon.EmitThreadNotification(native, map[string]any{"method": "item/completed", "params": map[string]any{
		"threadId": native, "turnId": turn, "item": map[string]any{"id": "final-human", "type": "agentMessage", "phase": "final_answer", "text": "done"},
	}})
	daemon.EmitThreadNotification(native, map[string]any{"method": "turn/completed", "params": map[string]any{
		"threadId": native, "turn": map[string]any{"id": turn, "status": "completed", "completedAt": 1787600100},
	}})
	waitForClaude(t, "the turn's completion recorded", func() bool {
		feed, err := completedFeedForTest(t, s.completedTurns).After(context.Background(), rec.HostedThreadID, "", 10)
		return err == nil && len(feed) == 1
	})
	if got, _ := s.agentExecutions.Turn(op.ID); threads.TurnTerminal(got.Status) {
		t.Fatalf("operation finished with the turn although its own row was never saved: %#v", got)
	}

	// Once its own row is proven saved, it finishes with the turn's recorded
	// final answer.
	if _, settled, err := sess.PromptStore().Settle(ctx, action); err != nil || !settled {
		t.Fatalf("settle: %v %v", settled, err)
	}
	daemon.EmitThreadNotification(native, map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": native, "turn": map[string]any{"id": turn, "status": "completed", "completedAt": 1787600100}}})
	waitForClaude(t, "proven correction", func() bool { got, _ := s.agentExecutions.Turn(op.ID); return got.Status == agentTurnCompleted })
	if got, _ := s.agentExecutions.Turn(op.ID); got.Status != agentTurnCompleted || got.FinalItemID != "final-human" || got.FinalResponse != "done" {
		t.Fatalf("proven operation = status %s final %q %q; want the turn's final answer", got.Status, got.FinalItemID, got.FinalResponse)
	}
}

// explicitZeroUsage reports a turn that records zero for all three counters.
func explicitZeroUsage(record coreturns.Record) bool {
	for _, value := range []*int64{record.UsageInputTokens, record.UsageCachedInputTokens, record.UsageOutputTokens} {
		if value == nil || *value != 0 {
			return false
		}
	}
	return true
}

func (h *settlementHarness) savedReader(sess *session) savedPromptReader {
	if reader := h.readers[sess]; reader != nil {
		return reader
	}
	return h.server.savedPromptsFor(sess)
}
func (h *settlementHarness) setSavedReader(sess *session, reader savedPromptReader) {
	if h.readers == nil {
		h.readers = map[*session]savedPromptReader{}
	}
	h.readers[sess] = reader
	sess.ConfigureSavedReaders(nil, reader, h.server.hostedCatalog().SavedPromptRows, h.server.hostedCatalog().SavedTurnFinal, nil)
}

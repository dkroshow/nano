package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/testsupport"
	"github.com/dkroshow/nano/internal/wire"
)

// claudeTestSession makes a session wired like a spawned one minus the spool:
// wire events land in memory, attachment writes are captured.
func claudeTestSessionConfig(t *testing.T) (sessions.Config, int64, *testsupport.AttachmentRecorder) {
	t.Helper()
	return providerSessionConfig(t, "claude")
}

func providerSessionConfig(t *testing.T, provider string) (sessions.Config, int64, *testsupport.AttachmentRecorder) {
	t.Helper()
	recorder := &testsupport.AttachmentRecorder{}
	metadata := newTestControlMetadata(t)
	migrateTestControl(t, metadata)
	const hostedID = "nht_claude_test"
	if err := metadata.EnsureHostedThread(hostedID, provider, presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	generation, err := metadata.BeginTransportBinding(hostedID)
	if err != nil {
		t.Fatal(err)
	}
	queue := mustQueuedTurnStore(t)
	attempts, err := newSendAttemptStore(queue.Database(), queue.Path(), "claude-test", testPromptStore(t, queue.Database()))
	if err != nil {
		t.Fatal(err)
	}
	turnStore, err := prompts.OpenStore(context.Background(), queue.Database())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turnStore.AdoptLegacy(context.Background()); err != nil {
		t.Fatal(err)
	}
	queue.BindTransactionStore(turnStore)
	attempts.BindTransactionStore(turnStore)
	cfg := sessions.Config{
		ID: "sess-1", Provider: provider, HostedThreadID: hostedID,
		CWD: t.TempDir(), CreatedAt: time.Now(), ProcessStarted: processStarted,
		RememberAttachment: recorder.Remember, ForgetAttachment: recorder.Forget,
		Metadata: metadata, Turns: turnStore, QueuedTurns: queue, SendAttempts: attempts,
	}
	return cfg, generation, recorder
}

func claudeTestSession(t *testing.T) (*sessions.Session, *testsupport.AttachmentRecorder) {
	cfg, generation, recorder := claudeTestSessionConfig(t)
	sess := newSession(cfg)
	if err := sess.InitializeBinding(generation); err != nil {
		t.Fatal(err)
	}
	return sess, recorder
}

func claudeWireEvents(sess *sessions.Session) []wire.Event {
	recorder := httptest.NewRecorder()
	sess.HandleEventsPage(recorder, httptest.NewRequest(http.MethodGet, "/events?beforeSeq=9223372036854775807&limit=2000", nil))
	var page struct {
		Events []wire.Event `json:"events"`
	}
	if recorder.Code != http.StatusOK {
		panic(recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		panic(err)
	}
	return page.Events
}

func waitForClaudeStatus(t *testing.T, sess *sessions.Session, want string) {
	t.Helper()
	waitForClaude(t, "session status "+want, func() bool { return sess.Summary().Status == want })
}

// waitForClaudeThreadID waits for the native id the child announces on its
// system.init. A claude session now goes "ready" as soon as its transport is up
// (docs/decisions/claude-ready-on-transport.md), so status is no longer a proxy
// for "the handshake landed" — the identity is what these assertions are about.
func waitForClaudeThreadID(t *testing.T, sess *sessions.Session, want string) {
	t.Helper()
	waitForClaude(t, "native thread id "+want, func() bool { return sess.Summary().ThreadID == want })
}

func claudeShimSpawnRequest(sess *sessions.Session, shimURL string) hostedmodel.SpawnRequest {
	return hostedmodel.SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "claude",
		Cwd:          sess.CWD(),
		Model:        "sonnet",
		ClaudeBin:    "/fake/claude",
		ClaudeShimWS: shimURL},
	}
}

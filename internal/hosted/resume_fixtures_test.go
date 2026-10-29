package hosted

import (
	"encoding/json"
	"github.com/dkroshow/nano/internal/core/prompts"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/testsupport"
	shimfixture "github.com/dkroshow/nano/internal/testsupport/claudeshim"
	shim "github.com/dkroshow/nano/internal/wire/claude"
	"path/filepath"
	"testing"
	"time"
)

const revivedNativeID = "f2000067-f9db-51d0-919d-079cc251e891"

func newOwnerRegistry() *sessions.Registry { r := sessions.NewRegistry(); return &r }
func newTestAttachmentOwner(sources AttachmentSources) *AttachmentCoordinator {
	if sources.Registry == nil {
		sources.Registry = newOwnerRegistry()
	}
	if sources.Boot == nil {
		sources.Boot = &sessions.BootAttachments{}
	}
	if sources.Runtime == nil {
		sources.Runtime = sidebar.New(sidebar.Sources{Registry: sources.Registry, Executions: sources.Executions, Metadata: sources.Metadata, Attachments: sources.Attachments, Boot: sources.Boot})
	}
	return NewAttachmentCoordinator(sources)
}
func newResumeCoordinator(t *testing.T) (*AttachmentCoordinator, *hostedstore.AttachmentStore) {
	t.Helper()
	s, store := newBootCoordinator(t, "", "")
	return s.AttachmentCoordinator, store
}
func newTestStore(t *testing.T) *hostedstore.AttachmentStore {
	t.Helper()
	s, e := hostedstore.NewAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func ownerSessionCount(store *hostedstore.ExecutionStore) int {
	rows, e := store.Control().CanonicalDatabase().Query(`SELECT hosted_thread_id FROM hosted_execution_sessions`)
	if e != nil {
		panic(e)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	if rows.Err() != nil {
		panic(rows.Err())
	}
	return count
}

func readyClaimSession(t *testing.T, cfg sessions.Config, native string) *sessions.Session {
	t.Helper()
	shimServer := shimfixture.NewServer(t, func(c *shimfixture.Conn) {
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			if call.Method == shim.MethodStart {
				c.Reply(call, map[string]any{"shimSessionId": "claim-live"})
				line, _ := json.Marshal(map[string]any{"type": "system", "subtype": "init", "session_id": native, "model": "test-model"})
				c.Line(1, "recv", string(line))
			} else {
				c.Reply(call, map[string]any{})
			}
		}
	})

	queue := testsupport.MustQueue(t)
	cfg.Turns = testsupport.PromptStore(t, queue.Database())
	cfg.QueuedTurns = queue
	journal, err := prompts.NewLegacyJournal(queue.Database(), queue.Path(), "claim-fixture", cfg.Turns)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SendAttempts = journal
	queue.BindTransactionStore(cfg.Turns)
	journal.BindTransactionStore(cfg.Turns)
	sess := sessions.New(cfg)
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	sess.ReserveNativeID(native)
	if err := sess.Runtime().BeginProviderSpawn(sessions.ProviderRequest{Provider: "claude", Cwd: t.TempDir(), ClaudeShimWS: shimServer.WSURL(), ClaudeShimSpools: t.TempDir(), ClaudeBin: "/fake/claude"}, nil, func() {}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.Close)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sess.Summary().Status == "ready" {
			return sess
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("live claim fixture did not become ready: %+v", sess.Summary())
	return nil
}

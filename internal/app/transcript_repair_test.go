package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// repairCatalog stands in for the attached-file catalog: each repair's fresh
// catch-up runs wait, which says whether the saved file was proven current.
type repairCatalog struct {
	waits    atomic.Int32
	wait     func(cancel <-chan struct{}) bool
	provider *fakeAppServer
}

func (*repairCatalog) BindAttachedSource(string, string, string, string, string, bool) {}

func (*repairCatalog) ReleaseAttachedSource(string) {}

func (c *repairCatalog) AwaitAttachedCurrent(_ string, fresh bool, _ func() bool, cancel <-chan struct{}) bool {
	c.waits.Add(1)
	if !fresh {
		return false
	}
	return c.wait(cancel)
}

func repairSession(t *testing.T, catalog *repairCatalog) *sessions.Session {
	t.Helper()
	dir := t.TempDir()
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "attempts.json"), "run")
	if err != nil {
		t.Fatal(err)
	}
	store := mustBindTransactionalTurnStore(t, queue, attempts)
	fake := newFakeAppServer(t)
	sess := resumeCodexFixture(t, sessions.Config{ID: "repair-session", Provider: "codex", HostedThreadID: "hosted",
		Projection: newLiveProjection("codex", "hosted"), QueuedTurns: queue, SendAttempts: attempts, Turns: store}, "thread", fake)
	sess.ConfigureRegistration(nil, nil, false, catalog)
	catalog.provider = fake
	return sess
}

func postRepair(t *testing.T, s *server, sess *sessions.Session, action string) (int, sessions.TranscriptRepairResponse) {
	t.Helper()
	recorder := httptest.NewRecorder()
	sess.HandleTranscriptRepair(recorder, httptest.NewRequest(http.MethodPost, "/api/sessions/"+sess.ID()+"/transcript-repair",
		bytes.NewBufferString(`{"clientActionId":"`+action+`"}`)))
	var response sessions.TranscriptRepairResponse
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
	}
	return recorder.Code, response
}

// The explicit Stop/discard/repair route reports repaired only once this
// attachment's saved file is proven current and its prompts settle, with the
// attachment it stopped still idle and ready; it always returns the queued
// prompts it discarded, and a retry returns the same result without another
// Stop. An attachment replaced during the catch-up or settlement is stale,
// whether or not they succeeded: never success, never a saved-file diagnosis.
// Boundary: POST /api/sessions/{id}/transcript-repair with a stand-in catalog.
func TestTranscriptRepairReportsRepairedOnlyAfterTheSavedFileCatchesUp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wait   func(*sessions.Session) func(<-chan struct{}) bool
		setup  func(*testing.T, *sessions.Session, *repairCatalog)
		status string
	}{
		{"caught up and settled", func(*sessions.Session) func(<-chan struct{}) bool {
			return func(<-chan struct{}) bool { return true }
		}, nil, "repaired"},
		{"file not proven current", func(*sessions.Session) func(<-chan struct{}) bool {
			return func(<-chan struct{}) bool { return false }
		}, nil, "saved_copy_not_ready"},
		{"saved read fails", func(*sessions.Session) func(<-chan struct{}) bool {
			return func(<-chan struct{}) bool { return true }
		}, func(t *testing.T, sess *sessions.Session, _ *repairCatalog) {
			seedAcceptedTransactionalTurn(t, sess.PromptStore(), "hosted", model.ProviderCodex, "thread", "delivered", "turn-1", "delivered prompt")
			sess.ConfigureSavedReaders(nil, func(context.Context, history.SavedUserKeys) ([]history.SavedUserRow, error) {
				return nil, errors.New("synthetic saved read failure")
			}, nil, nil, nil)
		}, "saved_copy_not_ready"},
		{"attachment replaced during the catch-up", func(sess *sessions.Session) func(<-chan struct{}) bool {
			return func(<-chan struct{}) bool {
				if err := sess.InitializeBinding(4); err != nil {
					panic(err)
				}
				return true
			}
		}, nil, "stale_attachment"},
		{"attachment replaced during a failed catch-up", func(sess *sessions.Session) func(<-chan struct{}) bool {
			return func(<-chan struct{}) bool {
				if err := sess.InitializeBinding(4); err != nil {
					panic(err)
				}
				return false
			}
		}, nil, "stale_attachment"},
		{"attachment replaced during a failed settlement", func(*sessions.Session) func(<-chan struct{}) bool {
			return func(<-chan struct{}) bool { return true }
		}, func(t *testing.T, sess *sessions.Session, _ *repairCatalog) {
			seedAcceptedTransactionalTurn(t, sess.PromptStore(), "hosted", model.ProviderCodex, "thread", "delivered", "turn-1", "delivered prompt")
			sess.ConfigureSavedReaders(nil, func(context.Context, history.SavedUserKeys) ([]history.SavedUserRow, error) {
				if err := sess.InitializeBinding(4); err != nil {
					panic(err)
				}
				return nil, errors.New("synthetic saved read failure")
			}, nil, nil, nil)
		}, "stale_attachment"},
		{"a question is pending", func(*sessions.Session) func(<-chan struct{}) bool {
			return func(<-chan struct{}) bool { return true }
		}, func(t *testing.T, sess *sessions.Session, catalog *repairCatalog) {
			fake := catalog.provider
			fake.EmitThreadNotification("thread", map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn-1", "status": "inProgress"}}})
			waitReconnectCondition(t, func() bool { return sess.Summary().TurnActive })
			fake.OnTurnInterrupt = func() {
				fake.EmitThreadNotification("thread", map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn-1", "status": "interrupted"}}})
			}
			fake.EmitThreadNotification("thread", map[string]any{"id": 42, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "thread", "turnId": "turn-1", "itemId": "q", "command": "pwd"}})
			waitReconnectCondition(t, func() bool { return len(sess.PendingInteractionList()) > 0 })
		}, "saved_copy_not_ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := &repairCatalog{}
			sess := repairSession(t, catalog)
			catalog.wait = tc.wait(sess)
			if tc.setup != nil {
				tc.setup(t, sess, catalog)
			}
			const text = "queued prompt"
			if _, _, err := sess.PromptStore().Submit(context.Background(), prompts.Submission{HostedThreadID: "hosted", Provider: model.ProviderCodex,
				NativeThreadID: "thread", ActionID: "queued", ProviderText: text, PresentationText: text,
				TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: prompts.HumanOrigin}); err != nil {
				t.Fatal(err)
			}
			s := &server{}
			code, response := postRepair(t, s, sess, "repair")
			if code != http.StatusOK || response.Status != tc.status || response.Repaired != (tc.status == "repaired") {
				t.Fatalf("repair = %d %+v, want %s", code, response, tc.status)
			}
			if len(response.Discarded) != 1 || response.Discarded[0].ClientActionID != "queued" || response.Discarded[0].Text != text {
				t.Fatalf("discarded prompts = %+v", response.Discarded)
			}
			if record, _, err := sess.PromptStore().Get(context.Background(), "queued"); err != nil || record.Outbox.State != prompts.Refused {
				t.Fatalf("queued prompt = %s, %v; want refused", record.Outbox.State, err)
			}
			// A retry of the same action returns the same answer without a
			// second Stop or catch-up; on a replaced attachment it is refused.
			if tc.status != "stale_attachment" {
				if _, again := postRepair(t, s, sess, "repair"); again.Status != response.Status || catalog.waits.Load() != 1 {
					t.Fatalf("retry = %+v after %d catch-ups", again, catalog.waits.Load())
				}
				if err := sess.InitializeBinding(4); err != nil {
					panic(err)
				}
			}
			if code, _ := postRepair(t, s, sess, "repair"); code != http.StatusConflict || catalog.waits.Load() != 1 {
				t.Fatalf("cached result on a new attachment = %d after %d catch-ups, want 409", code, catalog.waits.Load())
			}
		})
	}
}

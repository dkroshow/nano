package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// repairCatalog stands in for the attached-file catalog: each repair's fresh
// catch-up runs wait, which says whether the saved file was proven current.
type repairCatalog struct {
	waits atomic.Int32
	wait  func(cancel <-chan struct{}) bool
}

func (*repairCatalog) bindAttachedSource(string, string, string, string, string, bool) {}
func (*repairCatalog) releaseAttachedSource(string)                                    {}
func (c *repairCatalog) awaitAttachedCurrent(_ string, fresh bool, _ func() bool, cancel <-chan struct{}) bool {
	c.waits.Add(1)
	if !fresh {
		return false
	}
	return c.wait(cancel)
}

func repairSession(t *testing.T, catalog *repairCatalog) *session {
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
	return &session{id: "repair-session", provider: "codex", threadID: "thread", hostedThreadID: "hosted", attachmentGeneration: 3,
		projection: newLiveProjection("codex", "hosted"), queuedTurns: queue, turns: store, catalogSources: catalog,
		lifecycleState: testLifecycleSnapshot("ready", false)}
}

func postRepair(t *testing.T, s *server, sess *session, action string) (int, transcriptRepairResponse) {
	t.Helper()
	recorder := httptest.NewRecorder()
	s.handleTranscriptRepair(recorder, httptest.NewRequest(http.MethodPost, "/api/sessions/"+sess.id+"/transcript-repair",
		bytes.NewBufferString(`{"clientActionId":"`+action+`"}`)), sess)
	var response transcriptRepairResponse
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
		wait   func(*session) func(<-chan struct{}) bool
		setup  func(*testing.T, *session)
		status string
	}{
		{"caught up and settled", func(*session) func(<-chan struct{}) bool {
			return func(<-chan struct{}) bool { return true }
		}, nil, "repaired"},
		{"file not proven current", func(*session) func(<-chan struct{}) bool {
			return func(<-chan struct{}) bool { return false }
		}, nil, "saved_copy_not_ready"},
		{"saved read fails", func(*session) func(<-chan struct{}) bool {
			return func(<-chan struct{}) bool { return true }
		}, func(t *testing.T, sess *session) {
			seedAcceptedTransactionalTurn(t, sess.turns, "hosted", model.ProviderCodex, "thread", "delivered", "turn-1", "delivered prompt")
			sess.savedPrompts = func(context.Context, history.SavedUserKeys) ([]history.SavedUserRow, error) {
				return nil, errors.New("synthetic saved read failure")
			}
		}, "saved_copy_not_ready"},
		{"attachment replaced during the catch-up", func(sess *session) func(<-chan struct{}) bool {
			return func(<-chan struct{}) bool {
				sess.stateMu.Lock()
				sess.attachmentGeneration++
				sess.stateMu.Unlock()
				return true
			}
		}, nil, "stale_attachment"},
		{"attachment replaced during a failed catch-up", func(sess *session) func(<-chan struct{}) bool {
			return func(<-chan struct{}) bool {
				sess.stateMu.Lock()
				sess.attachmentGeneration++
				sess.stateMu.Unlock()
				return false
			}
		}, nil, "stale_attachment"},
		{"attachment replaced during a failed settlement", func(*session) func(<-chan struct{}) bool {
			return func(<-chan struct{}) bool { return true }
		}, func(t *testing.T, sess *session) {
			seedAcceptedTransactionalTurn(t, sess.turns, "hosted", model.ProviderCodex, "thread", "delivered", "turn-1", "delivered prompt")
			sess.savedPrompts = func(context.Context, history.SavedUserKeys) ([]history.SavedUserRow, error) {
				sess.stateMu.Lock()
				sess.attachmentGeneration++
				sess.stateMu.Unlock()
				return nil, errors.New("synthetic saved read failure")
			}
		}, "stale_attachment"},
		{"a question is pending", func(*session) func(<-chan struct{}) bool {
			return func(<-chan struct{}) bool { return true }
		}, func(_ *testing.T, sess *session) {
			sess.interactions = map[string]pendingInteraction{"q": {ID: "q"}}
		}, "saved_copy_not_ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := &repairCatalog{}
			sess := repairSession(t, catalog)
			catalog.wait = tc.wait(sess)
			if tc.setup != nil {
				tc.setup(t, sess)
			}
			const text = "queued prompt"
			if _, _, err := sess.turns.Submit(context.Background(), prompts.Submission{HostedThreadID: "hosted", Provider: model.ProviderCodex,
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
			if record, _, err := sess.turns.Get(context.Background(), "queued"); err != nil || record.Outbox.State != prompts.Refused {
				t.Fatalf("queued prompt = %s, %v; want refused", record.Outbox.State, err)
			}
			// A retry of the same action returns the same answer without a
			// second Stop or catch-up; on a replaced attachment it is refused.
			if tc.status != "stale_attachment" {
				if _, again := postRepair(t, s, sess, "repair"); again.Status != response.Status || catalog.waits.Load() != 1 {
					t.Fatalf("retry = %+v after %d catch-ups", again, catalog.waits.Load())
				}
				sess.stateMu.Lock()
				sess.attachmentGeneration++
				sess.stateMu.Unlock()
			}
			if code, _ := postRepair(t, s, sess, "repair"); code != http.StatusConflict || catalog.waits.Load() != 1 {
				t.Fatalf("cached result on a new attachment = %d after %d catch-ups, want 409", code, catalog.waits.Load())
			}
		})
	}
}

// A Stop that never reaches an idle provider fails the action before any
// catch-up, so no repair is claimed. Boundary: the repair action.
func TestTranscriptRepairFailsWhenStopDoesNotReachIdle(t *testing.T) {
	catalog := &repairCatalog{wait: func(<-chan struct{}) bool { return true }}
	sess := repairSession(t, catalog)
	sess.lifecycleState = testLifecycleSnapshot("ready", true)
	_, err := (&server{}).stopDiscardAndRepairWithin(context.Background(), sess, "repair", 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "stop did not reach an idle provider boundary") || catalog.waits.Load() != 0 {
		t.Fatalf("stop failure = %v after %d catch-ups", err, catalog.waits.Load())
	}
}

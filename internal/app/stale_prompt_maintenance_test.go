package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Old stuck prompts must clear without losing or mislabelling any: what the
// saved transcript proves becomes confirmed, only the rest expire, and nothing
// expires that a running turn, an unreadable saved transcript or a recent
// submission still governs. Nothing changes before a backup or on a stale
// preview, and an expired prompt is no longer published, listed or shown.
// Boundary: the maintenance endpoint on a real catalog of each saved format.
func TestStalePromptMaintenanceSettlesFirstThenExpiresTheRest(t *testing.T) {
	var idle *session
	for _, test := range []struct {
		name     string
		provider string
		stale    []string // prompts submitted more than a day ago
		// setup may attach the fixture thread; it runs before the preview.
		setup     func(t *testing.T, h *settlementHarness)
		after     func(t *testing.T)
		candidate int
		want      map[string]turns.DeliveryState
	}{
		{
			// Attached idle thread with an open pane: what expires leaves the
			// pane's live text at once, not when its loop next starts.
			name: "attached idle", provider: "codex", stale: []string{"running", "unproven", "unanswered"}, candidate: 3,
			setup: func(t *testing.T, h *settlementHarness) {
				idle = h.attach("codex", nil)
				idle.liveText = newLiveTextLoop(h.server, idle)
				liveTextOf(idle).running = true // the test runs the loop's steps itself
				liveTextOf(idle).reset()
				liveTextOf(idle).seedOutbox()
				if live := liveTextOf(idle).visible(); len(live) == 0 {
					t.Fatal("the open pane shows none of the stale prompts")
				}
				h.server.sessions = map[string]*session{idle.id: idle}
			},
			after: func(t *testing.T) {
				l := liveTextOf(idle)
				l.process(l.inbox)
				if live := l.visible(); len(live) != 0 {
					t.Fatalf("live text after expiry = %+v, want none", live)
				}
			},
			want: map[string]turns.DeliveryState{"running": turns.Expired, "unproven": turns.Expired, "unanswered": turns.Expired},
		},
		{
			// Detached thread: the saved transcript proves old and new; the
			// rest expire. A busy thread and a recent prompt stay.
			name: "detached", provider: "codex",
			stale: []string{"old", "new", "running", "unproven", "unanswered", "busy"}, candidate: 5,
			setup: func(t *testing.T, h *settlementHarness) {
				seedAcceptedTransactionalTurn(t, h.prompts, "busy", model.ProviderCodex, "busy-native", "busy", "turn-busy", "busy prompt")
				seedAcceptedTransactionalTurn(t, h.prompts, "hosted", model.ProviderCodex, h.fixture.native, "recent", "turn-recent", "recent prompt")
				h.server.sessions = map[string]*session{"busy-session": {id: "busy-session", provider: "codex", hostedThreadID: "busy", threadID: "busy-native",
					lifecycleState: testLifecycleSnapshot("ready", true)}}
			},
			want: map[string]turns.DeliveryState{"old": turns.Confirmed, "new": turns.Confirmed, "running": turns.Expired,
				"unproven": turns.Expired, "unanswered": turns.Expired, "busy": turns.Accepted, "recent": turns.Accepted},
		},
		{
			// Detached Claude thread whose only stale prompt was bound to the
			// shim handle: the newer prompts name the real session it proves in.
			name: "detached provisional", provider: "claude", stale: []string{"old"}, candidate: 1,
			want: map[string]turns.DeliveryState{"old": turns.Confirmed},
		},
		{
			// Attached thread whose saved transcript cannot be read: nothing
			// proves or disproves its prompts, so none expire.
			name: "attached unreadable", provider: "codex", stale: []string{"running", "unproven", "unanswered"}, candidate: 3,
			setup: func(t *testing.T, h *settlementHarness) {
				sess := h.attach("codex", nil)
				sess.savedPrompts = func(context.Context, history.SavedUserKeys) ([]history.SavedUserRow, error) {
					return nil, errors.New("catalog read failed")
				}
				h.server.sessions = map[string]*session{sess.id: sess}
			},
			want: map[string]turns.DeliveryState{"running": turns.Accepted, "unproven": turns.Accepted, "unanswered": turns.Uncertain},
		},
		{
			// Attached thread whose turn starts while its saved rows are read:
			// the running turn keeps its prompts.
			name: "attached turn starts", provider: "codex", stale: []string{"running", "unproven", "unanswered"}, candidate: 3,
			setup: func(t *testing.T, h *settlementHarness) {
				sess := h.attach("codex", nil)
				saved := sess.savedPrompts
				sess.savedPrompts = func(ctx context.Context, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
					sess.stateMu.Lock()
					sess.lifecycleState = testLifecycleSnapshot("ready", true)
					sess.stateMu.Unlock()
					return saved(ctx, keys)
				}
				h.server.sessions = map[string]*session{sess.id: sess}
			},
			want: map[string]turns.DeliveryState{"running": turns.Accepted, "unproven": turns.Accepted, "unanswered": turns.Uncertain},
		},
		{
			// Attached thread that starts a delivery while its saved rows are
			// read: the provider has not answered, so no turn is marked
			// running yet, but the thread is busy.
			name: "attached delivery starts", provider: "codex", stale: []string{"running", "unproven", "unanswered"}, candidate: 3,
			setup: func(t *testing.T, h *settlementHarness) {
				release := make(chan struct{})
				sess := h.attach("codex", func(context.Context, string) (json.RawMessage, error) {
					<-release
					return json.RawMessage(`{"turn":{"id":"turn-later"}}`), nil
				})
				t.Cleanup(func() {
					close(release)
					sess.sendMu.Lock()
					op := sess.sendOps["later"]
					sess.sendMu.Unlock()
					if op != nil {
						<-op.done // finish before the stores close
					}
				})
				saved := sess.savedPrompts
				sess.savedPrompts = func(ctx context.Context, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
					go postTurn(t, sess, context.Background(), "later", "later prompt")
					for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
						sess.sendMu.Lock()
						sending := sess.sendOps["later"] != nil
						sess.sendMu.Unlock()
						if sending {
							break
						}
					}
					return saved(ctx, keys)
				}
				h.server.sessions = map[string]*session{sess.id: sess}
			},
			want: map[string]turns.DeliveryState{"running": turns.Accepted, "unproven": turns.Accepted, "unanswered": turns.Uncertain},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newSettlementHarness(t, test.provider)
			h.server.turns, h.server.queuedTurns = h.prompts, h.queue
			if test.setup != nil {
				test.setup(t, h)
			}
			for _, action := range test.stale {
				if _, err := h.queue.db.Exec(`UPDATE queued_turns SET created_at='2026-09-01T00:00:00Z' WHERE client_action_id=?`, action); err != nil {
					t.Fatal(err)
				}
			}
			post := func(body map[string]any) (int, stalePromptReceipt) {
				t.Helper()
				encoded, _ := json.Marshal(body)
				rec := httptest.NewRecorder()
				h.server.handleStalePrompts(rec, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/stale-prompts", bytes.NewReader(encoded)))
				var receipt stalePromptReceipt
				if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
					t.Fatalf("body %s: %v", rec.Body.String(), err)
				}
				return rec.Code, receipt
			}
			state := func(action string) turns.Record {
				t.Helper()
				record, _, err := h.prompts.Get(context.Background(), action)
				if err != nil {
					t.Fatal(err)
				}
				return record
			}

			code, preview := post(map[string]any{"apply": false})
			if code != http.StatusOK || preview.Candidates != test.candidate {
				t.Fatalf("preview = %d %+v", code, preview)
			}
			apply := map[string]any{"apply": true, "cutoff": preview.Cutoff, "expectedFingerprint": "stale", "clientActionId": "test:expire:1"}
			if code, refused := post(apply); code != http.StatusConflict || refused.Applied || refused.BackupPath != "" {
				t.Fatalf("apply on a stale fingerprint = %d %+v", code, refused)
			}
			apply["expectedFingerprint"] = preview.Fingerprint
			code, applied := post(apply)
			if code != http.StatusOK || !applied.Applied {
				t.Fatalf("apply = %d %+v", code, applied)
			}
			if info, err := os.Stat(applied.BackupPath); err != nil || info.Size() == 0 {
				t.Fatalf("backup %q: %v", applied.BackupPath, err)
			}
			var expired []string
			for action, want := range test.want {
				if record := state(action); record.Outbox.State != want || record.ProviderText != action+" prompt" {
					t.Fatalf("%s = %q (text kept %v), want %q; receipt %+v", action, record.Outbox.State, record.ProviderText != "", want, applied)
				}
				if want == turns.Expired {
					expired = append(expired, action)
				}
			}
			unsettled, err := h.prompts.Unsettled(context.Background(), "hosted")
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range unsettled {
				if slices.Contains(expired, entry.ActionID) {
					t.Fatalf("expired %s is still published", entry.ActionID)
				}
			}
			for _, attempt := range h.attempts.ListThread(test.provider, h.fixture.native) {
				if slices.Contains(expired, attempt.ClientActionID) {
					t.Fatalf("delivery list still shows expired %s", attempt.ClientActionID)
				}
			}
			if test.after != nil {
				test.after(t)
			}
		})
	}
}

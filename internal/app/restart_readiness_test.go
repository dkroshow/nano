package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// restartWindow is one Codex thread attached through the real boot path to a
// fake app server, with its rollout indexed by a real catalog that the test
// can pause. A paused catalog leaves the attachment's catch-up pending.
type restartWindow struct {
	s       *server
	fake    *fakeAppServer
	rec     attachmentRecord
	catalog *threadCatalogService
	path    string
	// failReads makes every catalog read of the rollout fail; failedReads
	// counts them. failSavedRows makes the settlement read of saved prompts
	// fail.
	failReads     atomic.Bool
	failedReads   atomic.Int32
	failSavedRows atomic.Bool
}

// failingSavedRows is the catalog with a settlement read that can fail.
type failingSavedRows struct {
	*threadCatalogService
	fail *atomic.Bool
}

func (c failingSavedRows) LoadSavedUserRows(ctx context.Context, threadID string, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
	if c.fail.Load() {
		return nil, errors.New("saved rows unreadable")
	}
	return c.threadCatalogService.LoadSavedUserRows(ctx, threadID, keys)
}

func newRestartWindow(t *testing.T) *restartWindow {
	t.Helper()
	ctx := context.Background()
	fake := newFakeAppServer(t)
	s, store := bootReattachServer(t, "", fake.wsURL())
	rec := bootCodexRecord(t)
	root := t.TempDir()
	w := &restartWindow{s: s, fake: fake, path: filepath.Join(root, "rollout-"+rec.NativeThreadID+".jsonl")}
	writeSettlementFile(t, w.path, fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":"/disposable","timestamp":"2026-10-08T17:00:00Z"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-earlier"}}
{"type":"event_msg","payload":{"type":"user_message","message":"earlier prompt"}}
{"type":"response_item","payload":{"type":"message","id":"earlier-answer","role":"assistant","content":[{"type":"output_text","text":"earlier answer"}]}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-earlier"}}
`, rec.NativeThreadID))
	historyStore, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { historyStore.Close() })
	w.catalog, err = newThreadCatalogService(ctx, historyStore, threadCatalogServiceOptions{
		Roots: []history.SourceRoot{{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: root}},
		projectSource: func(ctx context.Context, candidate history.SourceCandidate, stored history.StoredSource, exists bool,
			header history.TranscriptCheckpoint, hasCheckpoint bool, at time.Time) (history.SourceProjection, *preparedTranscript, error) {
			if w.failReads.Load() && candidate.Path == w.path {
				w.failedReads.Add(1)
				return history.SourceProjection{}, nil, errors.New("rollout read failed")
			}
			return w.catalog.projectSource(ctx, candidate, stored, exists, header, hasCheckpoint, at)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.catalog.Close() })
	w.catalog.RequestRefresh()
	waitForCatalogWithin(t, time.Minute, func() bool {
		id, err := w.catalog.ResolveThreadReference(ctx, rec.NativeThreadID)
		return err == nil && id != ""
	})
	s.catalog, s.catalogSources = failingSavedRows{w.catalog, &w.failSavedRows}, w.catalog
	w.catalog.setTranscriptChanged(s.confirmDurableThread)
	store.remember(rec)
	seedBootAttachments(t, s)
	w.rec = store.records()[0]
	return w
}

// pause holds the catalog's worker, so no file is read until resume.
func (w *restartWindow) pause() func() {
	w.catalog.projectionMu.Lock()
	var once bool
	return func() {
		if !once {
			once = true
			w.catalog.projectionMu.Unlock()
		}
	}
}

// appendSaved writes rows Codex saved; a held file's watch queues the read.
func (w *restartWindow) appendSaved(t *testing.T, rows string) {
	t.Helper()
	file, err := os.OpenFile(w.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(rows); err != nil {
		t.Fatal(err)
	}
	file.Close()
	w.catalog.RequestPathRefresh([]string{w.path})
}

// savedPromptTurn is what Codex saves of a turn that took the prompt action
// and was then interrupted.
func savedPromptTurn(hosted, action, turn string) string {
	return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%[2]q}}
{"type":"response_item","payload":{"type":"message","id":"%[1]s-message","role":"user","content":[{"type":"input_text","text":"%[1]s prompt"}],"internal_chat_message_metadata_passthrough":{"turn_id":%[2]q}}}
{"type":"event_msg","payload":{"type":"item_completed","turn_id":%[2]q,"item":{"type":"UserMessage","id":"%[1]s-item","client_id":%[3]q,"content":[{"type":"text","text":"%[1]s prompt"}]}}}
{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":%[2]q,"reason":"interrupted"}}
`, action, turn, submissionEventID(hosted, action))
}

func (w *restartWindow) state(t *testing.T, action string) turns.DeliveryState {
	t.Helper()
	record, _, err := w.s.turns.Get(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	return record.Outbox.State
}

// The restart-window regression (seen 2026-10-08: a thread read ready 17:03:32Z
// while its restore ran to 17:04:01Z, then a prompt resent at 17:05:54Z). A
// person's prompt that Codex took and saved while Nano could not see it must
// never be sent again, and a prompt queued during the window is sent exactly
// once. The thread is not ready, and sends nothing, until its saved file is
// proven current: after a Nano restart, after a provider re-attach, through a
// catalog pass that fails until its retry succeeds, and while the file is
// missing. Only the provider's word that the thread was never prompted lets
// its first prompt go without that proof. The interruption reaches Nano
// either in the resume answer or as a live notification that crosses ahead
// of it.
// Boundary: prompt delivery through the real boot and reconnect handshakes, a
// real catalog paused by the test, and the real prompt store.
func TestRestartWindowNeverResendsAndSendsTheQueuedPromptOnce(t *testing.T) {
	for _, test := range []struct {
		name     string
		reattach bool // the gap is a provider re-attach, not a Nano restart
		ahead    bool // the interruption arrives live, ahead of a stale resume answer
		failing  bool // the catalog's reads of the rollout fail until the test stops them
		linkLost bool // the app-server connection drops while the saved file catches up
		unread   bool // the settlement read of saved prompts fails until the test stops it
		missing  bool // the rollout is absent until the test writes it
		early    bool // ... and indexed before the resume answer arrives
		unused   bool // Codex answers that the thread has no rollout: never prompted
	}{
		{name: "nano restart/interrupted resume answer"},
		{name: "nano restart/interruption ahead of the resume answer", ahead: true},
		{name: "provider re-attach/interrupted resume answer", reattach: true},
		{name: "provider re-attach/interruption ahead of the resume answer", reattach: true, ahead: true},
		{name: "nano restart/catch-up pass fails, then its retry succeeds", failing: true},
		{name: "nano restart/link lost while the saved file catches up", linkLost: true},
		{name: "nano restart/saved prompts unreadable, then read", unread: true},
		{name: "nano restart/saved file missing until written", missing: true},
		{name: "nano restart/saved file written before readiness waits", missing: true, early: true},
		{name: "nano restart/never prompted", unused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			w := newRestartWindow(t)
			hosted, native, turn := w.rec.HostedThreadID, w.rec.NativeThreadID, "turn-p1"
			resumeTurns := `[{"id":"turn-p1","status":"interrupted","items":[]}]`
			if test.ahead {
				resumeTurns = `[{"id":"turn-p1","status":"inProgress","items":[]}]`
			}
			// The resume answer waits, so a prompt can be admitted first.
			delay := 300 * time.Millisecond
			if test.early {
				delay = 3 * time.Second // time for the file to be written and indexed
			}
			w.fake.setReplyDelay("thread/resume", delay)
			var sess *session
			release := func() {}
			sentBefore := []string{}
			resumes := 0 // resume requests before the gap's own

			switch {
			case test.missing || test.unused:
				if err := os.Remove(w.path); err != nil {
					t.Fatal(err)
				}
				if test.unused {
					w.fake.setResumeError("no rollout found for thread id " + native)
				}
				go func() {
					w.s.reattachOnBoot()
					w.s.reattachWaitingHosted(hosted) // no work open, so it rests until needed
				}()
				waitForClaude(t, "the boot shell", func() bool {
					sess = w.s.sessionForHostedThread(hosted)
					return sess != nil && sess.turns != nil
				})
			case !test.reattach:
				// p1 was accepted into turn-p1 before Nano stopped; Codex
				// saved it and the turn's interruption while Nano was down.
				text := "p1 prompt"
				if _, _, err := w.s.turns.Submit(ctx, turns.Submission{HostedThreadID: hosted, Provider: model.ProviderCodex, NativeThreadID: native, ActionID: "p1",
					ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: turns.HumanOrigin}); err != nil {
					t.Fatal(err)
				}
				if _, claimed, err := w.s.turns.Claim(ctx, turns.ClaimRequest{HostedThreadID: hosted, Token: "before-restart", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
					t.Fatalf("claim p1 = %v, %v", claimed, err)
				}
				binding := turns.Binding{EventID: submissionEventID(hosted, "p1"), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}}
				if _, err := w.s.turns.Accepted(ctx, turns.Acceptance{ActionID: "p1", ClaimToken: "before-restart", AttachmentEpoch: 1, Binding: binding}); err != nil {
					t.Fatal(err)
				}
				release = w.pause()
				w.appendSaved(t, savedPromptTurn(hosted, "p1", turn))
				w.failReads.Store(test.failing)
				w.failSavedRows.Store(test.unread)
				w.fake.setResumeTurns(json.RawMessage(resumeTurns))
				go w.s.reattachOnBoot()
				waitForClaude(t, "the boot shell", func() bool {
					sess = w.s.sessionForHostedThread(hosted)
					return sess != nil && sess.turns != nil
				})
			default:
				w.s.reattachOnBoot()
				w.s.reattachWaitingHosted(hosted) // no work open, so it rests until needed
				waitForClaude(t, "the first attachment", func() bool {
					sess = w.s.sessionForHostedThread(hosted)
					return sess != nil && sess.summary().Status == "ready"
				})
				// p1 starts turn-p1 on the first attachment. The link drops
				// before Codex's echo of p1 arrives; in the gap Codex saves p1
				// and the turn's interruption.
				w.fake.setTurnStartResult(map[string]any{"turn": map[string]any{"id": turn}})
				if rec := postTurn(t, sess, ctx, "p1", "p1 prompt"); rec.Code != http.StatusOK {
					t.Fatalf("p1 delivery = %d %s", rec.Code, rec.Body.String())
				}
				sentBefore = []string{"p1 prompt"}
				w.fake.emitThreadNotification(native, map[string]any{"method": "turn/started", "params": map[string]any{
					"threadId": native, "turn": map[string]any{"id": turn, "status": "inProgress", "items": []any{}}}})
				waitForClaude(t, "turn-p1 working", func() bool {
					_, running := sess.activeTurn()
					return running == turn
				})
				release = w.pause()
				w.appendSaved(t, savedPromptTurn(hosted, "p1", turn))
				w.fake.setResumeTurns(json.RawMessage(resumeTurns))
				resumes = w.fake.resumeCount()
				w.fake.dropConnections()
				waitForClaude(t, "the link loss", func() bool { return sess.summary().Status == "reconnecting" })
			}
			defer release()
			if test.ahead {
				go func() {
					waitForClaude(t, "the resume request", func() bool { return w.fake.resumeCount() > resumes })
					w.fake.emitThreadNotification(native, map[string]any{"method": "turn/completed", "params": map[string]any{
						"threadId": native, "turn": map[string]any{"id": turn, "status": "interrupted"}}})
				}()
			}

			// p2 is typed while the thread cannot take it yet.
			w.fake.setTurnStartResult(map[string]any{"turn": map[string]any{"id": "turn-p2"}})
			if rec := postTurn(t, sess, ctx, "p2", "p2 prompt"); rec.Code != http.StatusAccepted {
				t.Fatalf("p2 admission = %d %s; want queued", rec.Code, rec.Body.String())
			}
			notReady := func(why string) {
				t.Helper()
				for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
					if status, sent := sess.summary().Status, w.fake.startedTexts(); status == "ready" || fmt.Sprint(sent) != fmt.Sprint(sentBefore) {
						t.Fatalf("%s: status %s, sent %q", why, status, sent)
					}
				}
			}
			writeRollout := func() {
				writeSettlementFile(t, w.path, fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":"/disposable","timestamp":"2026-10-08T17:00:00Z"}}`+"\n", native))
				w.catalog.RequestPathRefresh([]string{w.path})
			}
			switch {
			case test.early:
				// The pass that finds no file, then the one that indexes it,
				// both land before readiness starts to wait.
				var catchUp *attachedCatchUp
				waitForClaude(t, "the pass that found no file", func() bool {
					catchUp = w.catalog.attachedCatchUpFor(sess.id)
					return catchUp != nil && w.catalog.passedSince(catchUp.after)
				})
				writeRollout()
				waitForClaude(t, "the written file indexed", func() bool {
					select {
					case <-catchUp.done:
						return true
					default:
						return false
					}
				})
				if status := sess.summary().Status; status != "starting" {
					t.Fatalf("the resume answer was processed before the file was indexed (status %s)", status)
				}
			case !test.unused:
				// Attached, but its saved file is not proven current: not
				// ready, and p2 waits.
				waitForClaude(t, "the resume answer", func() bool { return w.fake.resumeCount() > resumes })
				notReady("before the saved file caught up")
				if test.missing {
					writeRollout()
				}
				if test.linkLost {
					// The wait gives way to the reconnect, which waits in turn.
					lost := w.fake.resumeCount()
					w.fake.dropConnections()
					waitForClaude(t, "the reconnect's resume", func() bool { return w.fake.resumeCount() > lost })
					notReady("after the reconnect, before the saved file caught up")
				}
			}
			release()
			if test.unread {
				notReady("while the saved prompts cannot be read")
				w.failSavedRows.Store(false) // the retry now reads them
			}
			if test.failing {
				waitForClaude(t, "a failed catalog read", func() bool { return w.failedReads.Load() > 0 })
				notReady("while the catalog cannot read the saved file")
				w.failReads.Store(false) // the held-file retry now succeeds
			}

			waitForClaudeWithin(t, time.Minute, "the queued prompt sent", func() bool {
				return len(w.fake.startedTexts()) > len(sentBefore) && sess.summary().Status == "ready" &&
					(test.missing || test.unused || w.state(t, "p1") != turns.Accepted)
			})
			time.Sleep(300 * time.Millisecond) // a resend would be queued by now
			records, err := w.s.turns.List(ctx, hosted)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if action := record.Outbox.ActionID; action != "p1" && action != "p2" {
					t.Fatalf("p1, which Codex saved, was resent as %s (p1 now %s)", action, w.state(t, "p1"))
				}
			}
			if !test.missing && !test.unused {
				if state := w.state(t, "p1"); state != turns.Confirmed {
					t.Fatalf("p1 = %s; want confirmed by its saved row", state)
				}
			}
			if got, want := w.fake.startedTexts(), append(sentBefore, "p2 prompt"); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("provider received %q; want %q", got, want)
			}
			if state := w.state(t, "p2"); state != turns.Accepted {
				t.Fatalf("p2 = %s; want accepted", state)
			}
		})
	}
}

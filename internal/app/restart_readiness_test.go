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

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// restartWindow is one Codex thread attached through the real boot path to a
// fake app server, with its rollout indexed by a real catalog that the test
// can pause. A paused catalog leaves the attachment's catch-up pending.
type restartWindow struct {
	s        *server
	fake     *fakeAppServer
	rec      hostedstore.Attachment
	catalog  *sidebar.CatalogSource
	attached *pausedAttachedCatalog
	t        *testing.T
	path     string
	// failReads makes every catalog read of the rollout fail; failedReads
	// counts them. failSavedRows makes the settlement read of saved prompts
	// fail.
	failReads     catalogReadFailure
	failSavedRows atomic.Bool
}

// failingSavedRows is the catalog with a settlement read that can fail.
type failingSavedRows struct {
	*sidebar.CatalogSource
	fail *atomic.Bool
}

func (c failingSavedRows) LoadSavedUserRows(ctx context.Context, threadID string, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
	if c.fail.Load() {
		return nil, errors.New("saved rows unreadable")
	}
	return c.CatalogSource.LoadSavedUserRows(ctx, threadID, keys)
}

func newRestartWindow(t *testing.T) *restartWindow {
	t.Helper()
	ctx := context.Background()
	fake := newFakeAppServer(t)
	s, store := bootReattachServer(t, "", fake.WSURL())
	rec := bootCodexRecord(t)
	root := t.TempDir()
	w := &restartWindow{s: s, fake: fake, t: t, path: filepath.Join(root, "rollout-"+rec.NativeThreadID+".jsonl")}
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
	w.catalog, err = newThreadCatalogService(ctx, historyStore, threadindex.Options{
		Roots: []history.SourceRoot{{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: root}},
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
	w.attached = &pausedAttachedCatalog{CatalogSource: w.catalog}
	w.failReads = catalogReadFailure{t: t, path: w.path, catalog: w.catalog}
	t.Cleanup(func() { w.failReads.Store(false) })
	s.catalog, s.catalogSources = failingSavedRows{w.catalog, &w.failSavedRows}, w.attached
	w.catalog.SetTranscriptChanged(s.attachmentCoordinator().ConfirmDurableThread)
	store.Remember(rec)
	seedBootAttachments(t, s)
	w.rec = fixtureAttachmentRecords(store)[0]
	return w
}

// pause holds the readiness result consumed by the attachment while the real index may continue working.
func (w *restartWindow) pause() func() { return w.attached.pause() }

// A read failure is a real permission failure on this test's disposable rollout.
type catalogReadFailure struct {
	t       *testing.T
	path    string
	catalog *sidebar.CatalogSource
}

func (f *catalogReadFailure) Store(fail bool) {
	mode := os.FileMode(0600)
	if fail {
		mode = 0
	}
	if err := os.Chmod(f.path, mode); err != nil {
		if os.IsNotExist(err) {
			return
		}
		f.t.Fatal(err)
	}
	at := time.Now().Add(time.Second)
	if err := os.Chtimes(f.path, at, at); err != nil {
		f.t.Fatal(err)
	}
	f.catalog.FileChanged(f.path)
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
	w.catalog.FileChanged(w.path)
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

func (w *restartWindow) state(t *testing.T, action string) prompts.DeliveryState {
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
			w.fake.SetReplyDelay("thread/resume", delay)
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
					w.fake.SetResumeError("no rollout found for thread id " + native)
				}
				go func() {
					w.s.attachmentCoordinator().ReattachOnBoot()
					w.s.attachmentCoordinator().ReattachWaitingHosted(hosted) // no work open, so it rests until needed
				}()
				waitForClaude(t, "the boot shell", func() bool {
					sess = w.s.SessionForHostedThread(hosted)
					return sess != nil && sess.PromptStore() != nil
				})
			case !test.reattach:
				// p1 was accepted into turn-p1 before Nano stopped; Codex
				// saved it and the turn's interruption while Nano was down.
				text := "p1 prompt"
				if _, _, err := w.s.turns.Submit(ctx, prompts.Submission{HostedThreadID: hosted, Provider: model.ProviderCodex, NativeThreadID: native, ActionID: "p1",
					ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: prompts.HumanOrigin}); err != nil {
					t.Fatal(err)
				}
				if _, claimed, err := w.s.turns.Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: "before-restart", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
					t.Fatalf("claim p1 = %v, %v", claimed, err)
				}
				binding := prompts.Binding{EventID: submissionEventID(hosted, "p1"), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}}
				if _, err := w.s.turns.Accepted(ctx, prompts.Acceptance{ActionID: "p1", ClaimToken: "before-restart", AttachmentEpoch: 1, Binding: binding}); err != nil {
					t.Fatal(err)
				}
				release = w.pause()
				w.appendSaved(t, savedPromptTurn(hosted, "p1", turn))
				w.failReads.Store(test.failing)
				w.failSavedRows.Store(test.unread)
				w.fake.SetResumeTurns(json.RawMessage(resumeTurns))
				if test.failing {
					w.fake.SetResumeResult(map[string]any{"model": "gpt-test", "thread": map[string]any{"id": native, "cwd": "/tmp", "path": w.path, "turns": json.RawMessage(resumeTurns)}})
				}
				go w.s.attachmentCoordinator().ReattachOnBoot()
				waitForClaude(t, "the boot shell", func() bool {
					sess = w.s.SessionForHostedThread(hosted)
					return sess != nil && sess.PromptStore() != nil
				})
			default:
				w.s.attachmentCoordinator().ReattachOnBoot()
				w.s.attachmentCoordinator().ReattachWaitingHosted(hosted) // no work open, so it rests until needed
				waitForClaude(t, "the first attachment", func() bool {
					sess = w.s.SessionForHostedThread(hosted)
					return sess != nil && sess.Summary().Status == "ready"
				})
				// p1 starts turn-p1 on the first attachment. The link drops
				// before Codex's echo of p1 arrives; in the gap Codex saves p1
				// and the turn's interruption.
				w.fake.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": turn}})
				if rec := postTurn(t, sess, ctx, "p1", "p1 prompt"); rec.Code != http.StatusOK {
					t.Fatalf("p1 delivery = %d %s", rec.Code, rec.Body.String())
				}
				sentBefore = []string{"p1 prompt"}
				w.fake.EmitThreadNotification(native, map[string]any{"method": "turn/started", "params": map[string]any{
					"threadId": native, "turn": map[string]any{"id": turn, "status": "inProgress", "items": []any{}}}})
				waitForClaude(t, "turn-p1 working", func() bool {
					record, _, err := sess.PromptStore().Get(ctx, "p1")
					return err == nil && record.ProviderTurnID == turn && sess.Summary().TurnActive
				})
				release = w.pause()
				w.appendSaved(t, savedPromptTurn(hosted, "p1", turn))
				w.fake.SetResumeTurns(json.RawMessage(resumeTurns))
				if test.failing {
					w.fake.SetResumeResult(map[string]any{"model": "gpt-test", "thread": map[string]any{"id": native, "cwd": "/tmp", "path": w.path, "turns": json.RawMessage(resumeTurns)}})
				}
				resumes = w.fake.ResumeCount()
				w.fake.DropConnections()
				waitForClaude(t, "the link loss", func() bool { return sess.Summary().Status == "reconnecting" })
			}
			defer release()
			if test.ahead {
				go func() {
					waitForClaude(t, "the resume request", func() bool { return w.fake.ResumeCount() > resumes })
					w.fake.EmitThreadNotification(native, map[string]any{"method": "turn/completed", "params": map[string]any{
						"threadId": native, "turn": map[string]any{"id": turn, "status": "interrupted"}}})
				}()
			}

			// p2 is typed while the thread cannot take it yet.
			w.fake.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "turn-p2"}})
			if rec := postTurn(t, sess, ctx, "p2", "p2 prompt"); rec.Code != http.StatusAccepted {
				t.Fatalf("p2 admission = %d %s; want queued", rec.Code, rec.Body.String())
			}
			notReady := func(why string) {
				t.Helper()
				for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
					if status, sent := sess.Summary().Status, w.fake.StartedTexts(); status == "ready" || fmt.Sprint(sent) != fmt.Sprint(sentBefore) {
						t.Fatalf("%s: status %s, sent %q", why, status, sent)
					}
				}
			}
			writeRollout := func() {
				writeSettlementFile(t, w.path, fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":"/disposable","timestamp":"2026-10-08T17:00:00Z"}}`+"\n", native))
				w.catalog.FileChanged(w.path)
			}
			switch {
			case test.early:
				// The pass that finds no file, then the one that indexes it,
				// both land before readiness starts to wait.
				waitForClaude(t, "the pass that found no file", func() bool {
					status := w.catalog.IngestStatus()
					return status.Attachments > 0 && status.Queued == 0 && status.InFlight == 0
				})
				writeRollout()
				current := make(chan bool, 1)
				go func() { current <- w.catalog.AwaitAttachedCurrent(sess.ID(), true, nil, ctx.Done()) }()
				select {
				case ok := <-current:
					if !ok {
						t.Fatal("written file failed catch-up")
					}
				case <-time.After(time.Second):
					t.Fatal("written file did not catch up")
				}

				if status := sess.Summary().Status; status != "starting" {
					t.Fatalf("the resume answer was processed before the file was indexed (status %s)", status)
				}
			case !test.unused:
				// Attached, but its saved file is not proven current: not
				// ready, and p2 waits.
				waitForClaude(t, "the resume answer", func() bool { return w.fake.ResumeCount() > resumes })
				notReady("before the saved file caught up")
				if test.missing {
					writeRollout()
				}
				if test.linkLost {
					// The wait gives way to the reconnect, which waits in turn.
					lost := w.fake.ResumeCount()
					w.fake.DropConnections()
					waitForClaude(t, "the reconnect's resume", func() bool { return w.fake.ResumeCount() > lost })
					notReady("after the reconnect, before the saved file caught up")
				}
			}
			release()
			if test.unread {
				notReady("while the saved prompts cannot be read")
				w.failSavedRows.Store(false) // the retry now reads them
			}
			if test.failing {
				waitForClaude(t, "a failed catalog read", func() bool { return w.catalog.IngestStatus().FailingSources > 0 })
				notReady("while the catalog cannot read the saved file")
				w.failReads.Store(false) // the held-file retry now succeeds

			}

			waitForClaudeWithin(t, time.Minute, "the queued prompt sent", func() bool {
				return len(w.fake.StartedTexts()) > len(sentBefore) && sess.Summary().Status == "ready" &&
					(test.missing || test.unused || w.state(t, "p1") != prompts.Accepted)
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
				if state := w.state(t, "p1"); state != prompts.Confirmed {
					t.Fatalf("p1 = %s; want confirmed by its saved row", state)
				}
			}
			if got, want := w.fake.StartedTexts(), append(sentBefore, "p2 prompt"); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("provider received %q; want %q", got, want)
			}
			if state := w.state(t, "p2"); state != prompts.Accepted {
				t.Fatalf("p2 = %s; want accepted", state)
			}
		})
	}
}

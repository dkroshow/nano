package sessions

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/threads"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func TestSessionLifecycleWaitingBlocksSendAndDisconnectedWork(t *testing.T) {
	sess := lifecycleTestSession("ready", true, "current")
	sess.delivery.addPendingInteraction(interact.PendingInteraction{ID: "permission", TurnID: "current"})
	if sum := sess.Summary(); !sum.Waiting {
		t.Fatal("waiting interaction missing")
	}
	sess.stateMu.Lock()
	permission := sess.lifecyclePermissionLocked()
	sess.stateMu.Unlock()
	if permission.Availability != lifecycle.AwaitingHuman || permission.Submit != lifecycle.QueuePrompt {
		t.Fatalf("waiting permissions = %+v", permission)
	}
	sess.observeConnection(lifecycle.EventLinkLost)
	if sum := sess.Summary(); sum.Waiting || sum.TurnActive {
		t.Fatalf("work leaked through connection loss: %+v", sum)
	}
	if _, id := sess.activeTurn(); id != "" {
		t.Fatal("disconnected turn remained interruptible")
	}
}

func TestSessionLifecycleAcceptanceCannotResurrectCompletedTurn(t *testing.T) {
	sess := lifecycleTestSession("ready", false, "")
	if sess.Summary().TurnActive {
		t.Fatal("reservation asserted provider work")
	}
	sess.acceptProviderTurn("current")
	if sess.lifecycleState.State.Work != lifecycle.Accepted {
		t.Fatal("provider ACK did not establish accepted work")
	}
	sess.setTurnActive(false, "current", 0, true)
	sess.acceptProviderTurn("current")
	if sess.Summary().TurnActive {
		t.Fatal("late ACK resurrected completed turn")
	}
}

func TestCodexLifecycleIgnoresOtherNativeThread(t *testing.T) {
	sess := lifecycleTestSession("ready", false, "")
	sess.threadID = "own-native"
	observeCodexWork(sess, protocol.Recv, []byte(`{"method":"turn/started","params":{"threadId":"other-native","turn":{"id":"current"}}}`), true)
	if sess.Summary().TurnActive {
		t.Fatal("other native thread asserted work")
	}
}

func TestProviderLifecycleCurrentUsesSettledProviderEvidence(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {
		for _, active := range []bool{false, true} {
			for _, settled := range []bool{false, true} {
				t.Run(provider+"/"+boolName(active)+"/settled="+boolName(settled), func(t *testing.T) {
					adapter := restartEvidenceFixture(provider, active, settled, 8, 13)
					observation, ok := adapter.Current()
					if ok != settled {
						t.Fatalf("settled=%v observation=%+v ok=%v", settled, observation, ok)
					}
					if !ok {
						return
					}
					want := lifecycle.EventReadyIdle
					if active {
						want = lifecycle.EventReadyWorking
					}
					if observation != (lifecycle.Observation{Event: want, Epoch: 8, Sequence: 13}) {
						t.Fatalf("provider snapshot = %+v", observation)
					}
				})
			}
		}
	}
}

func boolName(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func restartEvidenceFixture(provider string, active, settled bool, epoch, sequence uint64) lifecycle.Adapter {
	switch provider {
	case "codex":
		status := "completed"
		if active {
			status = "inProgress"
		}
		response, _ := json.Marshal(map[string]any{"thread": map[string]any{"id": "native", "turns": []map[string]any{{"id": "current", "status": status, "startedAt": 123}}}})
		return codexLifecycleAdapter{epoch: epoch, sequence: sequence, response: decodedCodexResume(response), nativeID: "native", settled: settled}
	case "claude":
		return claudeLifecycleAdapter{epoch: epoch, sequence: sequence, state: "running", active: active, settled: settled}
	case "grok":
		id := ""
		if active {
			id = "current"
		}
		response, _ := json.Marshal(map[string]any{"_meta": map[string]any{"sessionId": "native", "x.ai/runningPromptId": id}})
		return grokLifecycleAdapter{epoch: epoch, sequence: sequence, response: response, nativeID: "native", settled: settled}
	}
	panic(provider)
}

func TestProviderRestartIdleDiscardsOldWorkAnchors(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {
		t.Run(provider, func(t *testing.T) {
			sess := lifecycleTestSession("starting", false, "old-stored-turn")
			sess.provider = provider
			sess.turnStartedAtMs = 123
			sess.beginTurnRestore()
			epoch, sequence := sess.evidenceCoordinates()
			if !sess.settleProviderEvidence(restartEvidenceFixture(provider, false, true, epoch, sequence), "", 0) {
				t.Fatal("current provider snapshot refused")
			}
			if sum := sess.Summary(); sum.Status != "ready" || sum.TurnActive || sum.TurnStartedAtMs != 0 {
				t.Fatalf("stored anchors chose current work: %+v", sum)
			}
		})
	}
}

func TestProviderRestartNewerTerminalWinsOverResumeResponse(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {
		t.Run(provider, func(t *testing.T) {
			sess := lifecycleTestSession("starting", false, "")
			sess.provider = provider
			sess.beginTurnRestore()
			epoch, sequence := sess.evidenceCoordinates()
			response := restartEvidenceFixture(provider, true, true, epoch, sequence)
			sess.setTurnActive(false, "current", 0, true)
			if !sess.settleProviderEvidence(response, "current", 123) {
				t.Fatal("settlement refused")
			}
			if sess.Summary().TurnActive {
				t.Fatal("old response resurrected completed work")
			}
		})
	}
}

func TestProviderRestartConnectionLossRejectsEarlierResponse(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {
		t.Run(provider, func(t *testing.T) {
			sess := lifecycleTestSession("starting", false, "")
			sess.provider = provider
			sess.beginTurnRestore()
			epoch, sequence := sess.evidenceCoordinates()
			sess.observeConnection(lifecycle.EventLinkLost)
			if sess.settleProviderEvidence(restartEvidenceFixture(provider, true, true, epoch, sequence), "current", 123) {
				t.Fatal("response predating transport loss was accepted")
			}
			if sum := sess.Summary(); sum.Status != "reconnecting" || sum.TurnActive {
				t.Fatalf("lost transport promoted: %+v", sum)
			}
		})
	}
}

func TestProviderRestartWaitingSettlesWithReadiness(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {
		t.Run(provider, func(t *testing.T) {
			sess := lifecycleTestSession("starting", false, "")
			sess.provider = provider
			sess.beginTurnRestore()
			epoch, sequence := sess.evidenceCoordinates()
			sess.delivery.addPendingInteraction(interact.PendingInteraction{ID: "request", TurnID: "current"})
			if sum := sess.Summary(); sum.Waiting || sum.TurnActive {
				t.Fatal("pending replay became current before readiness")
			}
			if !sess.settleProviderEvidence(restartEvidenceFixture(provider, true, true, epoch, sequence), "current", 123) {
				t.Fatal("newer interaction prevented readiness")
			}
			if !sess.Summary().Waiting {
				t.Fatal("readiness lost pending provider request")
			}
			sess.delivery.takePendingInteraction("request")
			if got := sess.lifecycleState.State.Work; got != lifecycle.Working {
				t.Fatalf("answer did not resume work: %s", got)
			}
		})
	}
}

func TestProviderRestartAcceptanceWaitsForSettledEvidence(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {
		t.Run(provider, func(t *testing.T) {
			sess := lifecycleTestSession("starting", false, "stored-turn")
			sess.provider = provider
			sess.beginTurnRestore()
			epoch, sequence := sess.evidenceCoordinates()
			response := restartEvidenceFixture(provider, false, true, epoch, sequence)
			// The provider ACK arrives after this snapshot, while replay is open.
			sess.acceptProviderTurn("accepted-current")
			if sum := sess.Summary(); sum.Status != "starting" || sum.TurnActive {
				t.Fatal("acceptance escaped before provider replay settled")
			}
			if !sess.settleProviderEvidence(response, "", 0) || sess.lifecycleState.State.Work != lifecycle.Accepted || sess.turnID != "accepted-current" {
				t.Fatalf("newer provider acceptance lost at settlement: %+v", sess.lifecycleState)
			}
		})
	}
}

func TestProviderCurrentRefusesAbsentOrUnqualifiedEvidence(t *testing.T) {
	cases := map[string]lifecycle.Adapter{
		"codex/missing":                codexLifecycleAdapter{epoch: 1, sequence: 1, settled: true, nativeID: "native", response: decodedCodexResume([]byte(`{"thread":{}}`))},
		"codex/wrong-native":           codexLifecycleAdapter{epoch: 1, sequence: 1, settled: true, nativeID: "native", response: decodedCodexResume([]byte(`{"thread":{"id":"other"}}`))},
		"claude/missing":               claudeLifecycleAdapter{epoch: 1, sequence: 1, settled: true},
		"claude/exited":                claudeLifecycleAdapter{epoch: 1, sequence: 1, settled: true, state: "exited", active: true},
		"grok/missing-both-identities": grokLifecycleAdapter{epoch: 1, sequence: 1, settled: true, response: json.RawMessage(`{}`)},
		"grok/wrong-native":            grokLifecycleAdapter{epoch: 1, sequence: 1, settled: true, nativeID: "native", response: json.RawMessage(`{"sessionId":"other"}`)},
	}
	for _, provider := range []string{"codex", "claude", "grok"} {
		cases[provider+"/no-epoch"] = restartEvidenceFixture(provider, true, true, 0, 1)
		cases[provider+"/no-sequence"] = restartEvidenceFixture(provider, true, true, 1, 0)
	}
	for name, adapter := range cases {
		t.Run(name, func(t *testing.T) {
			if observation, ok := adapter.Current(); ok {
				t.Fatalf("unqualified provider became current: %+v", observation)
			}
		})
	}
}

func TestProviderRestartCloseAndResumeUseDurableEpoch(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {
		t.Run(provider, func(t *testing.T) {
			db, _, err := testTurnDatabase(t, filepath.Join(t.TempDir(), "lifecycle.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO hosted_threads(hosted_thread_id,provider,presentation_mode,revision,created_at,updated_at) VALUES(?,?,?,1,?,?)`, "owner", provider, "shared", time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			newAttachment := func() *Session {
				binding, err := threads.BeginTransportBinding(context.Background(), db, "owner")
				generation := binding.Generation
				if err != nil {
					t.Fatal(err)
				}
				state, err := lifecycle.New(uint64(generation))
				if err != nil {
					t.Fatal(err)
				}
				sess := lifecycleTestSession("starting", false, "stale-anchor")
				sess.provider, sess.lifecycleState, sess.attachmentGeneration = provider, state, generation
				sess.beginTurnRestore()
				return sess
			}
			old := newAttachment()
			epoch, sequence := old.evidenceCoordinates()
			response := restartEvidenceFixture(provider, true, true, epoch, sequence)
			old.observeConnection(lifecycle.EventClose)
			if old.settleProviderEvidence(response, "current", 123) || old.Summary().Status != "exited" {
				t.Fatal("late readiness reopened a closed attachment")
			}
			resumed := newAttachment()
			if resumed.lifecycleState.Epoch <= epoch || resumed.settleProviderEvidence(response, "current", 123) {
				t.Fatal("resume accepted an obsolete durable attachment epoch")
			}
			epoch, sequence = resumed.evidenceCoordinates()
			if !resumed.settleProviderEvidence(restartEvidenceFixture(provider, false, true, epoch, sequence), "", 0) {
				t.Fatal("new attachment refused current provider evidence")
			}
			if sum := resumed.Summary(); sum.Status != "ready" || sum.TurnActive || sum.TurnStartedAtMs != 0 {
				t.Fatalf("resumed idle provider inherited stale work: %+v", sum)
			}
		})
	}
}

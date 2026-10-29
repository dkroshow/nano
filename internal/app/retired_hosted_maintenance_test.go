package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	state "github.com/dkroshow/nano/internal/state/store"
	"github.com/dkroshow/nano/internal/threads"
)

type retirementServerFixture struct {
	server             *server
	old, owner, child  threads.SessionRecord
	oldTurn, childTurn threads.TurnRecord
}

func newRetirementServer(t *testing.T) retirementServerFixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nano_threads.sqlite")
	control, err := fixtureOpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	if _, err := prompts.MigrateTurnRecords(ctx, control.CanonicalDatabase(), prompts.LegacyTurnPaths{}, filepath.Join(t.TempDir(), "migration")); err != nil {
		t.Fatal(err)
	}
	if _, err := prompts.MigrateTurnTransactions(ctx, control.CanonicalDatabase(), filepath.Join(t.TempDir(), "schema20.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := coreturns.MigrateCompletedTurnUsage(ctx, control.CanonicalDatabase(), filepath.Join(t.TempDir(), "schema21.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := prompts.MigrateMemberRecovery(ctx, control.CanonicalDatabase(), filepath.Join(t.TempDir(), "schema22.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := prompts.MigrateParkedDiscard(ctx, control.CanonicalDatabase(), filepath.Join(t.TempDir(), "schema23.sqlite")); err != nil {
		t.Fatal(err)
	}
	executions, err := newControlAgentExecutionStore(control, "test")
	if err != nil {
		t.Fatal(err)
	}
	old := createIdentityHosted(t, executions, "codex", identityNativeOld)
	owner := createIdentityHosted(t, executions, "codex", identityNativeNew)
	if err := control.TransferNativeClaim(owner.ID, "codex", identityNativeOld, false); err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession(owner.ID, "owner-runtime", identityNativeOld, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	ownerMetadata, err := control.View(owner.ID)
	owner.NativeThreadID = ownerMetadata.NativeThreadID
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := fixtureBeginSessionWithInvoker(executions, "child-alias", "child-hash", "codex", presentationShared, old)
	if err != nil {
		t.Fatal(err)
	}
	oldTurn, _, err := fixtureBeginTurn(executions, old.ID, "old-action", "old-hash")
	if err != nil {
		t.Fatal(err)
	}
	childTurn, _, err := fixtureHistoricalTurn(executions, child.ID, "child-action", "child-hash", hostedstore.TurnAttribution{SenderHostedThreadID: old.ID, SenderNativeThreadID: identityNativeOld, SenderProvider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	// Reload the historical fixture rows so the runtime starts from their committed attribution.
	executions, err = newControlAgentExecutionStore(control, "test")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newQueuedTurnStore(control.CanonicalDatabase(), path, testPromptStore(t, control.CanonicalDatabase()))
	if err != nil {
		t.Fatal(err)
	}
	// Seeded after adoption, as the schema-20 rows retirement handles: an
	// adopted journal row would carry the retiring hosted id, which the
	// retirement check refuses.
	if err := prompts.SaveQueuedTurns(ctx, control.CanonicalDatabase(), []prompts.QueuedTurn{{ClientActionID: "unconfirmed", Provider: "codex", HostedThreadID: old.ID, NativeThreadID: identityNativeOld, Text: "keep this evidence", State: submissionAccepted}}); err != nil {
		t.Fatal(err)
	}
	if err := prompts.SaveSendAttempts(ctx, control.CanonicalDatabase(), []prompts.SendAttempt{{ClientActionID: "unconfirmed", Provider: "codex", NativeThreadID: identityNativeOld, Status: sendAttemptAccepted}}); err != nil {
		t.Fatal(err)
	}
	return retirementServerFixture{server: &server{controlMetadata: control, agentExecutions: executions, queuedTurns: queue, Registry: newSessionRegistry()}, old: old, owner: owner, child: child, oldTurn: oldTurn, childTurn: childTurn}
}
func retirementHTTP(t *testing.T, s *server, body string) (retiredHostedReceipt, int) {
	t.Helper()
	w := httptest.NewRecorder()
	fixtureOwners(s).hostedController().HandleRetirement(w, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/retired-hosted-records", strings.NewReader(body)))
	var result retiredHostedReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result, w.Code
}

func TestRetiredHostedHTTPKeepsCachesAndResolverConsistent(t *testing.T) {
	for _, disposition := range []string{"repoint", "delete"} {
		t.Run(disposition, func(t *testing.T) {
			f := newRetirementServer(t)
			s := f.server
			preview, status := retirementHTTP(t, s, fmt.Sprintf(`{"apply":false,"outboxDisposition":%q}`, disposition))
			if status != 200 || !preview.OK || preview.Applied {
				t.Fatalf("preview: %+v status=%d", preview, status)
			}
			if _, ok := s.agentExecutions.Session(f.old.ID); !ok {
				t.Fatal("preview removed cached session")
			}
			request := fmt.Sprintf(`{"apply":true,"outboxDisposition":%q,"expectedFingerprint":%q}`, disposition, preview.Preview.Fingerprint)
			receipt, status := retirementHTTP(t, s, request)
			if status != 200 || !receipt.OK || !receipt.Applied {
				t.Fatalf("apply: %+v status=%d", receipt, status)
			}
			if !reflect.DeepEqual(preview.Preview, receipt.Preview) {
				t.Fatal("apply changed the previewed set")
			}
			if len(receipt.Resolutions) != 1 || receipt.Resolutions[0].HostedThreadID != f.owner.ID {
				t.Fatalf("resolver proof: %+v", receipt.Resolutions)
			}
			if _, ok := s.agentExecutions.Session(f.old.ID); ok {
				t.Fatal("cached retired session survived")
			}
			if _, ok := s.agentExecutions.Turn(f.oldTurn.ID); ok {
				t.Fatal("cached retired operation survived")
			}
			child, _ := s.agentExecutions.Session(f.child.ID)
			if child.InvokerHostedThreadID != f.owner.ID {
				t.Fatal("cached child retains old invoker")
			}
			childTurn, _ := s.agentExecutions.Turn(f.childTurn.ID)
			if childTurn.SenderHostedThreadID != f.owner.ID {
				t.Fatal("cached operation retains old sender")
			}
			queued, ok := s.queuedTurns.Get("unconfirmed")
			if disposition == "delete" {
				if ok {
					t.Fatal("cached outbox survived delete")
				}
			} else if !ok || queued.HostedThreadID != f.owner.ID || queued.State != submissionAccepted || queued.Text != "keep this evidence" {
				t.Fatalf("cached outbox changed: %+v", queued)
			}
			// Exercise later ordinary writes: stale maps must not restore deleted facts
			// or undo either owner-column update.
			if _, _, err := QueueEnqueue(s.queuedTurns, prompts.QueuedTurn{ClientActionID: "later", Provider: "codex", HostedThreadID: f.child.ID, Text: "later evidence"}); err != nil {
				t.Fatal(err)
			}
			if err := s.agentExecutions.UpdateSession(f.child.ID, "", "", "detached", ""); err != nil {
				t.Fatal(err)
			}
			rec, _ := s.agentExecutions.Turn(f.childTurn.ID)
			err := s.agentExecutions.MarkTurnDelivery(rec.ID, rec.ProviderTurnID, rec.Status, rec.Error)
			if err != nil {
				t.Fatal(err)
			}
			var sender string
			if err := s.controlMetadata.CanonicalDatabase().QueryRow(`SELECT sender_hosted_id FROM hosted_turn_operations WHERE operation_id=?`, f.childTurn.ID).Scan(&sender); err != nil || sender != f.owner.ID {
				t.Fatalf("later write restored sender: %s %v", sender, err)
			}
			var old int
			if err := s.controlMetadata.CanonicalDatabase().QueryRow(`SELECT count(*) FROM hosted_threads WHERE hosted_thread_id=?`, f.old.ID).Scan(&old); err != nil || old != 0 {
				t.Fatal("later write revived retired record")
			}
			status, resolved, raw := resolveIdentity(t, s, identityNativeOld)
			if status != 200 || resolved.HostedThreadID != f.owner.ID || resolved.MatchCount != 1 {
				t.Fatalf("public resolver: %d %s", status, raw)
			}
			if resolved != receipt.Resolutions[0] {
				t.Fatalf("receipt disagrees with public resolver: %+v %+v", resolved, receipt.Resolutions[0])
			}
			retry, status := retirementHTTP(t, s, request)
			if status != http.StatusConflict || retry.Applied {
				t.Fatal("stale preview applied twice")
			}
		})
	}
}

func TestRetiredHostedResolverFailureRollsBackDatabaseAndCaches(t *testing.T) {
	f := newRetirementServer(t)
	s := f.server
	other := createIdentityHosted(t, s.agentExecutions, "claude", identityNativeNew)
	if _, err := s.controlMetadata.CanonicalDatabase().Exec(`INSERT INTO hosted_thread_native_ids(hosted_thread_id,provider,native_thread_id,retired_at,first_seen_at,last_seen_at) VALUES(?,'claude',?,'retired','before','before')`, other.ID, identityNativeOld); err != nil {
		t.Fatal(err)
	}
	preview := s.retireHostedRecords(context.Background(), state.RetirementRequest{OutboxDisposition: "repoint"})
	if !preview.OK {
		t.Fatal(preview.Error)
	}
	result := s.retireHostedRecords(context.Background(), state.RetirementRequest{Apply: true, OutboxDisposition: "repoint", ExpectedFingerprint: preview.Preview.Fingerprint})
	if result.OK || result.Applied || !strings.Contains(result.Error, "resolver proof") {
		t.Fatalf("ambiguous resolver accepted: %+v", result)
	}
	if _, ok := s.agentExecutions.Session(f.old.ID); !ok {
		t.Fatal("rollback lost cached retired session")
	}
	if row, ok := s.queuedTurns.Get("unconfirmed"); !ok || row.HostedThreadID != f.old.ID {
		t.Fatal("rollback changed outbox cache")
	}
	var retainedID string
	if err := s.controlMetadata.CanonicalDatabase().QueryRow(`SELECT hosted_thread_id FROM hosted_threads WHERE hosted_thread_id=?`, f.old.ID).Scan(&retainedID); err != nil || retainedID != f.old.ID {
		t.Fatalf("rollback lost durable session: %q %v", retainedID, err)
	}
}

func TestRetiredHostedRefusesRuntimeAndAttachmentReferences(t *testing.T) {
	for _, kind := range []string{"runtime-owner", "runtime-invoker", "attachment-owner", "attachment-invoker"} {
		t.Run(kind, func(t *testing.T) {
			f := newRetirementServer(t)
			s := f.server
			switch kind {
			case "runtime-owner":
				s.RegisterSession(newFixtureSession(sessions.Config{HostedThreadID: f.old.ID}, ""))
			case "runtime-invoker":
				s.RegisterSession(newFixtureSession(sessions.Config{HostedThreadID: f.child.ID, InvokerHostedThreadID: f.old.ID}, ""))
			case "attachment-owner":
				s.attachments = newTestStore(t)
				s.attachments.Remember(hostedstore.Attachment{Provider: "codex", NativeThreadID: "retired-native", HostedThreadID: f.old.ID})
			case "attachment-invoker":
				s.attachments = newTestStore(t)
				s.attachments.Remember(hostedstore.Attachment{Provider: "codex", NativeThreadID: "retired-native", HostedThreadID: f.child.ID, InvokerHostedThreadID: f.old.ID})
			}
			result := s.retireHostedRecords(context.Background(), state.RetirementRequest{OutboxDisposition: "repoint"})
			if result.OK || result.Applied || !strings.Contains(result.Error, "reference") && !strings.Contains(result.Error, "edge") {
				t.Fatalf("attached target accepted: %+v", result)
			}
		})
	}
}

func TestRetiredHostedHTTPRequiresExplicitBoundedRequest(t *testing.T) {
	f := newRetirementServer(t)
	for _, body := range []string{`{}`, `{"apply":true}`, `{"apply":true,"outboxDisposition":"repoint"}`, `{"apply":false,"outboxDisposition":"guess"}`, `{"apply":false,"outboxDisposition":"repoint","hostedIds":["anything"]}`, `{"apply":false,"outboxDisposition":"repoint"} {}`} {
		w := httptest.NewRecorder()
		fixtureOwners(f.server).hostedController().HandleRetirement(w, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/retired-hosted-records", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("accepted %s: %d %s", body, w.Code, w.Body.String())
		}
	}
}

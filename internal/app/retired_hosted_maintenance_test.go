package app

import (
	"context"
	"encoding/json"
	"fmt"
	state "github.com/dkroshow/nano/internal/app/statestore"
	turns "github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type retirementServerFixture struct {
	server             *server
	old, owner, child  agentSessionRecord
	oldTurn, childTurn agentTurnRecord
}

func newRetirementServer(t *testing.T) retirementServerFixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nano_threads.sqlite")
	control, err := openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	if _, err := turns.MigrateTurnRecords(ctx, control.db, turns.LegacyTurnPaths{}, filepath.Join(t.TempDir(), "migration")); err != nil {
		t.Fatal(err)
	}
	if _, err := turns.MigrateTurnTransactions(ctx, control.db, filepath.Join(t.TempDir(), "schema20.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := coreturns.MigrateCompletedTurnUsage(ctx, control.db, filepath.Join(t.TempDir(), "schema21.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := turns.MigrateMemberRecovery(ctx, control.db, filepath.Join(t.TempDir(), "schema22.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := turns.MigrateParkedDiscard(ctx, control.db, filepath.Join(t.TempDir(), "schema23.sqlite")); err != nil {
		t.Fatal(err)
	}
	executions, err := newControlAgentExecutionStore(control, "test")
	if err != nil {
		t.Fatal(err)
	}
	old := createIdentityHosted(t, executions, "codex", identityNativeOld)
	owner := createIdentityHosted(t, executions, "codex", identityNativeNew)
	if err := control.transferNativeClaim(owner.ID, "codex", identityNativeOld, false); err != nil {
		t.Fatal(err)
	}
	if err := executions.bindSession(owner.ID, "owner-runtime", identityNativeOld, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	owner, _, err = executions.sessionFromCanonical(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := executions.beginSessionWithInvoker("child-alias", "child-hash", "codex", presentationShared, old)
	if err != nil {
		t.Fatal(err)
	}
	oldTurn, _, err := executions.beginTurn(old.ID, "old-action", "old-hash")
	if err != nil {
		t.Fatal(err)
	}
	childTurn, _, err := executions.beginTurnAttributed(child.ID, "child-action", "child-hash", turnAttribution{SenderHostedThreadID: old.ID, SenderNativeThreadID: identityNativeOld, SenderProvider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newQueuedTurnStore(control.db, path, testPromptStore(t, control.db))
	if err != nil {
		t.Fatal(err)
	}
	// Seeded after adoption, as the schema-20 rows retirement handles: an
	// adopted journal row would carry the retiring hosted id, which the
	// retirement check refuses.
	if err := turns.SaveQueuedTurns(ctx, control.db, []queuedTurnRecord{{ClientActionID: "unconfirmed", Provider: "codex", HostedThreadID: old.ID, NativeThreadID: identityNativeOld, Text: "keep this evidence", State: submissionAccepted}}); err != nil {
		t.Fatal(err)
	}
	if err := turns.SaveSendAttempts(ctx, control.db, []sendAttemptRecord{{ClientActionID: "unconfirmed", Provider: "codex", NativeThreadID: identityNativeOld, Status: sendAttemptAccepted}}); err != nil {
		t.Fatal(err)
	}
	return retirementServerFixture{server: &server{controlMetadata: control, agentExecutions: executions, queuedTurns: queue, sessionRegistry: sessionRegistry{sessions: map[string]*session{}}}, old: old, owner: owner, child: child, oldTurn: oldTurn, childTurn: childTurn}
}
func retirementHTTP(t *testing.T, s *server, body string) (retiredHostedReceipt, int) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleRetiredHostedRecords(w, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/retired-hosted-records", strings.NewReader(body)))
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
			if _, ok := s.agentExecutions.session(f.old.ID); !ok {
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
			if _, ok := s.agentExecutions.session(f.old.ID); ok {
				t.Fatal("cached retired session survived")
			}
			if _, ok := s.agentExecutions.turns[f.oldTurn.ID]; ok {
				t.Fatal("cached retired operation survived")
			}
			child, _ := s.agentExecutions.session(f.child.ID)
			if child.InvokerHostedThreadID != f.owner.ID {
				t.Fatal("cached child retains old invoker")
			}
			if s.agentExecutions.turns[f.childTurn.ID].SenderHostedThreadID != f.owner.ID {
				t.Fatal("cached operation retains old sender")
			}
			queued, ok := s.queuedTurns.get("unconfirmed")
			if disposition == "delete" {
				if ok {
					t.Fatal("cached outbox survived delete")
				}
			} else if !ok || queued.HostedThreadID != f.owner.ID || queued.State != submissionAccepted || queued.Text != "keep this evidence" {
				t.Fatalf("cached outbox changed: %+v", queued)
			}
			// Exercise later ordinary writes: stale maps must not restore deleted facts
			// or undo either owner-column update.
			if _, _, err := s.queuedTurns.enqueue(queuedTurnRecord{ClientActionID: "later", Provider: "codex", HostedThreadID: f.child.ID, Text: "later evidence"}); err != nil {
				t.Fatal(err)
			}
			if err := s.agentExecutions.updateSession(f.child.ID, "", "", "detached", ""); err != nil {
				t.Fatal(err)
			}
			s.agentExecutions.mu.Lock()
			rec := s.agentExecutions.turns[f.childTurn.ID]
			touchAgentTurn(&rec)
			s.agentExecutions.turns[rec.ID] = rec
			err := s.agentExecutions.persistAndNotifyLocked()
			s.agentExecutions.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			var sender string
			if err := s.controlMetadata.db.QueryRow(`SELECT sender_hosted_id FROM hosted_turn_operations WHERE operation_id=?`, f.childTurn.ID).Scan(&sender); err != nil || sender != f.owner.ID {
				t.Fatalf("later write restored sender: %s %v", sender, err)
			}
			var old int
			if err := s.controlMetadata.db.QueryRow(`SELECT count(*) FROM hosted_threads WHERE hosted_thread_id=?`, f.old.ID).Scan(&old); err != nil || old != 0 {
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
	if _, err := s.controlMetadata.db.Exec(`INSERT INTO hosted_thread_native_ids(hosted_thread_id,provider,native_thread_id,retired_at,first_seen_at,last_seen_at) VALUES(?,'claude',?,'retired','before','before')`, other.ID, identityNativeOld); err != nil {
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
	if _, ok := s.agentExecutions.session(f.old.ID); !ok {
		t.Fatal("rollback lost cached retired session")
	}
	if row, ok := s.queuedTurns.get("unconfirmed"); !ok || row.HostedThreadID != f.old.ID {
		t.Fatal("rollback changed outbox cache")
	}
	if row, ok, err := s.agentExecutions.sessionFromCanonical(f.old.ID); err != nil || !ok || row.ID != f.old.ID {
		t.Fatalf("rollback lost durable session: %+v %v", row, err)
	}
}

func TestRetiredHostedRefusesRuntimeAndAttachmentReferences(t *testing.T) {
	for _, kind := range []string{"runtime-owner", "runtime-invoker", "attachment-owner", "attachment-invoker"} {
		t.Run(kind, func(t *testing.T) {
			f := newRetirementServer(t)
			s := f.server
			switch kind {
			case "runtime-owner":
				s.sessions["live"] = &session{hostedThreadID: f.old.ID}
			case "runtime-invoker":
				s.sessions["live"] = &session{hostedThreadID: f.child.ID, invokerHostedThreadID: f.old.ID}
			case "attachment-owner":
				s.attachments = &attachmentStore{byKey: map[attachmentKey]attachmentRecord{{}: {HostedThreadID: f.old.ID}}}
			case "attachment-invoker":
				s.attachments = &attachmentStore{byKey: map[attachmentKey]attachmentRecord{{}: {HostedThreadID: f.child.ID, InvokerHostedThreadID: f.old.ID}}}
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
		f.server.handleRetiredHostedRecords(w, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/retired-hosted-records", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("accepted %s: %d %s", body, w.Code, w.Body.String())
		}
	}
}

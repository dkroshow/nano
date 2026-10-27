package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/history"
)

func TestCreationNameMaintenanceHTTPPreviewsAndRequiresFingerprintForApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nano_threads.sqlite")
	control, err := openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	if _, err := turns.MigrateTurnRecords(context.Background(), control.db, turns.LegacyTurnPaths{}, filepath.Join(t.TempDir(), "migration")); err != nil {
		t.Fatal(err)
	}
	if _, err := turns.MigrateTurnTransactions(context.Background(), control.db, filepath.Join(t.TempDir(), "schema20.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := coreturns.MigrateCompletedTurnUsage(context.Background(), control.db, filepath.Join(t.TempDir(), "schema21.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := turns.MigrateMemberRecovery(context.Background(), control.db, filepath.Join(t.TempDir(), "schema22.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := turns.MigrateParkedDiscard(context.Background(), control.db, filepath.Join(t.TempDir(), "schema23.sqlite")); err != nil {
		t.Fatal(err)
	}
	executions, err := newControlAgentExecutionStore(control, "repair-http")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := executions.ensureSession("nht_repair_http", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := executions.bindSession(hosted.ID, "nano", "native-repair", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	queue, err := newQueuedTurnStore(control.db, path, testPromptStore(t, control.db))
	if err != nil {
		t.Fatal(err)
	}
	catalog := &fakeThreadCatalog{names: map[string]string{}}
	s := &server{controlMetadata: control, agentExecutions: executions, queuedTurns: queue, catalog: catalog}
	records := fmt.Sprintf(`[{"hostedThreadId":%q,"provider":"codex","nativeThreadId":"native-repair","name":"repaired name","source":"creation_request"}]`, hosted.ID)

	preview := httptest.NewRecorder()
	s.handleCreationNameRepair(preview, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/creation-names", strings.NewReader(`{"apply":false,"records":`+records+`}`)))
	var receipt creationNameRepairHTTPReceipt
	if err := json.Unmarshal(preview.Body.Bytes(), &receipt); err != nil || preview.Code != http.StatusOK || !receipt.OK || receipt.Preview.Missing != 1 {
		t.Fatalf("preview = %d %+v, %v", preview.Code, receipt, err)
	}

	unguarded := httptest.NewRecorder()
	s.handleCreationNameRepair(unguarded, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/creation-names", strings.NewReader(`{"apply":true,"records":`+records+`}`)))
	if unguarded.Code != http.StatusBadRequest {
		t.Fatalf("unguarded apply = %d %s", unguarded.Code, unguarded.Body)
	}

	unrecoverable := httptest.NewRecorder()
	unrecoverableBody := fmt.Sprintf(`{"apply":false,"unrecoverable":[{"hostedThreadId":%q,"provider":"codex","nativeThreadId":"native-repair","reason":"no creation record"}]}`, hosted.ID)
	s.handleCreationNameRepair(unrecoverable, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/creation-names", strings.NewReader(unrecoverableBody)))
	if err := json.Unmarshal(unrecoverable.Body.Bytes(), &receipt); err != nil || unrecoverable.Code != http.StatusOK || len(receipt.Preview.Unrecoverable) != 1 || receipt.Preview.Unrecoverable[0].State != "no_recoverable_name" {
		t.Fatalf("unrecoverable preview = %d %+v, %v", unrecoverable.Code, receipt, err)
	}

	apply := httptest.NewRecorder()
	// The unrecoverable preview replaced receipt, so recover the guarded repair
	// fingerprint from the original response.
	var guarded creationNameRepairHTTPReceipt
	if err := json.Unmarshal(preview.Body.Bytes(), &guarded); err != nil {
		t.Fatal(err)
	}
	applyBody := fmt.Sprintf(`{"apply":true,"expectedFingerprint":%q,"records":%s}`, guarded.Preview.Fingerprint, records)
	s.handleCreationNameRepair(apply, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/creation-names", strings.NewReader(applyBody)))
	if err := json.Unmarshal(apply.Body.Bytes(), &receipt); err != nil || apply.Code != http.StatusOK || !receipt.OK || !receipt.Applied {
		t.Fatalf("apply = %d %+v, %v", apply.Code, receipt, err)
	}
	ref, ok := catalog.ThreadNameRefFor(context.Background(), "codex", "native-repair")
	if !ok || ref.Value != "repaired name" || ref.Provenance != history.ThreadNameCreation {
		t.Fatalf("applied repair projection = %+v, found=%v", ref, ok)
	}
	idempotent := httptest.NewRecorder()
	s.handleCreationNameRepair(idempotent, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/creation-names", strings.NewReader(`{"apply":false,"records":`+records+`}`)))
	if err := json.Unmarshal(idempotent.Body.Bytes(), &receipt); err != nil || idempotent.Code != http.StatusOK || !receipt.OK || receipt.Preview.Existing != 1 {
		t.Fatalf("post-apply preview = %d %+v, %v", idempotent.Code, receipt, err)
	}

	if err := catalog.AssignThreadName("codex", "native-repair", "operator rename"); err != nil {
		t.Fatal(err)
	}
	nowNamed := httptest.NewRecorder()
	s.handleCreationNameRepair(nowNamed, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/creation-names", strings.NewReader(`{"apply":false,"records":`+records+`}`)))
	if nowNamed.Code != http.StatusConflict {
		t.Fatalf("named target preview = %d %s", nowNamed.Code, nowNamed.Body)
	}
}

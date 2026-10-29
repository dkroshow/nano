package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/history"
)

func TestHostedCreationPersistsNameBeforeAnyNativeBind(t *testing.T) {
	control, err := openTestMetadataStore(filepath.Join(t.TempDir(), "nano_threads.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	executions, err := newControlAgentExecutionStore(control, "creation-name-test")
	if err != nil {
		t.Fatal(err)
	}
	hosted, created, err := executions.BeginNamedSessionWithSubscriptionsAndPlacement(
		"creation-name-client", "request-hash", "codex", presentationShared, sidebarPlacementInvoker,
		"created before bind", agentSessionRecord{}, subscriptions.Choices{Inbox: subscriptions.None, Prompt: subscriptions.None},
	)
	if err != nil || !created || hosted.NativeThreadID != "" {
		t.Fatalf("create = %+v, created=%v, err=%v", hosted, created, err)
	}
	if got, ok := control.CreationName(hosted.ID); !ok || got != "created before bind" {
		t.Fatalf("canonical creation name = %q, %v", got, ok)
	}
}

func TestCreationThreadNamesProjectsConfirmedCurrentIdentity(t *testing.T) {
	control, err := openTestMetadataStore(filepath.Join(t.TempDir(), "nano_threads.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	executions, err := newControlAgentExecutionStore(control, "creation-name-projection")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := executions.ensureSession("nht_creation_projection", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.recordCreationName(hosted.ID, "created title"); err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession(hosted.ID, "nano", "native-projection", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}

	Records, err := control.CreationThreadNames(context.Background())
	if err != nil || len(Records) != 1 {
		t.Fatalf("creation projection records = %+v, %v", Records, err)
	}
	record := Records[0]
	if record.Provider != history.ProviderCodex || record.NativeThreadID != "native-projection" || record.Name != "created title" || record.Provenance != history.ThreadNameCreation || record.UpdatedAt == nil {
		t.Fatalf("creation projection record = %+v", record)
	}
}

package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/history"
)

func TestHostedCreationPersistsNameBeforeAnyNativeBind(t *testing.T) {
	control, err := openControlMetadataStore(filepath.Join(t.TempDir(), "nano_threads.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	executions, err := newControlAgentExecutionStore(control, "creation-name-test")
	if err != nil {
		t.Fatal(err)
	}
	hosted, created, err := executions.beginNamedSessionWithSubscriptionsAndPlacement(
		"creation-name-client", "request-hash", "codex", presentationShared, sidebarPlacementInvoker,
		"created before bind", agentSessionRecord{}, subscriptions.Choices{Inbox: subscriptions.None, Prompt: subscriptions.None},
	)
	if err != nil || !created || hosted.NativeThreadID != "" {
		t.Fatalf("create = %+v, created=%v, err=%v", hosted, created, err)
	}
	if got, ok := control.creationName(hosted.ID); !ok || got != "created before bind" {
		t.Fatalf("canonical creation name = %q, %v", got, ok)
	}
}

func TestCreationThreadNamesProjectsConfirmedCurrentIdentity(t *testing.T) {
	control, err := openControlMetadataStore(filepath.Join(t.TempDir(), "nano_threads.sqlite"))
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

	records, err := control.creationThreadNames(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatalf("creation projection records = %+v, %v", records, err)
	}
	record := records[0]
	if record.Provider != history.ProviderCodex || record.NativeThreadID != "native-projection" || record.Name != "created title" || record.Provenance != history.ThreadNameCreation || record.UpdatedAt == nil {
		t.Fatalf("creation projection record = %+v", record)
	}
}

func TestCreationNameSurvivesLaterBindWithoutOverwritingOperatorRename(t *testing.T) {
	control, err := openControlMetadataStore(filepath.Join(t.TempDir(), "nano_threads.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	executions, err := newControlAgentExecutionStore(control, "creation-name-test")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := executions.ensureSession("nht_creation_name", "claude", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.recordCreationName(hosted.ID, "creation name"); err != nil {
		t.Fatal(err)
	}

	catalog := &fakeThreadCatalog{names: map[string]string{}}
	server := &server{catalog: catalog}
	fixture := newProviderRuntimeFixture(t, sessionConfig{ID: "creation-name", Provider: "claude", HostedThreadID: hosted.ID, CreationName: func() string {
		name, _ := control.creationName(hosted.ID)
		return name
	}, NameThread: server.assignSpawnName, PublishCreationName: func(provider, nativeID, name string) {
		if err := catalog.PublishCreationNames(context.Background(), []creationNameProjectionRecord{{
			Provider: provider, NativeThreadID: nativeID, Name: name,
		}}); err != nil {
			t.Fatal(err)
		}
	}}, "native-after-restart", "")
	sess := fixture.session
	key := "claude\x00native-after-restart"
	if got := catalog.names[key]; got != "creation name" {
		t.Fatalf("bind name = %q, want creation name", got)
	}
	if got := catalog.nameSources[key]; got != history.ThreadNameCreation {
		t.Fatalf("bind provenance = %q, want creation", got)
	}

	if err := catalog.AssignThreadName("claude", "native-after-restart", "operator rename"); err != nil {
		t.Fatal(err)
	}
	emitClaudeInit(t, sess, fixture.claude, fixture.seq+1, "native-after-restart", "")
	if got := catalog.names[key]; got != "operator rename" {
		t.Fatalf("later bind restored %q over operator rename", got)
	}
}

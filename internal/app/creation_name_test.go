package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dkroshow/nano/internal/history"
)

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
	hosted, err := fixtureEnsureSession(executions, "nht_creation_name", "claude", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixtureRecordCreationName(control, hosted.ID, "creation name"); err != nil {
		t.Fatal(err)
	}

	catalog := &fakeThreadCatalog{names: map[string]string{}}
	server := &server{catalog: catalog}
	fixture := newProviderRuntimeFixture(t, sessionConfig{ID: "creation-name", Provider: "claude", HostedThreadID: hosted.ID, CreationName: func() string {
		name, _ := control.CreationName(hosted.ID)
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

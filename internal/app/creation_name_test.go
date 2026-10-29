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

	if err := executions.BindSession(hosted.ID, "nano", "native-after-restart", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	catalog := fixtureIdentityCatalog(t, nil)
	catalog.SetCreationNameSource(control.CreationThreadNames)
	server := &server{catalog: catalog, controlMetadata: control}
	fixture := newProviderRuntimeFixture(t, sessionConfig{ID: "creation-name", Provider: "claude", HostedThreadID: hosted.ID, CreationName: func() string {
		name, _ := control.CreationName(hosted.ID)
		return name
	}, NameThread: server.assignSpawnName, PublishCreationName: server.publishCreationName}, "native-after-restart", "")
	sess := fixture.session
	if got, _ := catalog.ThreadNameFor(context.Background(), "claude", "native-after-restart"); got != "creation name" {
		t.Fatalf("bind name = %q, want creation name", got)
	}
	if ref, _ := catalog.ThreadNameRefFor(context.Background(), "claude", "native-after-restart"); ref == nil || ref.Provenance != history.ThreadNameCreation {
		t.Fatalf("bind provenance = %+v, want creation", ref)
	}

	if err := catalog.AssignThreadName("claude", "native-after-restart", "operator rename"); err != nil {
		t.Fatal(err)
	}
	waitForCatalog(t, func() bool {
		name, _ := catalog.ThreadNameFor(context.Background(), "claude", "native-after-restart")
		return name == "operator rename"
	})
	emitClaudeInit(t, sess, fixture.claude, fixture.seq+1, "native-after-restart", "")
	if got, _ := catalog.ThreadNameFor(context.Background(), "claude", "native-after-restart"); got != "operator rename" {
		t.Fatalf("later bind restored %q over operator rename", got)
	}
}

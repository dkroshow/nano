package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/edge/stateopen"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

func TestCreationNameSurvivesLaterBindWithoutOverwritingOperatorRename(t *testing.T) {
	control, err := stateopen.OpenMetadata(filepath.Join(t.TempDir(), "nano_threads.sqlite"))
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
	catalog := creationNameCatalogBeforeFact(t, control.CreationThreadNames)
	if err := fixtureRecordCreationName(control, hosted.ID, "creation name"); err != nil {
		t.Fatal(err)
	}

	if err := executions.BindSession(hosted.ID, "nano", "native-after-restart", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	server := &server{catalog: catalog, controlMetadata: control}
	fixture := newProviderRuntimeFixture(t, sessionConfig{ID: "creation-name", Provider: "claude", HostedThreadID: hosted.ID, CreationName: func() string {
		name, _ := control.CreationName(hosted.ID)
		return name
	}, NameThread: func(provider, native, name string) {
		if err := catalog.AssignThreadNameIfEmpty(provider, native, name); err != nil {
			t.Fatal(err)
		}
	}, PublishCreationName: fixtureOwners(server).creationNames().Publish}, "native-after-restart", "")
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

// creationNameCatalogBeforeFact drains startup names work while the canonical
// source is empty. Later names can reach the catalog only through the bind
// publication under test, not a background refresh left over from setup.
func creationNameCatalogBeforeFact(t *testing.T, source func(context.Context) ([]history.ThreadNameRecord, error)) *sidebar.CatalogSource {
	t.Helper()
	if records, err := source(context.Background()); err != nil || len(records) != 0 {
		t.Fatalf("creation-name fixture must start without facts: %v, %v", records, err)
	}
	dir := t.TempDir()
	store, err := history.Open(filepath.Join(dir, "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	refreshed := make(chan struct{}, 1)
	catalog, err := newThreadCatalogService(context.Background(), store, threadindex.Options{
		Roots:     []history.SourceRoot{{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}},
		NamesPath: filepath.Join(dir, "names.json"), DisableWatcher: true,
		Logf: func(format string, args ...any) {
			if strings.HasPrefix(format, "thread catalog recovery delay=") {
				refreshed <- struct{}{}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(catalog.Close)
	catalog.SetCreationNameSource(source)
	catalog.RequestRefresh()
	select {
	case <-refreshed:
	case <-time.After(10 * time.Second):
		t.Fatal("catalog did not finish the empty creation-name refresh")
	}
	return catalog
}

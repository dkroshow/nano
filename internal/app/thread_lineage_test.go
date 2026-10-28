package app

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// lineageFixture is a real catalog and control store for the lineage pass:
// page rows whose supervisors sit outside the page, chosen through every
// precedence rule (a related-thread assignment, a standalone stop, a hosted
// invoker, a provider-native parent) and two levels deep.
type lineageFixture struct {
	budget *budgetFixture
	page   []catalogThread
}

func newLineageFixture(t *testing.T, extraWorkers int) *lineageFixture {
	t.Helper()
	ctx := context.Background()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}
	if _, err := store.EnsureRoot(ctx, root); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	recent, old := now.Add(-time.Hour), now.Add(-40*24*time.Hour)
	court := []historyCourt{{final: true, text: "Done."}}
	seed := func(native, parent string, kind history.ThreadKind, updated time.Time) {
		seedSidebarThread(t, store, root, native, parent, kind, updated, court)
	}
	// Page rows, newest first.
	seed("w-related", "", history.ThreadMain, recent)
	seed("w-standalone", "", history.ThreadMain, recent.Add(-time.Minute))
	seed("w-invoker", "", history.ThreadMain, recent.Add(-2*time.Minute))
	seed("w-native", "p-native", history.ThreadSubagent, recent.Add(-3*time.Minute))
	seed("w-dup", "p-native", history.ThreadSubagent, recent.Add(-4*time.Minute))
	seed("w-covered", "onpage-parent", history.ThreadSubagent, recent.Add(-5*time.Minute))
	seed("onpage-parent", "", history.ThreadMain, recent.Add(-6*time.Minute))
	for i := 0; i < extraWorkers; i++ {
		seed(fmt.Sprintf("w-extra-%03d", i), fmt.Sprintf("p-extra-%03d", i), history.ThreadSubagent, recent.Add(-time.Duration(10+i)*time.Minute))
	}
	// Supervisors outside the page; p-native has its own parent (depth two).
	seed("p-related", "", history.ThreadMain, old)
	seed("p-invoker", "", history.ThreadMain, old.Add(-time.Minute))
	seed("p-standalone-invoker", "", history.ThreadMain, old.Add(-2*time.Minute))
	seed("p-native", "g-native", history.ThreadSubagent, old.Add(-3*time.Minute))
	seed("g-native", "", history.ThreadMain, old.Add(-4*time.Minute))
	for i := 0; i < extraWorkers; i++ {
		seed(fmt.Sprintf("p-extra-%03d", i), "", history.ThreadMain, old.Add(-time.Duration(10+i)*time.Minute))
	}

	metadata, executions := newTestHostedControl(t)
	hosted := func(name, native string, invoker *agentSessionRecord) agentSessionRecord {
		var record agentSessionRecord
		var err error
		if invoker == nil {
			record, _, err = executions.beginSession(name, name, "codex", presentationShared)
		} else {
			record, _, err = executions.beginSessionWithInvoker(name, name, "codex", presentationShared, *invoker)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := executions.BindSession(record.ID, "nano-"+name, native, "ready", presentationShared); err != nil {
			t.Fatal(err)
		}
		record, _ = executions.session(record.ID)
		return record
	}
	relatedParent := hosted("p-related", "p-related", nil)
	invokerParent := hosted("p-invoker", "p-invoker", nil)
	standaloneInvoker := hosted("p-standalone-invoker", "p-standalone-invoker", nil)
	unrelatedInvoker := hosted("p-unrelated", "p-unrelated", nil)
	related := hosted("w-related", "w-related", &unrelatedInvoker)
	if _, err := metadata.appendOrchestrationAssignment(related.ID, "lineage-rel", &orchestrationAssignment{
		AssignmentRole: "auditor", Focus: "Audit", RelatedHostedThreadID: relatedParent.ID,
	}); err != nil {
		t.Fatal(err)
	}
	standalone := hosted("w-standalone", "w-standalone", &standaloneInvoker)
	if _, _, _, err := executions.setSidebarPlacement(standalone.ID, sidebarPlacementStandalone); err != nil {
		t.Fatal(err)
	}
	hosted("w-invoker", "w-invoker", &invokerParent)

	catalog := &countingCatalog{threadCatalogService: &threadCatalogService{store: store}}
	s := &server{catalog: catalog, sessionRegistry: sessionRegistry{sessions: map[string]*session{}}, controlMetadata: metadata, agentExecutions: executions}
	s.runtime = s
	f := &lineageFixture{budget: &budgetFixture{server: s, catalog: catalog, now: now}}
	from := now.Add(-2 * 24 * time.Hour)
	page, err := catalog.ListThreads(ctx, catalogListQuery{UpdatedFrom: &from, IncludeSubagents: true, Limit: 250})
	if err != nil {
		t.Fatal(err)
	}
	f.page = page.Items
	return f
}

func (f *lineageFixture) parents(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, row := range f.budget.server.lineageParentRows(context.Background(), catalogListQuery{}, f.page) {
		out = append(out, row.Provider+"/"+row.NativeID)
	}
	return out
}

// TestLineageParentsKeepPrecedenceAndOrder catches a batched lineage pass
// choosing a different parent or order: the related-thread assignment wins over
// the invoker, a standalone thread stops, a hosted invoker is followed, a
// provider-native parent is followed once for two workers, a parent already on
// the page is not repeated, and a parent's own parent is found, in the order
// the one-row-at-a-time walk produced. Boundary: lineageParentRows over a
// real store.
func TestLineageParentsKeepPrecedenceAndOrder(t *testing.T) {
	f := newLineageFixture(t, 0)
	want := []string{"codex/p-related", "codex/p-invoker", "codex/p-native", "codex/g-native"}
	if got := f.parents(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("lineage parents = %q, want %q", got, want)
	}
}

// TestLineageStatementsBoundedByDepth catches per-row work in the lineage
// pass: every returned row read its hosted record and its assignment, and
// every parent was a separate catalog read, and every live session is a
// returned row. Boundary: lineageParentRows over the same two-level page with
// 5 and with 50 extra workers (each with its own parent outside the page);
// control-store statements and catalog reads must not change.
func TestLineageStatementsBoundedByDepth(t *testing.T) {
	measure := func(extra int) (statements, catalogCalls int64) {
		f := newLineageFixture(t, extra)
		before := f.budget.catalog.calls()
		statements = f.budget.countControlStatements(t, func() { f.parents(t) })
		return statements, f.budget.catalog.calls() - before
	}
	smallStatements, smallCalls := measure(5)
	largeStatements, largeCalls := measure(50)
	if largeStatements != smallStatements || largeCalls != smallCalls {
		t.Fatalf("lineage grew with rows: 5 extra workers -> %d statements, %d catalog reads; 50 -> %d, %d",
			smallStatements, smallCalls, largeStatements, largeCalls)
	}
}

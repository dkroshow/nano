package history

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestReconcileRootBatchedAbsenceRequiresReadyWalk proves the restructured
// reconciliation: absence is derived from stored − ObservedPaths (not from the
// projected Sources), an observed-but-unprojected file (the Slice F unchanged
// fast path) never churns the catalog, and only a ready walk that stops
// observing a file marks it missing.
func TestReconcileRootBatchedAbsenceRequiresReadyWalk(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: filepath.Join(t.TempDir(), "projects")}
	root, err := store.EnsureRoot(ctx, rootSpec)
	if err != nil {
		t.Fatal(err)
	}
	source := testSource(rootSpec, "session.jsonl", "native", ThreadMain, nil)
	applied, err := store.ApplySource(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	path := source.Source.Path

	// A ready walk that OBSERVES the file but carries no projection must leave it
	// present and bump nothing — the unchanged file's observed-ness alone must
	// not be mistaken for a change.
	before, err := store.CatalogVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reconciled, err := store.ReconcileRoot(ctx, RootReconciliation{
		RootID: root.ID, ObservedPaths: []string{path}, State: IndexReady, CompletedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Changed || reconciled.SourceCount != 1 {
		t.Fatalf("observed-but-unprojected file changed the catalog: %#v", reconciled)
	}
	after, err := store.CatalogVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("catalog version bumped on a no-op cycle: %d -> %d", before, after)
	}
	detail, err := store.GetThread(ctx, applied.ThreadID)
	if err != nil || detail.Source.Status != SourcePresent {
		t.Fatalf("observed file was not left present: %#v, %v", detail, err)
	}

	// A degraded walk that no longer observes the file must NOT mark it missing:
	// a partial walk cannot prove absence.
	if _, err := store.ReconcileRoot(ctx, RootReconciliation{
		RootID: root.ID, State: IndexDegraded, CompletedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	detail, err = store.GetThread(ctx, applied.ThreadID)
	if err != nil || detail.Source.Status != SourcePresent {
		t.Fatalf("degraded walk marked an unobserved file missing: %#v, %v", detail, err)
	}

	// A ready targeted pass is also partial: it may update one observed file
	// but cannot mark another file missing from the rest of the root, and it
	// leaves the root's state, source count and completion time alone (a
	// state write per pass would put every open Explorer into scan polling).
	beforePartial, err := store.IndexStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileRoot(ctx, RootReconciliation{
		RootID: root.ID, Partial: true, State: IndexReady, CompletedAt: time.Now().Add(time.Hour).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	detail, err = store.GetThread(ctx, applied.ThreadID)
	if err != nil || detail.Source.Status != SourcePresent {
		t.Fatalf("partial ready pass marked an unobserved file missing: %#v, %v", detail, err)
	}
	afterPartial, err := store.IndexStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before, after := beforePartial.Roots[0], afterPartial.Roots[0]; before.State != after.State || before.SourceCount != after.SourceCount ||
		!reflect.DeepEqual(before.LastCompletedAt, after.LastCompletedAt) {
		t.Fatalf("partial pass rewrote root index state: before %#v, after %#v", before, after)
	}

	// A ready walk that no longer observes the file marks it missing via the
	// batched observed-set comparison.
	reconciled, err = store.ReconcileRoot(ctx, RootReconciliation{
		RootID: root.ID, State: IndexReady, CompletedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reconciled.Changed || reconciled.SourceCount != 0 {
		t.Fatalf("ready walk did not detect absence: %#v", reconciled)
	}
	detail, err = store.GetThread(ctx, applied.ThreadID)
	if err != nil || detail.Source.Status != SourceMissing || detail.Sources[0].Status != SourceMissing {
		t.Fatalf("removed file not marked missing: %#v, %v", detail, err)
	}
}

// TestReconcileRootRejectsProjectionOutsideObservedSet guards the invariant that
// every projected source was actually observed by the walk.
func TestReconcileRootRejectsProjectionOutsideObservedSet(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	root, err := store.EnsureRoot(ctx, rootSpec)
	if err != nil {
		t.Fatal(err)
	}
	source := testSource(rootSpec, "one.jsonl", "native-one", ThreadMain, nil)
	_, err = store.ReconcileRoot(ctx, RootReconciliation{
		RootID: root.ID, ObservedPaths: []string{filepath.Join(rootSpec.Path, "other.jsonl")},
		Sources: []SourceProjection{source}, State: IndexReady, CompletedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("reconciliation accepted a projection whose path was never observed")
	}
}

// TestLoadSourcesByRootReturnsWholeRoot proves the batched read returns every
// stored source keyed by canonical path in a single query.
func TestLoadSourcesByRootReturnsWholeRoot(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	root, err := store.EnsureRoot(ctx, rootSpec)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"a.jsonl", "b.jsonl", "c.jsonl"}
	for i, name := range names {
		if _, err := store.ApplySource(ctx, testSource(rootSpec, name, fmt.Sprintf("native-%d", i), ThreadMain, nil)); err != nil {
			t.Fatal(err)
		}
	}
	byPath, err := store.LoadSourcesByRoot(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(byPath) != len(names) {
		t.Fatalf("LoadSourcesByRoot returned %d sources, want %d", len(byPath), len(names))
	}
	for _, name := range names {
		path := filepath.Join(rootSpec.Path, name)
		stored, ok := byPath[path]
		if !ok || stored.Source.Path != path || stored.Status != SourcePresent {
			t.Fatalf("LoadSourcesByRoot missing or wrong entry for %q: %#v", path, stored)
		}
	}
}

// TestReconcileNoOpCycleIssuesNoPerFileStatements is the statement-count gate:
// a steady-state reconcile of an all-unchanged root must issue a bounded number
// of statements independent of catalog size. It counts every statement reaching
// the driver and asserts the count for 4 and 400 sources is identical (and
// small), which can only hold if no per-file statement runs.
func TestReconcileNoOpCycleIssuesNoPerFileStatements(t *testing.T) {
	registerCountingDriver(t)
	measure := func(sourceCount int) int64 {
		ctx := context.Background()
		store, err := openStore(countingDriverName, filepath.Join(t.TempDir(), "history.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		rootSpec := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
		root, err := store.EnsureRoot(ctx, rootSpec)
		if err != nil {
			t.Fatal(err)
		}
		paths := make([]string, 0, sourceCount)
		for i := 0; i < sourceCount; i++ {
			name := fmt.Sprintf("session-%04d.jsonl", i)
			if _, err := store.ApplySource(ctx, testSource(rootSpec, name, fmt.Sprintf("native-%04d", i), ThreadMain, nil)); err != nil {
				t.Fatal(err)
			}
			paths = append(paths, filepath.Join(rootSpec.Path, name))
		}
		// Settle index_state so the measured cycle is a pure steady-state no-op.
		if _, err := store.ReconcileRoot(ctx, RootReconciliation{
			RootID: root.ID, ObservedPaths: paths, State: IndexReady, CompletedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		atomic.StoreInt64(&stmtCounter, 0)
		reconciled, err := store.ReconcileRoot(ctx, RootReconciliation{
			RootID: root.ID, ObservedPaths: paths, State: IndexReady, CompletedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if reconciled.Changed {
			t.Fatalf("steady-state reconcile of %d sources reported a change", sourceCount)
		}
		if reconciled.SourceCount != sourceCount {
			t.Fatalf("reconciled.SourceCount = %d, want %d", reconciled.SourceCount, sourceCount)
		}
		return atomic.LoadInt64(&stmtCounter)
	}
	small := measure(4)
	large := measure(400)
	if small != large {
		t.Fatalf("no-op reconcile statement count scaled with catalog size: %d (4 sources) vs %d (400 sources)", small, large)
	}
	if large > 8 {
		t.Fatalf("no-op reconcile issued %d statements; expected a small constant with zero per-file work", large)
	}
}

const countingDriverName = "sqlite-counting"

// stmtCounter tallies statements executed through the counting driver. Tests in
// this package run sequentially, and each measurement resets it, so a package
// global is safe.
var (
	stmtCounter          int64
	stmtArgumentBytes    int64
	countingRegisterOnce sync.Once
)

func registerCountingDriver(t *testing.T) {
	t.Helper()
	countingRegisterOnce.Do(func() {
		probe, err := sql.Open("sqlite", "file:counting-probe?mode=memory")
		if err != nil {
			t.Fatalf("probe sqlite driver: %v", err)
		}
		base := probe.Driver()
		if err := probe.Close(); err != nil {
			t.Fatalf("close probe: %v", err)
		}
		sql.Register(countingDriverName, countingDriver{inner: base, counter: &stmtCounter})
	})
}

// countingDriver wraps the registered sqlite driver and increments a counter for
// every statement that reaches the database. modernc's conn implements neither
// the context nor the Execer/Queryer fast paths, so database/sql routes all
// statements through a prepared stmt — wrapping Stmt.Exec/Query is sufficient to
// count every statement.
type countingDriver struct {
	inner   driver.Driver
	counter *int64
}

func (d countingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return countingConn{inner: conn, counter: d.counter}, nil
}

type countingConn struct {
	inner   driver.Conn
	counter *int64
}

func (c countingConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.inner.Prepare(query)
	if err != nil {
		return nil, err
	}
	return countingStmt{inner: stmt, counter: c.counter}, nil
}

func (c countingConn) Close() error              { return c.inner.Close() }
func (c countingConn) Begin() (driver.Tx, error) { return c.inner.Begin() }

type countingStmt struct {
	inner   driver.Stmt
	counter *int64
}

func (s countingStmt) Close() error  { return s.inner.Close() }
func (s countingStmt) NumInput() int { return s.inner.NumInput() }

func (s countingStmt) Exec(args []driver.Value) (driver.Result, error) {
	atomic.AddInt64(s.counter, 1)
	for _, arg := range args {
		switch value := arg.(type) {
		case string:
			atomic.AddInt64(&stmtArgumentBytes, int64(len(value)))
		case []byte:
			atomic.AddInt64(&stmtArgumentBytes, int64(len(value)))
		}
	}
	return s.inner.Exec(args)
}

func (s countingStmt) Query(args []driver.Value) (driver.Rows, error) {
	atomic.AddInt64(s.counter, 1)
	return s.inner.Query(args)
}

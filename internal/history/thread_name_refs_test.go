package history

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// TestThreadNameRefsMatchOneIDReadsInChunkedReads catches the batch name
// reader disagreeing with the one-id reader it stands in for on list paths, or
// costing more than one statement per 500 pairs, or scanning the whole names
// table. Boundary: ThreadNameRefs against
// ThreadNameRefFor on one store: manual, creation and imported provenance, a
// missing pair, the same native id under two providers (and under a third
// that holds no name), a duplicate and an empty identity, and 1,106 distinct
// pairs (three reads); and the query plan of the one name query that every
// overlay (list, detail, sidebar graph, load by ids) shares.
func TestThreadNameRefsMatchOneIDReadsInChunkedReads(t *testing.T) {
	registerCountingDriver(t)
	ctx := context.Background()
	store, err := openStore(countingDriverName, filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records := []ThreadNameRecord{
		{Provider: ProviderCodex, NativeThreadID: "native-a", Name: "A manual", Provenance: ThreadNameManual},
		{Provider: ProviderClaude, NativeThreadID: "native-a", Name: "A claude", Provenance: ThreadNameManual},
		{Provider: ProviderCodex, NativeThreadID: "native-b", Name: "B creation", Provenance: ThreadNameCreation},
		{Provider: ProviderCodex, NativeThreadID: "native-c", Name: "C import", Provenance: ThreadNameHistoricalImport},
	}
	identities := []ThreadIdentity{
		{ProviderCodex, "native-a"}, {ProviderClaude, "native-a"}, {ProviderGrok, "native-a"},
		{ProviderCodex, "native-b"}, {ProviderCodex, "native-c"}, {ProviderCodex, "native-missing"},
		{ProviderCodex, ""}, {ProviderCodex, "native-a"},
	}
	for i := 0; i < 1100; i++ {
		native := fmt.Sprintf("bulk-%04d", i)
		identities = append(identities, ThreadIdentity{ProviderCodex, native})
		if i%3 != 0 {
			records = append(records, ThreadNameRecord{Provider: ProviderCodex, NativeThreadID: native, Name: "Bulk " + native, Provenance: ThreadNameManual})
		}
	}
	if _, err := store.SyncThreadNames(ctx, records); err != nil {
		t.Fatal(err)
	}

	want := map[ThreadIdentity]ThreadNameRef{}
	for _, identity := range identities {
		ref, err := store.ThreadNameRefFor(ctx, identity.Provider, identity.NativeThreadID)
		if err != nil {
			t.Fatal(err)
		}
		if ref != nil {
			want[identity] = *ref
		}
	}
	atomic.StoreInt64(&stmtCounter, 0)
	got, err := store.ThreadNameRefs(ctx, identities)
	statements := atomic.LoadInt64(&stmtCounter)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batch names differ from the one-id reads: got %d, want %d; codex/native-a = %#v, claude/native-a = %#v",
			len(got), len(want), got[identities[0]], got[identities[1]])
	}
	if len(want) != 4+733 || want[identities[3]].Provenance != ThreadNameCreation || want[identities[4]].Provenance != ThreadNameHistoricalImport {
		t.Fatalf("the reference reads found %d names; the fixture is not exercising every case", len(want))
	}
	if statements != 3 {
		t.Fatalf("1,106 distinct pairs took %d statements, want 3 (500, 500, 106)", statements)
	}
	atomic.StoreInt64(&stmtCounter, 0)
	if _, err := store.ThreadNameRefs(ctx, nil); err != nil || atomic.LoadInt64(&stmtCounter) != 0 {
		t.Fatalf("empty input: %d statements, %v; want none", atomic.LoadInt64(&stmtCounter), err)
	}
	clause, args := identityFilter(identities[:3])
	plan, err := store.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+threadNameSelect+clause, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	var steps []string
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, detail)
	}
	if joined := strings.Join(steps, "; "); !strings.Contains(joined, "SEARCH thread_names USING") || strings.Contains(joined, "SCAN thread_names") {
		t.Fatalf("the name query does not search thread_names by key: %s", joined)
	}
}

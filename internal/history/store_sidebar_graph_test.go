package history

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestListSidebarGraphReturnsEligibleIdentityAndCourtInOneSnapshot(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: t.TempDir()}
	if _, err := store.EnsureRoot(ctx, root); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-2 * 24 * time.Hour)
	old := now.Add(-40 * 24 * time.Hour)
	recentID := fleetCourtThread(t, store, root, "recent.jsonl", "native-recent", ThreadMain, &recent, []fleetCourtItem{
		{itemType: ItemUserMessage, text: "please continue"},
	})
	oldID := fleetCourtThread(t, store, root, "old.jsonl", "native-old", ThreadMain, &old, []fleetCourtItem{
		{itemType: ItemAgentMessage, final: true, text: "Done."},
	})
	childUpdated := now.Add(-time.Hour)
	childID := fleetCourtThread(t, store, root, "child.jsonl", "native-child", ThreadSubagent, &childUpdated, []fleetCourtItem{
		{itemType: ItemAgentMessage, sidechain: true, final: true, text: "working"},
	})
	if _, err := store.db.Exec(`UPDATE threads SET parent_native_id = ? WHERE id = ?`, "native-recent", childID); err != nil {
		t.Fatal(err)
	}

	updatedFrom := now.Add(-30 * 24 * time.Hour)
	graph, err := store.ListSidebarGraph(ctx, SidebarGraphParams{
		List: ListThreadsParams{
			IncludeSubagents:    true,
			IncludeProgrammatic: false,
			Archived:            ArchivedInclude,
			UpdatedFrom:         &updatedFrom,
		},
	})
	if err != nil {
		t.Fatalf("ListSidebarGraph: %v", err)
	}
	byID := map[string]SidebarGraphRow{}
	for _, row := range graph.Rows {
		byID[row.Thread.ID] = row
	}
	if _, ok := byID[oldID]; ok {
		t.Fatalf("idle row outside 30-day window leaked into eligible graph: %+v", byID[oldID])
	}
	recentRow, ok := byID[recentID]
	if !ok {
		t.Fatalf("recent row missing: %+v", graph.Rows)
	}
	if recentRow.Thread.NativeID != "native-recent" || recentRow.Court.ItemType != ItemUserMessage {
		t.Fatalf("recent graph row = %+v", recentRow)
	}
	childRow, ok := byID[childID]
	if !ok {
		t.Fatalf("child row missing: %+v", graph.Rows)
	}
	if childRow.Thread.ParentNativeID != "native-recent" {
		t.Fatalf("child parent = %q", childRow.Thread.ParentNativeID)
	}
	if graph.CatalogVersion == 0 {
		t.Fatal("catalog version missing from sidebar graph snapshot")
	}
}

func TestListSidebarGraphRescuesExtraIdentitiesOutsideTheWindow(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: t.TempDir()}
	if _, err := store.EnsureRoot(ctx, root); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	old := now.Add(-80 * 24 * time.Hour)
	starredID := fleetCourtThread(t, store, root, "starred.jsonl", "native-starred", ThreadMain, &old, []fleetCourtItem{
		{itemType: ItemUserMessage, text: "pin me"},
	})
	updatedFrom := now.Add(-30 * 24 * time.Hour)
	graph, err := store.ListSidebarGraph(ctx, SidebarGraphParams{
		List: ListThreadsParams{
			IncludeSubagents:    true,
			IncludeProgrammatic: false,
			Archived:            ArchivedInclude,
			UpdatedFrom:         &updatedFrom,
		},
		Extra: []SidebarGraphKey{{Provider: ProviderClaude, NativeID: "native-starred"}},
	})
	if err != nil {
		t.Fatalf("ListSidebarGraph: %v", err)
	}
	found := false
	for _, row := range graph.Rows {
		if row.Thread.ID == starredID {
			found = true
			if row.Court.ItemType != ItemUserMessage {
				t.Fatalf("rescued court = %+v", row.Court)
			}
		}
	}
	if !found {
		t.Fatalf("starred rescue missing: %+v", graph.Rows)
	}
}

func TestListSidebarGraphLoadsFullThreadsOnlyForRequestedIDs(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: t.TempDir()}
	if _, err := store.EnsureRoot(ctx, root); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	keepID := fleetCourtThread(t, store, root, "keep.jsonl", "native-keep", ThreadMain, &now, nil)
	otherID := fleetCourtThread(t, store, root, "other.jsonl", "native-other", ThreadMain, &now, nil)
	threads, err := store.LoadThreadsByIDs(ctx, []string{keepID})
	if err != nil {
		t.Fatalf("LoadThreadsByIDs: %v", err)
	}
	if len(threads) != 1 || threads[0].ID != keepID || threads[0].NativeID != "native-keep" {
		t.Fatalf("loaded = %+v", threads)
	}
	for _, thread := range threads {
		if thread.ID == otherID {
			t.Fatal("excluded history materialized")
		}
	}
}

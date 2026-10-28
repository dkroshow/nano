package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// Failure caught: throughput that counts what was not committed (a failed
// apply, a rebuild's whole projection, an unchanged pass), a minute served
// while it is still open, or minute history that grows without bound.
// Boundary: the minutes GET /api/v1/monitor serves.
func TestIngestMinutesCountOnlyCommittedWorkAndKeepTenClosedMinutes(t *testing.T) {
	var metrics ingestMetrics
	base := time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC)
	source := func(at time.Time, prepareMs, saveMs int) *ingestOutcome {
		since := at.Add(-time.Duration(prepareMs+saveMs+10) * time.Millisecond)
		prepareStart := since.Add(10 * time.Millisecond)
		prepareEnd := prepareStart.Add(time.Duration(prepareMs) * time.Millisecond)
		return &ingestOutcome{provider: history.ProviderCodex, found: true, take: dirtyTake{since: since},
			prepareStart: prepareStart, prepareEnd: prepareEnd, savedAt: prepareEnd.Add(time.Duration(saveMs) * time.Millisecond), bytesRead: 300}
	}
	appended := source(base, 4, 30)
	appended.advance = transcriptAdvance{applied: true, startOffset: 1000, endOffset: 1300, records: 2}
	metrics.recordSource(appended, base)
	rebuilt := source(base, 400, 900)
	rebuilt.rebuild = true
	rebuilt.advance = transcriptAdvance{applied: true, rebuild: true, endOffset: 50_000, records: 120}
	metrics.recordSource(rebuilt, base)
	failed := source(base, 5, 0)
	failed.savedAt = time.Time{}
	failed.fail(ingestErrorApply, errors.New("database is locked"))
	metrics.recordSource(failed, base)
	metrics.recordSource(&ingestOutcome{provider: history.ProviderCodex, found: true}, base)
	metrics.recordSource(&ingestOutcome{provider: history.ProviderCodex, found: true, err: history.ErrTranscriptChanged, errKind: ingestErrorIdentity}, base)
	metrics.recordPublication(history.ProviderCodex, 3*time.Millisecond, 40*time.Millisecond, base)

	if open := metrics.report(base); len(open) != 0 {
		t.Fatalf("the open minute was served: %#v", open)
	}
	served := metrics.report(base.Add(time.Minute))
	if len(served) != 1 {
		t.Fatalf("served %d minutes, want 1", len(served))
	}
	codex := served[0].Providers["codex"]
	want := ingestCounts{Passes: 5, Unchanged: 1, BytesRead: 900, CommittedBytes: 300, CommittedRecords: 2, Rebuilds: 1, RebuildBytes: 50_000,
		Errors: map[string]int{"apply": 1, "identity_retry": 1}}
	if codex.Passes != want.Passes || codex.Unchanged != want.Unchanged || codex.BytesRead != want.BytesRead ||
		codex.CommittedBytes != want.CommittedBytes || codex.CommittedRecords != want.CommittedRecords ||
		codex.Rebuilds != want.Rebuilds || codex.RebuildBytes != want.RebuildBytes ||
		codex.Errors["apply"] != 1 || codex.Errors["identity_retry"] != 1 {
		t.Fatalf("counts = %+v, want %+v", codex.ingestCounts, want)
	}
	if prepare := codex.Stages["append.prepare"]; prepare.Count != 2 || prepare.MedianMS != 5 || prepare.MaxMS != 5 {
		t.Fatalf("append prepare = %+v, want two samples in the 5 ms bucket", prepare)
	}
	if save := codex.Stages["append.save"]; save.Count != 1 || save.MedianMS != 30 {
		t.Fatalf("append save = %+v, want the one committed append at 30 ms (bucket bound capped by its max)", save)
	}
	if rebuild := codex.Stages["rebuild.save"]; rebuild.Count != 1 || rebuild.MaxMS != 900 {
		t.Fatalf("rebuild save = %+v, want the rebuild reported apart", rebuild)
	}
	if publish := codex.Stages["publish.run"]; publish.Count != 1 || publish.MaxMS != 40 {
		t.Fatalf("publish run = %+v", publish)
	}

	for minute := 1; minute <= 12; minute++ {
		metrics.recordRecovery(base.Add(time.Duration(minute)*time.Minute), 3*time.Second)
	}
	retained := metrics.report(base.Add(13 * time.Minute))
	if len(retained) != ingestMinutesKept || !retained[0].Minute.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("retained %d minutes from %v, want the newest %d closed", len(retained), retained[0].Minute, ingestMinutesKept)
	}
	if recovery := retained[len(retained)-1].Recovery; recovery == nil || recovery.Count != 1 || recovery.MaxMS != 3000 {
		t.Fatalf("recovery = %+v, want the explicit walk reported apart", recovery)
	}
}

// Failure caught: a long rebuild reported stalled before its single commit,
// or dirty work with no progress never reported. Boundary: the monitor's
// current ingest view of the dirty queue.
func TestIngestStatusStallNeedsNoReadAndNoCommit(t *testing.T) {
	service := &threadCatalogService{dirty: map[string]*dirtySource{}, owners: map[string]*attachedOwner{}, held: map[string]*heldSource{}}
	old := time.Now().Add(-time.Minute)
	reading := &dirtySource{since: old, running: true}
	reading.readAt.Store(ingestNanos())
	service.dirty["/reading"] = reading
	service.dirty["/stuck"] = &dirtySource{since: old, running: true}
	service.dirty["/fresh"] = &dirtySource{since: time.Now(), queued: true}
	service.held["/stuck"] = &heldSource{observed: true, committed: true, size: 900, readOffset: 700, completeOffset: 650}
	status := service.ingestStatus()
	if status.StalledSources != 1 || status.InFlight != 2 || status.Queued != 1 {
		t.Fatalf("status = stalled %d, in flight %d, queued %d; want 1, 2, 1", status.StalledSources, status.InFlight, status.Queued)
	}
	if status.UnreadBytes != 200 || status.PartialLineBytes != 50 || status.OldestPendingMS == nil || *status.OldestPendingMS < 59_000 {
		t.Fatalf("backlog = unread %d, partial %d, oldest %v", status.UnreadBytes, status.PartialLineBytes, status.OldestPendingMS)
	}
	if status.Watch.Mode != "unavailable" {
		t.Fatalf("watch mode = %q with no watcher and no reason, want unavailable", status.Watch.Mode)
	}
}

// Failure caught: a repair rebuilt inside the transcript step (an errored
// checkpoint) reported zero preparation instants and impossible durations
// and counted as unchanged; and a fast transcript-change callback finished
// before its receipt existed, so the receipt never showed publication.
// Boundary: a held file's receipt and the served minutes after real passes.
func TestIngestReceiptsCoverRepairAndFastPublication(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "projects")
	project := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeID := "63000000-0000-4000-8000-000000000001"
	path := filepath.Join(project, nativeID+".jsonl")
	writeClaudeCatalogFixture(t, path, nativeID, "2026-10-03T05:00:00Z", "first")
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := newThreadCatalogService(context.Background(), store, threadCatalogServiceOptions{
		Roots: []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}}, DisableWatcher: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.setTranscriptChanged(func(context.Context, transcriptChange) {}) // as fast as a callback gets
	service.holdAttachedSource("owner", []string{path})
	<-service.attachedCatchUpFor("owner").done
	published := func() ingestReceipt {
		t.Helper()
		var receipt ingestReceipt
		waitForCatalog(t, func() bool {
			if backlog, running := service.transcriptChangeStatus(); backlog != 0 || running != 0 {
				return false
			}
			receipts := service.ingestReceipts()
			if len(receipts) != 1 {
				return false
			}
			receipt = receipts[0]
			return true
		})
		if receipt.PublishedAt == nil || receipt.PublishStartedAt.Before(receipt.SavedAt) {
			t.Fatalf("receipt after its callback finished has no publication after save: %+v", receipt)
		}
		return receipt
	}
	first := published()

	if err := store.MarkTranscriptError(context.Background(), first.ThreadID, first.SourceID, "injected", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Publication can finish before completeIngest records the pass metrics.
	// Attachment readiness waits for that pass, rather than just its receipt.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !service.AwaitAttachedCurrent("owner", true, nil, ctx.Done()) {
		t.Fatal("repair did not finish its attachment catch-up")
	}
	repair := published()
	if repair.ProjectionKey == first.ProjectionKey {
		t.Fatal("repair retained the failed projection")
	}
	if repair.Mode != "rebuild" || repair.PrepareStartedAt.Before(repair.HintAt) || repair.PreparedAt.Before(repair.PrepareStartedAt) ||
		repair.SavedAt.Before(repair.PreparedAt) || repair.QueueMS < 0 || repair.PrepareMS <= 0 || repair.SaveMS < 0 || repair.SaveMS > 60_000 {
		t.Fatalf("repair receipt stages = %+v", repair)
	}
	var rebuilds, unchanged, saves int
	for _, minute := range service.ingest.report(time.Now().Add(time.Minute)) { // the current minute, as if closed
		claude := minute.Providers["claude"]
		rebuilds, unchanged, saves = rebuilds+claude.Rebuilds, unchanged+claude.Unchanged, saves+claude.Stages["rebuild.save"].Count
	}
	if rebuilds != 2 || saves != 2 || unchanged != 0 {
		t.Fatalf("served minutes: rebuilds %d, rebuild saves %d, unchanged %d; want the catch-up and the repair as rebuilds", rebuilds, saves, unchanged)
	}
}

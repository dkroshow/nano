//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// attachedCatalog is a real service with real file watches over one Claude
// root. Nothing in these tests hints a path: every ingestion follows a
// kernel event on a held file.
type attachedCatalog struct {
	service *threadCatalogService
	store   *history.Store
	root    string
	project string
}

func newAttachedCatalog(t *testing.T, store *history.Store, root string) *attachedCatalog {
	t.Helper()
	if store == nil {
		var err error
		if store, err = history.Open(filepath.Join(t.TempDir(), "history.sqlite")); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
	}
	if root == "" {
		root = filepath.Join(t.TempDir(), ".claude", "projects")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	service, err := newThreadCatalogService(context.Background(), store, threadCatalogServiceOptions{
		Roots: []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: root}}, Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	service.setTranscriptChanged(func(context.Context, transcriptChange) {})
	if service.files == nil {
		t.Fatalf("no file watcher: %v", service.filesErr)
	}
	return &attachedCatalog{service: service, store: store, root: root, project: filepath.Join(root, "-workspace")}
}

func (c *attachedCatalog) catchUp(t *testing.T, owner string) catchUpResult {
	t.Helper()
	catchUp := c.service.attachedCatchUpFor(owner)
	if catchUp == nil {
		t.Fatalf("owner %s holds nothing", owner)
	}
	select {
	case <-catchUp.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("owner %s did not catch up", owner)
	}
	return c.service.catchUpResultOf(catchUp)
}

// texts returns the saved transcript's row texts, failing on a duplicated
// durable row.
func (c *attachedCatalog) texts(t *testing.T, nativeID string) []string {
	t.Helper()
	id := catalogIDFor(t, c.service, nativeID)
	if id == "" {
		return nil
	}
	page, err := c.store.ReadTranscript(context.Background(), history.TranscriptReadRequest{ThreadID: id, Limit: history.MaxTranscriptPageLimit})
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var texts []string
	for _, item := range page.Items {
		if seen[item.ID] {
			t.Fatalf("durable row %s saved twice", item.ID)
		}
		seen[item.ID] = true
		if item.Text != "" {
			texts = append(texts, item.Text)
		}
	}
	return texts
}

// writeUserRows writes a Claude transcript of user rows only, so every saved
// row is one of the prompts.
func writeUserRows(t *testing.T, path, nativeID string, prompts ...string) {
	t.Helper()
	var body string
	for _, prompt := range prompts {
		body += claudeCatalogUserRow(nativeID, "2026-10-03T01:00:00Z", prompt)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (c *attachedCatalog) waitTexts(t *testing.T, nativeID string, want []string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := c.texts(t, nativeID)
		if fmt.Sprint(got) == fmt.Sprint(want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("saved rows = %q, want %q", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Failure caught: an append through a writer that keeps its descriptor open
// (the Codex app server's rollouts) was never seen without a timer; a
// partial final line became a row early or twice; a burst lost or repeated
// rows; a restart lost the partial line's continuation. Boundary: a held
// file through kqueue, the catalog worker and the saved transcript, with no
// hint from any wire.
func TestAttachedHeldWriterPartialLineBurstAndRestart(t *testing.T) {
	c := newAttachedCatalog(t, nil, "")
	if err := os.MkdirAll(c.project, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeID := "51000000-0000-4000-8000-000000000001"
	path := filepath.Join(c.project, nativeID+".jsonl")
	writeUserRows(t, path, nativeID, "prompt 0")
	writer, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	c.service.holdAttachedSource("session-1", []string{path})
	if result := c.catchUp(t, "session-1"); result != catchUpReady {
		t.Fatalf("catch-up = %v, want ready", result)
	}
	want := []string{"prompt 0"}
	c.waitTexts(t, nativeID, want)

	write := func(s string) {
		if _, err := writer.WriteString(s); err != nil {
			t.Fatal(err)
		}
	}
	row := claudeCatalogUserRow(nativeID, "2026-10-03T01:01:00Z", "prompt 1")
	write(row[:len(row)/2])
	time.Sleep(300 * time.Millisecond)
	if got := c.texts(t, nativeID); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("a partial line became a row: %q", got)
	}
	write(row[len(row)/2:])
	want = append(want, "prompt 1")
	c.waitTexts(t, nativeID, want)

	for i := 2; i < 52; i++ {
		write(claudeCatalogUserRow(nativeID, "2026-10-03T01:02:00Z", fmt.Sprintf("prompt %d", i)))
		want = append(want, fmt.Sprintf("prompt %d", i))
	}
	c.waitTexts(t, nativeID, want)

	// Restart with half a line written: the restarted catch-up keeps the
	// continuation, and the completed line becomes exactly one row.
	row = claudeCatalogUserRow(nativeID, "2026-10-03T01:03:00Z", "after restart")
	write(row[:10])
	time.Sleep(300 * time.Millisecond)
	c.service.Close()
	restarted := newAttachedCatalog(t, c.store, c.root)
	restarted.service.holdAttachedSource("session-2", []string{path})
	if result := restarted.catchUp(t, "session-2"); result != catchUpReady {
		t.Fatalf("restart catch-up = %v, want ready", result)
	}
	write(row[10:])
	restarted.waitTexts(t, nativeID, append(want, "after restart"))
}

// Failure caught: at boot every re-attached thread waited for the one catalog
// worker to pass its file, two files a pass, so readiness trailed the whole
// queue even for files that had not changed since they were saved. An
// unchanged saved file is proven current at hold time; a file that grew while
// Nano was down still waits for a pass. Boundary: holdAttachedSource on a
// restarted service whose worker cannot run a pass.
func TestAttachedUnchangedFileCatchesUpWithoutWaitingForAPass(t *testing.T) {
	c := newAttachedCatalog(t, nil, "")
	if err := os.MkdirAll(c.project, 0o700); err != nil {
		t.Fatal(err)
	}
	unchangedID, grownID := "53000000-0000-4000-8000-000000000001", "53000000-0000-4000-8000-000000000002"
	unchanged, grown := filepath.Join(c.project, unchangedID+".jsonl"), filepath.Join(c.project, grownID+".jsonl")
	writeUserRows(t, unchanged, unchangedID, "saved before restart")
	writeUserRows(t, grown, grownID, "saved before restart")
	c.service.holdAttachedSource("before-unchanged", []string{unchanged})
	c.service.holdAttachedSource("before-grown", []string{grown})
	if c.catchUp(t, "before-unchanged") != catchUpReady || c.catchUp(t, "before-grown") != catchUpReady {
		t.Fatal("first catch-up did not finish")
	}
	c.service.Close()
	writer, err := os.OpenFile(grown, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString(claudeCatalogUserRow(grownID, "2026-10-03T01:05:00Z", "written while down")); err != nil {
		t.Fatal(err)
	}
	writer.Close()

	restarted := newAttachedCatalog(t, c.store, c.root)
	restarted.service.projectionMu.Lock() // no pass can run
	restarted.service.holdAttachedSource("unchanged", []string{unchanged})
	restarted.service.holdAttachedSource("grown", []string{grown})
	select {
	case <-restarted.service.attachedCatchUpFor("unchanged").done:
	case <-time.After(2 * time.Second):
		restarted.service.projectionMu.Unlock()
		t.Fatal("an unchanged saved file waited for a catalog pass to be proven current")
	}
	select {
	case <-restarted.service.attachedCatchUpFor("grown").done:
		restarted.service.projectionMu.Unlock()
		t.Fatal("a file that grew while Nano was down was proven current without a pass")
	case <-time.After(200 * time.Millisecond):
	}
	restarted.service.projectionMu.Unlock()
	if restarted.catchUp(t, "unchanged") != catchUpReady || restarted.catchUp(t, "grown") != catchUpReady {
		t.Fatal("restarted catch-up did not finish ready")
	}
	restarted.waitTexts(t, grownID, []string{"saved before restart", "written while down"})
}

// Failure caught: an attached thread whose file did not exist yet, or was
// replaced, stayed stale until a timer or a root walk, or read as caught up
// before its file was written; a released thread kept being watched; an
// unattached file was imported automatically. Boundary: held paths through
// kqueue's directory waits and rebinds.
func TestAttachedAbsentReplacedReleasedAndUnattached(t *testing.T) {
	c := newAttachedCatalog(t, nil, "")
	// This test owns the kqueue boundary: outside discovery
	// (TestOutsideSessionIsDiscoveredWithoutRefresh) would otherwise import
	// its unattached file.
	c.service.discovery.Close()
	nativeID := "52000000-0000-4000-8000-000000000001"
	path := filepath.Join(c.project, nativeID+".jsonl")
	c.service.holdAttachedSource("session", []string{path})
	pending := c.service.attachedCatchUpFor("session")
	waitForCatalogWithin(t, 10*time.Second, func() bool { return c.service.passedSince(pending.after) })
	if stats := c.service.files.Stats(); stats.Waiting != 1 || stats.Directories != 1 {
		t.Fatalf("watch stats = %+v, want the file waiting on one directory", stats)
	}
	select {
	case <-pending.done:
		t.Fatal("an absent file finished its catch-up")
	default:
	}

	// The project directory and the file appear later, with no hint: the
	// watch's pass finishes the same catch-up.
	if err := os.MkdirAll(c.project, 0o700); err != nil {
		t.Fatal(err)
	}
	writeUserRows(t, path, nativeID, "created later")
	if result := c.catchUp(t, "session"); result != catchUpReady {
		t.Fatalf("catch-up once the file was written = %v", result)
	}
	c.waitTexts(t, nativeID, []string{"created later"})

	// An unattached file in the same directory is not imported.
	outsideID := "52000000-0000-4000-8000-000000000002"
	writeClaudeCatalogFixture(t, filepath.Join(c.project, outsideID+".jsonl"), outsideID, "2026-10-03T02:00:00Z", "outside")

	// Atomic replacement: a new file renamed over the held name.
	before := mustCatalogSource(t, c.store, onlyCatalogRoot(t, c.store).ID, path)
	next := path + ".tmp"
	writeUserRows(t, next, nativeID, "replacement")
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	c.waitTexts(t, nativeID, []string{"replacement"})
	if after := mustCatalogSource(t, c.store, onlyCatalogRoot(t, c.store).ID, path); after.Generation <= before.Generation {
		t.Fatalf("replacement kept source generation %d, want a new one", after.Generation)
	}
	if id := catalogIDFor(t, c.service, outsideID); id != "" {
		t.Fatal("an unattached file was imported without an explicit refresh")
	}

	// Release: no watch remains, and later writes are not ingested.
	c.service.releaseAttachedSource("session")
	if stats := c.service.files.Stats(); stats.Paths != 0 || stats.Files != 0 || stats.Directories != 0 {
		t.Fatalf("watch stats after release = %+v, want none", stats)
	}
	appendCatalogRow(t, path, claudeCatalogUserRow(nativeID, "2026-10-03T02:02:00Z", "after release"))
	time.Sleep(300 * time.Millisecond)
	if got := c.texts(t, nativeID); fmt.Sprint(got) != fmt.Sprint([]string{"replacement"}) {
		t.Fatalf("a released file was still ingested: %q", got)
	}
}

// Failure caught: Grok rewrites summary.json (in place or by replacement),
// before and after updates.jsonl exists, and creates updates.jsonl only with
// the first turn; any of these changes was missed on a watched session.
// Boundary: a held Grok session directory.
func TestAttachedGrokSummaryReplacementThenUpdatesCreation(t *testing.T) {
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := filepath.Join(t.TempDir(), ".grok", "sessions")
	nativeID := "53000000-0000-4000-8000-000000000001"
	session := filepath.Join(root, "%2Fworkspace", nativeID)
	if err := os.MkdirAll(session, 0o700); err != nil {
		t.Fatal(err)
	}
	service, err := newThreadCatalogService(context.Background(), store, threadCatalogServiceOptions{
		Roots: []history.SourceRoot{{Provider: history.ProviderGrok, Kind: history.RootGrokSessions, Path: root}}, Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.setTranscriptChanged(func(context.Context, transcriptChange) {})
	summary := filepath.Join(session, "summary.json")
	writeSummary := func(name string) {
		tmp := summary + ".tmp"
		body := fmt.Sprintf(`{"info":{"id":%q,"cwd":"/workspace"},"generated_title":%q}`, nativeID, name)
		if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, summary); err != nil {
			t.Fatal(err)
		}
	}
	writeSummary("first")
	paths, _ := service.attachedSourcePaths(context.Background(), "grok", nativeID, "/workspace", "")
	if len(paths) != 2 || paths[0] != summary {
		t.Fatalf("grok paths = %q, want summary.json and updates.jsonl under %s", paths, session)
	}
	service.holdAttachedSource("grok-session", paths)
	waitForCatalog(t, func() bool { return catalogIDFor(t, service, nativeID) != "" })
	threadID := catalogIDFor(t, service, nativeID)
	writeSummary("renamed")
	waitForCatalog(t, func() bool {
		thread, err := service.LoadThread(context.Background(), threadID)
		return err == nil && thread.Title != nil && *thread.Title == "renamed"
	})
	updates := `{"timestamp":1893456000,"method":"session/update","params":{"sessionId":"` + nativeID + `","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Hi"}}}}` + "\n" +
		`{"timestamp":1893456001,"method":"session/update","params":{"sessionId":"` + nativeID + `","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Saved answer."}},"_meta":{"promptId":"p1"}}}` + "\n"
	if err := os.WriteFile(paths[1], []byte(updates), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForCatalog(t, func() bool {
		page, err := store.ReadTranscript(context.Background(), history.TranscriptReadRequest{ThreadID: threadID})
		return err == nil && len(page.Items) == 2 && page.Items[1].Text == "Saved answer."
	})

	// With updates.jsonl unchanged, later summary rewrites, by replacement or
	// in place, still update the thread's metadata.
	title := func(want string) {
		t.Helper()
		waitForCatalog(t, func() bool {
			thread, err := service.LoadThread(context.Background(), threadID)
			return err == nil && thread.Title != nil && *thread.Title == want
		})
	}
	writeSummary("replaced after updates")
	title("replaced after updates")
	body := fmt.Sprintf(`{"info":{"id":%q,"cwd":"/workspace"},"generated_title":%q}`, nativeID, "rewritten in place")
	if err := os.WriteFile(summary, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	title("rewritten in place")
	if page, err := store.ReadTranscript(context.Background(), history.TranscriptReadRequest{ThreadID: threadID}); err != nil || len(page.Items) != 2 {
		t.Fatalf("metadata rewrites changed the saved rows: %d, %v", len(page.Items), err)
	}
}

// Failure caught: from 10-02 a session Nano did not start (one opened in a
// terminal) entered the catalog only on Explorer's Refresh, and 14 were
// missing for five days. Boundary: a new file under a provider root, through
// FSEvents, the catalog worker and the saved transcript, with no Refresh and
// no hint. A file nobody wrote to since start stays unknown, because
// discovery reads only what changed, never the root. Outside holds stay at
// most outsideHoldLimit, and the one that gives way is the idlest: an active
// session whose writer keeps its file open is never dropped for being quiet.
func TestOutsideSessionIsDiscoveredWithoutRefresh(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".claude", "projects")
	project := filepath.Join(root, "-workspace")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	untouched := "52000000-0000-4000-8000-000000000001"
	writeUserRows(t, filepath.Join(project, untouched+".jsonl"), untouched, "before start")
	c := newAttachedCatalog(t, nil, root)
	if c.service.discovery == nil {
		t.Fatal("no outside-session discovery")
	}

	nativeID := "52000000-0000-4000-8000-000000000002"
	path := filepath.Join(project, nativeID+".jsonl")
	writeUserRows(t, path, nativeID, "prompt 0")
	c.waitTexts(t, nativeID, []string{"prompt 0"})
	if id := catalogIDFor(t, c.service, untouched); id != "" {
		t.Fatalf("an unchanged outside file was read (%s): discovery walked the root", id)
	}

	// Fill the cap with sessions that have not been written since, then
	// write the first one through a descriptor kept open, as Codex does,
	// which FSEvents does not report: its own watch must still see it.
	var idle [][]string
	for i := range outsideHoldLimit - 1 {
		idle = append(idle, []string{filepath.Join(project, fmt.Sprintf("idle-%03d.jsonl", i))})
	}
	c.service.holdOutside(idle)
	idleSince := time.Now()
	writer, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.WriteString(claudeCatalogUserRow(nativeID, "2026-10-03T01:01:00Z", "prompt 1")); err != nil {
		t.Fatal(err)
	}
	c.waitTexts(t, nativeID, []string{"prompt 0", "prompt 1"})
	// The saved rows are readable before the pass records the hold's
	// activity; on a slower host the cap was reached in that gap and the
	// active session looked idlest.
	waitForCatalog(t, func() bool {
		c.service.attachedMu.Lock()
		defer c.service.attachedMu.Unlock()
		held := c.service.held[path]
		return held != nil && held.progressAt.After(idleSince)
	})

	// One more session past the cap releases the idlest hold, not the
	// active one.
	c.service.holdOutside([][]string{{filepath.Join(project, "newest.jsonl")}})
	stillIdle := 0
	for _, files := range idle {
		if c.service.holdsAttachedSource(files[0]) {
			stillIdle++
		}
	}
	if len(c.service.outside) != outsideHoldLimit || stillIdle != len(idle)-1 || !c.service.holdsAttachedSource(path) {
		t.Fatalf("outside holds = %d, idle held %d of %d, active held %v; want %d with one idle hold released",
			len(c.service.outside), stillIdle, len(idle), c.service.holdsAttachedSource(path), outsideHoldLimit)
	}
}

// Failure caught: a Grok session's summary.json was refused as a source
// once its updates.jsonl existed, so a title Grok writes after the transcript
// never reached the catalog. Boundary: both files appearing in one event
// batch under a Grok root, then a summary-only rewrite.
func TestOutsideGrokTitleAfterTranscript(t *testing.T) {
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	root := filepath.Join(t.TempDir(), ".grok", "sessions")
	nativeID := "54000000-0000-4000-8000-000000000001"
	session := filepath.Join(root, "%2Fworkspace", nativeID)
	if err := os.MkdirAll(session, 0o700); err != nil {
		t.Fatal(err)
	}
	service, err := newThreadCatalogService(context.Background(), store, threadCatalogServiceOptions{
		Roots: []history.SourceRoot{{Provider: history.ProviderGrok, Kind: history.RootGrokSessions, Path: root}}, Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	service.setTranscriptChanged(func(context.Context, transcriptChange) {})
	summary := filepath.Join(session, "summary.json")
	writeSummary := func(name string) {
		body := fmt.Sprintf(`{"info":{"id":%q,"cwd":"/workspace"},"generated_title":%q}`, nativeID, name)
		if err := os.WriteFile(summary, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSummary("first")
	updates := `{"timestamp":1893456000,"method":"session/update","params":{"sessionId":"` + nativeID + `","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Hi"}}}}` + "\n"
	if err := os.WriteFile(filepath.Join(session, "updates.jsonl"), []byte(updates), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForCatalog(t, func() bool { return catalogIDFor(t, service, nativeID) != "" })
	threadID := catalogIDFor(t, service, nativeID)
	writeSummary("generated title")
	waitForCatalog(t, func() bool {
		thread, err := service.LoadThread(context.Background(), threadID)
		return err == nil && thread.Title != nil && *thread.Title == "generated title"
	})
}

// Failure caught: after a restart, hosted files written before their
// attachments re-held them were taken as outside sessions and kept those
// holds (209 of the 256 slots, served 2026-10-27), outliving the sessions.
// Boundary: an attachment holding a file outside discovery holds.
func TestAttachmentTakesOverAnOutsideHold(t *testing.T) {
	c := newAttachedCatalog(t, nil, "")
	path := filepath.Join(c.project, "52000000-0000-4000-8000-000000000003.jsonl")
	c.service.holdOutside([][]string{{path}})
	c.service.holdAttachedSource("session", []string{path})
	if len(c.service.outside) != 0 || !c.service.holdsAttachedSource(path) {
		t.Fatalf("outside holds = %d, held %v; want the attachment's hold alone", len(c.service.outside), c.service.holdsAttachedSource(path))
	}
	c.service.releaseAttachedSource("session")
	if c.service.holdsAttachedSource(path) {
		t.Fatal("a released attachment's file is still watched")
	}
}

package app

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	statedb "github.com/dkroshow/nano/internal/state/db"
)

const stage5BadQueuedAction = "agent-4eac7709f014e5a187cba60260f9e865"

// This fixture is the action that made the schema-21 build refuse every boot.
// Prompt content is deliberately absent; boot must retain and park the row.
func TestThreadRuntimeParksIncompleteQueuedFixtureAndBoots(t *testing.T) {
	directory := t.TempDir()
	initial, err := openThreadRuntimeStores(context.Background(), directory, filepath.Join(directory, "turn-completed.sqlite"), "initial")
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.control.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := statedb.Open(context.Background(), filepath.Join(directory, "nano_threads.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	queued := queuedTurnRecord{
		ClientActionID: stage5BadQueuedAction,
		Provider:       "codex",
		HostedThreadID: "nht_fixture_owner",
		NativeThreadID: "6ac9d71f-ac2f-593b-8875-9358e6a5fe47",
		TextHash:       "5296043bb46993218abb5590607ea1eedf787773e1f1eedede05f5d8ad25c983",
		RequestHash:    "7b610f46e7545df371b68162da3a8c7d55194657021bea6b4d5fd65411736968",
		State:          submissionQueued,
		Origin:         turnOriginAgent,
		CreatedAt:      "2026-09-12T10:59:27.597011Z",
	}
	attempt := sendAttemptRecord{
		ClientActionID: stage5BadQueuedAction,
		Provider:       queued.Provider,
		NativeThreadID: queued.NativeThreadID,
		TextHash:       queued.RequestHash,
		Status:         sendAttemptQueued,
		CreatedAt:      queued.CreatedAt,
		UpdatedAt:      queued.CreatedAt,
		RunID:          "pre-crash",
	}
	if err := turns.SaveQueuedTurns(context.Background(), db, []turns.QueuedTurn{queued}); err != nil {
		t.Fatal(err)
	}
	if err := turns.SaveSendAttempts(context.Background(), db, []turns.SendAttempt{attempt}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var bootLog bytes.Buffer
	priorLog := log.Writer()
	log.SetOutput(&bootLog)
	t.Cleanup(func() { log.SetOutput(priorLog) })
	stores, err := openThreadRuntimeStores(context.Background(), directory, filepath.Join(directory, "turn-completed.sqlite"), "reboot")
	if err != nil {
		t.Fatalf("one incomplete row made Nano unbootable: %v", err)
	}
	for _, want := range []string{"transactional turn boot parked", stage5BadQueuedAction, "missing=text"} {
		if !strings.Contains(bootLog.String(), want) {
			t.Fatalf("boot log %q does not contain %q", bootLog.String(), want)
		}
	}

	got, ok := stores.queue.get(stage5BadQueuedAction)
	if !ok {
		t.Fatal("boot repair removed the offending row")
	}
	if got.State != submissionParked || got.Text != "" {
		t.Fatalf("boot repair row = %+v, want retained parked row", got)
	}
	if _, deliverable := stores.queue.first(queued.Provider, queued.NativeThreadID, queued.HostedThreadID); deliverable {
		t.Fatal("boot-parked row remained eligible for delivery")
	}
	gotAttempt, ok := stores.attempts.get(stage5BadQueuedAction)
	if !ok || gotAttempt.Status != submissionParked || gotAttempt.Reason != "boot_incomplete_outbox:text" {
		t.Fatalf("boot repair journal marker = %+v, present=%v", gotAttempt, ok)
	}

	projection := newLiveProjection(queued.Provider, queued.HostedThreadID, "nano-fixture")
	projection.setNativeThreadID(queued.NativeThreadID)
	sess := &session{
		provider: queued.Provider, hostedThreadID: queued.HostedThreadID,
		threadID: queued.NativeThreadID, projection: projection, queuedTurns: stores.queue,
	}
	sess.restoreQueuedTurnBootDiagnostics()
	diagnostics := projection.threadDiagnosticsSnapshot()
	if len(diagnostics) != 1 || diagnostics[0].Code != "boot_incomplete_outbox" ||
		diagnostics[0].Source != "turn-store-boot" || diagnostics[0].ActionID != stage5BadQueuedAction ||
		!strings.Contains(diagnostics[0].Message, "missing: text") {
		t.Fatalf("thread boot diagnostics = %#v", diagnostics)
	}

	if err := stores.control.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openThreadRuntimeStores(context.Background(), directory, filepath.Join(directory, "turn-completed.sqlite"), "second-reboot")
	if err != nil {
		t.Fatalf("park marker did not survive the next boot: %v", err)
	}
	t.Cleanup(func() { _ = reopened.control.Close() })
	if parks := reopened.queue.bootParksFor(queued.Provider, queued.NativeThreadID, queued.HostedThreadID); len(parks) != 1 || parks[0].ActionID != stage5BadQueuedAction {
		t.Fatalf("reopened boot diagnostics = %#v", parks)
	}
}

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dkroshow/nano/internal/edge/statewiring"
	statedb "github.com/dkroshow/nano/internal/state/db"
	statestore "github.com/dkroshow/nano/internal/state/store"
)

// newTestStore opens an attachment store over a fresh current-schema threads
// database. Reopening the database stands in for a Nano restart.
func newTestStore(t *testing.T) (*AttachmentStore, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nano_threads.sqlite")
	if _, err := statestore.PrepareStore(context.Background(), statestore.StorePaths{Destination: path, BackupRoot: filepath.Join(dir, "backups")}, statewiring.Migrations()); err != nil {
		t.Fatal(err)
	}
	db, err := statedb.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return restart(t, db), db
}

func restart(t *testing.T, db *sql.DB) *AttachmentStore {
	t.Helper()
	store, err := NewAttachmentStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func attach(hosted, native, at string) Attachment {
	return Attachment{Provider: "codex", HostedThreadID: hosted, NativeThreadID: native, AttachedAt: at}
}

type rowState struct{ Native, State, FinishedBy string }

func rowOf(t *testing.T, db *sql.DB, hosted string) rowState {
	t.Helper()
	var r rowState
	if err := db.QueryRow(`SELECT native_thread_id,state,finished_by FROM hosted_attachments WHERE hosted_thread_id=?`, hosted).Scan(&r.Native, &r.State, &r.FinishedBy); err != nil {
		t.Fatalf("row %s: %v", hosted, err)
	}
	return r
}

// The durability rule across a restart: a thread that was live comes back as
// resumable (nothing is live at boot), a thread a caller closed stays
// finished and out of every active read, and its row is kept with who closed
// it. Nothing changes a state because time passed.
func TestAttachmentStatesAcrossRestart(t *testing.T) {
	store, db := newTestStore(t)
	store.Remember(attach("nht_keep", "t-keep", "2026-07-25T10:00:00Z"))
	store.Remember(attach("nht_closed", "t-closed", "2026-07-25T11:00:00Z"))
	store.Remember(attach("nht_live", "t-live", "2026-07-25T12:00:00Z"))
	closed, _ := store.Get("codex", "t-closed")
	if err := store.Finish(closed, "nht_cleanup"); err != nil {
		t.Fatal(err)
	}
	if got := store.DetachedExcept(map[AttachmentKey]bool{{Provider: "codex", NativeThreadID: "t-live"}: true}); len(got) != 1 || got[0].NativeThreadID != "t-keep" {
		t.Fatalf("detached = %+v, want only t-keep", got)
	}

	reopened := restart(t, db)
	got := reopened.DetachedExcept(nil)
	if len(got) != 2 || got[0].NativeThreadID != "t-live" || got[1].NativeThreadID != "t-keep" {
		t.Fatalf("after restart = %+v, want [t-live, t-keep] newest first", got)
	}
	for _, rec := range got {
		if rec.State != AttachmentResumable {
			t.Fatalf("%s came back %q; nothing is live at boot", rec.NativeThreadID, rec.State)
		}
	}
	if r := rowOf(t, db, "nht_closed"); r.State != AttachmentFinished || r.FinishedBy != "nht_cleanup" {
		t.Fatalf("closed row = %+v, want kept as finished by the cleanup caller", r)
	}
	if _, ok := reopened.Get("codex", "t-closed"); ok {
		t.Fatal("a finished thread came back as active after a restart")
	}
}

// One row per hosted thread. A Claude thread's provisional shim handle
// resolving to its real native id moves the row; the handle's staged title
// does not follow it (a later rename must win at boot), and forgetting the
// retired handle afterwards changes nothing.
func TestAttachmentRekeyMovesTheThreadsOneRow(t *testing.T) {
	store, db := newTestStore(t)
	store.Remember(Attachment{Provider: "claude", HostedThreadID: "nht_c", NativeThreadID: "cs_handle", ShimSessionID: "cs_handle", PendingName: "Staged", AttachedAt: "2026-07-25T10:00:00Z"})
	store.Remember(Attachment{Provider: "claude", HostedThreadID: "nht_c", NativeThreadID: "native-c", ShimSessionID: "cs_handle", AttachedAt: "2026-07-25T10:01:00Z"})
	store.Forget("claude", "cs_handle")

	if _, ok := store.Get("claude", "cs_handle"); ok {
		t.Fatal("the retired handle still names an active row")
	}
	rec, ok := store.Get("claude", "native-c")
	if !ok || rec.PendingName != "" {
		t.Fatalf("re-keyed row = %+v, %v; want it under native-c without the staged title", rec, ok)
	}
	if r := rowOf(t, db, "nht_c"); r != (rowState{Native: "native-c", State: AttachmentAttached}) {
		t.Fatalf("stored row = %+v", r)
	}
}

// A Claude row carries the spawn posture (bin + exact args) so a later
// `--resume` fallback can restore it after a restart.
func TestAttachmentResumeDataSurvivesRestart(t *testing.T) {
	store, db := newTestStore(t)
	args := []string{"-p", "--permission-mode", "bypassPermissions", "--model", "opus"}
	store.Remember(Attachment{Provider: "claude", HostedThreadID: "nht_c", NativeThreadID: "c-1", AttachedAt: "2026-07-25T10:00:00Z", Bin: "/usr/local/bin/claude", Args: args, ShimSessionID: "cs_1"})
	got, ok := restart(t, db).Get("claude", "c-1")
	if !ok || got.Bin != "/usr/local/bin/claude" || got.ShimSessionID != "cs_1" || !reflect.DeepEqual(got.Args, args) {
		t.Fatalf("reloaded = %+v, %v", got, ok)
	}
}

// A session that ends without a close leaves its thread resumable and one
// that comes back makes it attached again; neither reopens a finished thread.
// Only an explicit resume (a new durable handshake) does that.
func TestAttachmentLivenessNeverReopensAFinishedThread(t *testing.T) {
	store, db := newTestStore(t)
	store.Remember(attach("nht_a", "t-a", "2026-07-25T10:00:00Z"))
	store.SetLive("codex", "t-a", false)
	if r := rowOf(t, db, "nht_a"); r.State != AttachmentResumable {
		t.Fatalf("after the session ended = %+v, want resumable", r)
	}
	store.SetLive("codex", "t-a", true)
	if r := rowOf(t, db, "nht_a"); r.State != AttachmentAttached {
		t.Fatalf("after the session came back = %+v, want attached", r)
	}
	rec, _ := store.Get("codex", "t-a")
	if err := store.Finish(rec, "operator"); err != nil {
		t.Fatal(err)
	}
	store.SetLive("codex", "t-a", true)
	if r := rowOf(t, db, "nht_a"); r.State != AttachmentFinished {
		t.Fatalf("a liveness report reopened a finished thread: %+v", r)
	}
	store.Remember(attach("nht_a", "t-a", "2026-07-26T10:00:00Z"))
	if r := rowOf(t, db, "nht_a"); r != (rowState{Native: "t-a", State: AttachmentAttached}) {
		t.Fatalf("explicit resume = %+v, want attached again", r)
	}
}

// Finish ends only the process generation the closer saw: a row re-attached
// meanwhile is preserved.
func TestAttachmentFinishRefusesAChangedGeneration(t *testing.T) {
	store, db := newTestStore(t)
	store.Remember(attach("nht_a", "t-a", "2026-07-25T10:00:00Z"))
	seen, _ := store.Get("codex", "t-a")
	store.Remember(attach("nht_a", "t-a", "2026-07-25T11:00:00Z"))
	if err := store.Finish(seen, "operator"); err == nil {
		t.Fatal("finished a generation the closer never saw")
	}
	if r := rowOf(t, db, "nht_a"); r.State != AttachmentAttached {
		t.Fatalf("replacement = %+v, want kept attached", r)
	}
}

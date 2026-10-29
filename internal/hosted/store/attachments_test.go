package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func newTestStore(t *testing.T) *AttachmentStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "attachments.json")
	store, err := NewAttachmentStore(path)
	if err != nil {
		t.Fatalf("newAttachmentStore: %v", err)
	}
	return store
}

func attach(id, cwd, at string) attachmentRecord {
	return attachmentRecord{Provider: "codex", NativeThreadID: id, Cwd: cwd, Model: "gpt-5", AttachedAt: at}
}

// liveCodex builds the composite live set detachedExcept expects from codex
// native ids — the common shape in these tests.
func liveCodex(ids ...string) map[attachmentKey]bool {
	live := map[attachmentKey]bool{}
	for _, id := range ids {
		live[attachmentKey{Provider: "codex", NativeThreadID: id}] = true
	}
	return live
}

// The whole durability feature in one test: a remembered record that is NOT
// forgotten survives (a restart-killed session), while an explicitly forgotten
// one does not (a user-closed session). detachedExcept hides live threads.
func TestAttachmentRememberForgetDetached(t *testing.T) {
	store := newTestStore(t)
	store.Remember(attach("t-keep", "/a", "2026-07-25T10:00:00Z"))
	store.Remember(attach("t-closed", "/b", "2026-07-25T11:00:00Z"))
	store.Remember(attach("t-live", "/c", "2026-07-25T12:00:00Z"))

	// Explicit close forgets one; the other two remain.
	store.Forget("codex", "t-closed")

	// t-live is currently attached, so it is not "detached".
	got := store.DetachedExcept(liveCodex("t-live"))
	if len(got) != 1 || got[0].NativeThreadID != "t-keep" {
		t.Fatalf("detachedExcept = %+v, want only t-keep", got)
	}

	// With nothing live, both survivors are detached, newest first.
	all := store.DetachedExcept(nil)
	if len(all) != 2 || all[0].NativeThreadID != "t-live" || all[1].NativeThreadID != "t-keep" {
		t.Fatalf("detachedExcept(nil) = %+v, want [t-live, t-keep]", all)
	}
}

// records must outlive the process: a fresh store over the same file sees what
// a prior store remembered. This is the "survive a restart" guarantee.
func TestAttachmentPersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attachments.json")
	first, err := NewAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first.Remember(attach("t-1", "/x", "2026-07-25T10:00:00Z"))
	first.Forget("codex", "t-1")
	first.Remember(attach("t-2", "/y", "2026-07-25T11:00:00Z"))

	second, err := NewAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := second.DetachedExcept(nil)
	if len(got) != 1 || got[0].NativeThreadID != "t-2" || got[0].Cwd != "" || got[0].Model != "" {
		t.Fatalf("reloaded store = %+v, want only transport fields for t-2", got)
	}
}

func TestAttachmentMigratesAgentSessionIDToHostedThreadID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attachments.json")
	legacy := []byte(`[{"provider":"codex","nativeThreadId":"native-1","agentSessionId":"legacy-caller","cwd":"/tmp","attachedAt":"2026-08-04T00:00:00Z"}]`)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	record, ok := store.Get("codex", "native-1")
	if !ok || record.HostedThreadID != "legacy-caller" || record.LegacyAgentSessionID != "" {
		t.Fatalf("migrated attachment = %#v, present=%v", record, ok)
	}
	store.Remember(record)
	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rewritten, []byte(`"hostedThreadId": "legacy-caller"`)) || bytes.Contains(rewritten, []byte("agentSessionId")) {
		t.Fatalf("rewritten attachment did not converge: %s", rewritten)
	}
}

// PendingName is session-staged state like ContinuedFrom: the transport that
// rebuilds the record on a later re-attach cannot know it, so an empty incoming
// value must carry the stored one forward rather than erase it. A non-empty
// incoming value (a pre-init rename) still replaces it.
func TestAttachmentCarriesPendingNameForward(t *testing.T) {
	store := newTestStore(t)
	provisional := attachmentRecord{
		Provider: "claude", NativeThreadID: "cs_shim", ShimSessionID: "cs_shim",
		Cwd: "/w", PendingName: "nano api fable", AttachedAt: "2026-08-06T10:00:00Z",
	}
	store.Remember(provisional)

	provisional.PendingName = ""
	store.Remember(provisional)
	if got, ok := store.Get("claude", "cs_shim"); !ok || got.PendingName != "nano api fable" {
		t.Fatalf("pendingName after empty re-remember = %q, %v; want carried forward", got.PendingName, ok)
	}

	provisional.PendingName = "renamed"
	store.Remember(provisional)
	if got, _ := store.Get("claude", "cs_shim"); got.PendingName != "renamed" {
		t.Fatalf("pendingName after rename re-remember = %q, want renamed", got.PendingName)
	}
}

// remember is idempotent by native id: re-attaching the same thread updates the
// record in place rather than duplicating it.
func TestAttachmentRememberIdempotent(t *testing.T) {
	store := newTestStore(t)
	store.Remember(attach("t-1", "/old", "2026-07-25T10:00:00Z"))
	store.Remember(attach("t-1", "/new", "2026-07-25T12:00:00Z"))
	got := store.DetachedExcept(nil)
	if len(got) != 1 || got[0].Cwd != "" || got[0].Model != "" || got[0].TurnServiceTier != "" {
		t.Fatalf("got %+v, want single transport-only record", got)
	}
}

func TestAttachmentRememberScrubsRetiredMetadataWithoutLosingTransport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attachments.json")
	legacy := `[{"provider":"claude","nativeThreadId":"native-1","hostedThreadId":"nht_legacy","cwd":"/old","model":"opus","turnServiceTier":"priority","bin":"/claude","args":["-p","--resume","native-1"],"shimSessionId":"shim-1","attachedAt":"2026-08-01T00:00:00Z"}]`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Remember(attachmentRecord{Provider: "claude", NativeThreadID: "native-1", HostedThreadID: "nht_legacy", Bin: "/claude", Args: []string{"-p", "--resume", "native-1"}, ShimSessionID: "shim-1", AttachedAt: "2026-08-02T00:00:00Z"})
	reopened, err := NewAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get("claude", "native-1")
	if !ok || got.Cwd != "" || got.Model != "" || got.TurnServiceTier != "" {
		t.Fatalf("retired metadata survived transport refresh: %+v", got)
	}
	if got.Bin != "/claude" || !reflect.DeepEqual(got.Args, []string{"-p", "--resume", "native-1"}) || got.ShimSessionID != "shim-1" {
		t.Fatalf("transport fields lost during scrub: %+v", got)
	}
}

// A blank native id is never recorded — a session that never reached a thread
// id must not leave an unusable ghost.
func TestAttachmentIgnoresBlankID(t *testing.T) {
	store := newTestStore(t)
	store.Remember(attach("", "/x", "2026-07-25T10:00:00Z"))
	if got := store.DetachedExcept(nil); len(got) != 0 {
		t.Fatalf("blank id recorded: %+v", got)
	}
}

// forget of an unknown id is a no-op and must not create or corrupt the file.
func TestAttachmentForgetUnknownNoop(t *testing.T) {
	store := newTestStore(t)
	store.Forget("codex", "never-seen")
	if _, err := os.Stat(store.path); !os.IsNotExist(err) {
		t.Fatalf("forget of unknown id touched disk: stat err = %v", err)
	}
}

// A garbage file disables the store loudly rather than silently swallowing it,
// so a corrupt attachments file can't masquerade as "no attachments".
func TestAttachmentLoadRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attachments.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAttachmentStore(path); err == nil {
		t.Fatal("expected error loading garbage attachments file")
	}
}

// The on-disk shape is a flat JSON array of records — the contract the boot
// loader and any external inspector rely on.
func TestAttachmentOnDiskShape(t *testing.T) {
	store := newTestStore(t)
	store.Remember(attach("t-1", "/x", "2026-07-25T10:00:00Z"))
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	var Records []attachmentRecord
	if err := json.Unmarshal(data, &Records); err != nil {
		t.Fatalf("on-disk file is not a record array: %v", err)
	}
	if len(Records) != 1 || Records[0].NativeThreadID != "t-1" {
		t.Fatalf("on-disk records = %+v", Records)
	}
}

// A file written before attachments were provider-aware still loads: its
// records are codex by construction, and the next rewrite carries the provider
// field explicitly, so the legacy shape converts on first touch rather than
// lingering as an ambiguous record.
func TestAttachmentLegacyFileLoadsAsCodex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attachments.json")
	legacy := `[{"nativeThreadId":"t-old","cwd":"/a","model":"gpt-5","attachedAt":"2026-07-25T10:00:00Z"}]`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := store.DetachedExcept(nil)
	if len(got) != 1 || got[0].Provider != "codex" {
		t.Fatalf("legacy record = %+v, want provider codex", got)
	}
	// The legacy record is addressable as codex, so a live codex session hides
	// it — the whole point of normalizing on read.
	if live := store.DetachedExcept(liveCodex("t-old")); len(live) != 0 {
		t.Fatalf("legacy record not keyed as codex: %+v", live)
	}

	// Any write rewrites the whole file; the legacy record gains the field.
	store.Remember(attach("t-new", "/b", "2026-07-25T11:00:00Z"))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 {
		t.Fatalf("on-disk records = %+v", raw)
	}
	for _, rec := range raw {
		if rec["provider"] != "codex" {
			t.Fatalf("record missing written provider: %+v", rec)
		}
	}
}

// A claude record carries the spawn posture (bin + exact args) so a later
// `--resume` fallback can restore it; those fields must survive a reload
// verbatim, and codex records must not grow empty ones on disk.
func TestAttachmentClaudeRecordRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attachments.json")
	first, err := NewAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-p", "--permission-mode", "bypassPermissions", "--model", "opus"}
	first.Remember(attachmentRecord{
		Provider:       "claude",
		NativeThreadID: "c-1",
		Cwd:            "/w",
		Model:          "opus",
		AttachedAt:     "2026-07-25T10:00:00Z",
		Bin:            "/usr/local/bin/claude",
		Args:           args,
	})
	first.Remember(attach("t-1", "/x", "2026-07-25T09:00:00Z"))

	second, err := NewAttachmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := second.DetachedExcept(nil)
	if len(got) != 2 {
		t.Fatalf("reloaded = %+v, want 2 records", got)
	}
	claude := got[0] // newest first
	if claude.Provider != "claude" || claude.Bin != "/usr/local/bin/claude" {
		t.Fatalf("claude record = %+v", claude)
	}
	if !reflect.DeepEqual(claude.Args, args) {
		t.Fatalf("args = %v, want %v", claude.Args, args)
	}

	// bin/args are claude-only: a codex record must not carry the keys at all.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	// Sorted by provider then id: claude first.
	if raw[0]["provider"] != "claude" || raw[1]["provider"] != "codex" {
		t.Fatalf("on-disk order = %+v, want claude then codex", raw)
	}
	if _, ok := raw[1]["bin"]; ok {
		t.Fatalf("codex record carries bin: %+v", raw[1])
	}
	if _, ok := raw[1]["args"]; ok {
		t.Fatalf("codex record carries args: %+v", raw[1])
	}
}

// records are identified by (provider, native id), not by the id alone: the
// same id under two providers is two independent records, each forgotten and
// hidden-when-live on its own.
func TestAttachmentProviderScopedIdentity(t *testing.T) {
	store := newTestStore(t)
	store.Remember(attach("shared", "/a", "2026-07-25T10:00:00Z"))
	store.Remember(attachmentRecord{
		Provider:       "claude",
		NativeThreadID: "shared",
		Cwd:            "/b",
		AttachedAt:     "2026-07-25T11:00:00Z",
	})
	if got := store.DetachedExcept(nil); len(got) != 2 {
		t.Fatalf("detached = %+v, want one record per provider", got)
	}

	// A live claude session hides only the claude record.
	got := store.DetachedExcept(map[attachmentKey]bool{
		{Provider: "claude", NativeThreadID: "shared"}: true,
	})
	if len(got) != 1 || got[0].Provider != "codex" {
		t.Fatalf("detached = %+v, want only the codex record", got)
	}

	// Closing the codex session leaves the claude record standing.
	store.Forget("codex", "shared")
	got = store.DetachedExcept(nil)
	if len(got) != 1 || got[0].Provider != "claude" {
		t.Fatalf("after codex forget = %+v, want only the claude record", got)
	}
}

// forgetEveryProvider is the dismiss path's hammer: the route knows only the
// native id, so it clears the id wherever it is remembered.
func TestAttachmentForgetEveryProvider(t *testing.T) {
	store := newTestStore(t)
	store.Remember(attach("shared", "/a", "2026-07-25T10:00:00Z"))
	store.Remember(attachmentRecord{
		Provider:       "claude",
		NativeThreadID: "shared",
		AttachedAt:     "2026-07-25T11:00:00Z",
	})
	store.Remember(attach("other", "/c", "2026-07-25T12:00:00Z"))

	store.forgetEveryProvider("shared")
	got := store.DetachedExcept(nil)
	if len(got) != 1 || got[0].NativeThreadID != "other" {
		t.Fatalf("detached = %+v, want only other", got)
	}
}

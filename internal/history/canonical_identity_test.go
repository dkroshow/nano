package history

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This is deliberately a parser-level regression: the prior implementation
// included ProjectionKey in nit_ derivation, so the first assertion fails when
// run against that behavior.
func TestCanonicalTranscriptIdentitySurvivesProjectionRebuild(t *testing.T) {
	input := `{"type":"event_msg","payload":{"type":"user_message","message":"same durable source"}}` + "\n"
	adapter := newTranscriptFixture(t, ProviderCodex)
	first, err := adapter.ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}}, NativeThreadID: "native-canonical", Reader: strings.NewReader(input), ProjectionKey: "ntp_old",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := adapter.ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}}, NativeThreadID: "native-canonical", Reader: strings.NewReader(input), ProjectionKey: "ntp_new",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || len(second.Items) != 1 {
		t.Fatalf("items = %#v %#v", first.Items, second.Items)
	}
	if first.Items[0].ID != second.Items[0].ID {
		t.Fatalf("rebuild changed canonical id: %q != %q", first.Items[0].ID, second.Items[0].ID)
	}
	if first.Items[0].SourceKey != "line:1:0" {
		t.Fatalf("source key lost emission suffix: %q", first.Items[0].SourceKey)
	}
	third, err := adapter.ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}}, NativeThreadID: "other-native", Reader: strings.NewReader(input), ProjectionKey: "ntp_new",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Items[0].ID == third.Items[0].ID {
		t.Fatal("native thread identity did not participate in canonical id")
	}
}

// Claude emits a different durable transcript shape from Codex, but its
// normalized identity has the same provider/native/source-lineage contract.
// This fails if the Claude adapter falls back to its projection key.
func TestCanonicalClaudeTranscriptIdentitySurvivesProjectionRebuild(t *testing.T) {
	input := `{"type":"user","timestamp":"2026-08-30T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"same durable source"}]}}` + "\n"
	adapter := newTranscriptFixture(t, ProviderClaude)
	first, err := adapter.ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: SourceCandidate{Root: SourceRoot{Provider: ProviderClaude}}, NativeThreadID: "claude-native-canonical", Reader: strings.NewReader(input), ProjectionKey: "ntp_old",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := adapter.ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: SourceCandidate{Root: SourceRoot{Provider: ProviderClaude}}, NativeThreadID: "claude-native-canonical", Reader: strings.NewReader(input), ProjectionKey: "ntp_new",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || len(second.Items) != 1 {
		t.Fatalf("items = %#v %#v", first.Items, second.Items)
	}
	if first.Items[0].ID != second.Items[0].ID || first.Items[0].ID != canonicalTranscriptItemID(ProviderClaude, "claude-native-canonical", first.Items[0].SourceKey) {
		t.Fatalf("Claude identity = %q / %q, source=%q", first.Items[0].ID, second.Items[0].ID, first.Items[0].SourceKey)
	}
}

// The provider namespace and suffix-complete source key are both part of the
// canonical formula. This is particularly important for Grok, whose updates
// can emit several normalized rows from one source line.
func TestCanonicalTranscriptIdentityScopesProviderNativeAndEmission(t *testing.T) {
	codex := canonicalTranscriptItemID(ProviderCodex, "native-1", "line:7:0")
	claude := canonicalTranscriptItemID(ProviderClaude, "native-1", "line:7:0")
	grok := canonicalTranscriptItemID(ProviderGrok, "native-1", "line:7:0")
	emission := canonicalTranscriptItemID(ProviderGrok, "native-1", "line:7:1")
	otherNative := canonicalTranscriptItemID(ProviderGrok, "native-2", "line:7:0")
	seen := map[string]bool{}
	for label, id := range map[string]string{"codex": codex, "claude": claude, "grok": grok, "emission": emission, "native": otherNative} {
		if seen[id] {
			t.Fatalf("canonical identity collision at %s: %q", label, id)
		}
		seen[id] = true
	}
}

// Before schema v15, this red-proves the canonical-id migration defect: the
// second rebuild writes the same ncb_ content reference into a retained shadow
// projection and SQLite rejects the global unique constraint.
func TestCanonicalRebuildRetainsLargeBodyEvidenceAcrossProjections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	metadata := testSource(root, "canonical-large.jsonl", "native-canonical", ThreadMain, nil)
	thread, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	rootID := mustRootID(t, store, root)
	stored, ok, err := store.LoadSource(ctx, rootID, metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("source = %#v, %v, %v", stored, ok, err)
	}
	adapter := newTranscriptFixture(t, ProviderCodex)
	project := func(legacy bool) TranscriptProjectionResult {
		result, projectErr := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{
			Source: metadata.Source, NativeThreadID: "native-canonical", Reader: bytes.NewReader([]byte(codexLargeTurnJSONL())),
		})
		if projectErr != nil {
			t.Fatal(projectErr)
		}
		if legacy {
			legacyIndex := -1
			for index := range result.Items {
				if result.Items[index].ContentRef != "" {
					legacyIndex = index
					break
				}
			}
			if legacyIndex < 0 {
				t.Fatal("fixture did not emit a retained body")
			}
			canonicalID := result.Items[legacyIndex].ID
			result.Items[legacyIndex].ID = "legacy-projection-item"
			for index := range result.Bodies {
				if result.Bodies[index].ItemID == canonicalID {
					result.Bodies[index].ItemID = "legacy-projection-item"
				}
			}
		}
		return result
	}
	first, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: thread.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: ProviderCodex, ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(), Result: project(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: thread.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: ProviderCodex, ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(),
		ExpectedProjectionKey: first.ProjectionKey, Result: project(false),
	})
	if err != nil {
		t.Fatalf("canonical rebuild with retained body evidence: %v", err)
	}
	if second.ProjectionKey == first.ProjectionKey {
		t.Fatal("rebuild retained its prior projection key")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatalf("reopen canonical aliases: %v", err)
	}
	var bodies int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_item_bodies`).Scan(&bodies); err != nil {
		t.Fatal(err)
	}
	if bodies != 2 {
		t.Fatalf("retained body count = %d, want 2", bodies)
	}
	page, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: thread.ThreadID, Limit: 10})
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("canonical active page = %#v, %v", page, err)
	}
	var activeItem TranscriptItem
	for _, item := range page.Items {
		if item.ContentRef != "" {
			activeItem = item
			break
		}
	}
	if activeItem.ID == "" || activeItem.ID == "legacy-projection-item" {
		t.Fatalf("active page retained historical id: %#v", page.Items)
	}
	if page.Projection.AliasCount != 1 {
		t.Fatalf("alias count = %d, want 1", page.Projection.AliasCount)
	}
	body, err := store.LoadTranscriptBody(ctx, thread.ThreadID, "legacy-projection-item")
	if err != nil || body.ItemID != activeItem.ID {
		t.Fatalf("legacy body alias = %#v, %v", body, err)
	}
	evidence, err := store.LoadTranscriptEvidenceRef(ctx, thread.ThreadID, "legacy-projection-item")
	if err != nil || evidence.ItemID != activeItem.ID {
		t.Fatalf("legacy evidence alias = %#v, %v", evidence, err)
	}
}

// A bad legacy alias must not suppress the active transcript. The former
// behavior returned ErrTranscriptConflict, which made the rebuild degraded and
// hid otherwise valid rows. We retain the existing mapping, record the
// disagreement, and continue with the new active projection.
func TestCanonicalAliasConflictIsDiagnosedWithoutBlockingRebuild(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	metadata := testSource(root, "alias-conflict.jsonl", "native-alias-conflict", ThreadMain, nil)
	thread, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	rootID := mustRootID(t, store, root)
	stored, ok, err := store.LoadSource(ctx, rootID, metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("source = %#v, %v, %v", stored, ok, err)
	}
	adapter := newTranscriptFixture(t, ProviderCodex)
	project := func() TranscriptProjectionResult {
		result, projectErr := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{
			Source: metadata.Source, NativeThreadID: "native-alias-conflict",
			Reader: strings.NewReader(`{"type":"event_msg","payload":{"type":"user_message","message":"prompt"}}` + "\n"),
		})
		if projectErr != nil {
			t.Fatal(projectErr)
		}
		return result
	}
	firstResult := project()
	firstResult.Items[0].ID = "legacy-projection-item"
	first, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: thread.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: ProviderCodex, ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(), Result: firstResult,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO conversation_item_aliases(thread_id, old_item_id, canonical_item_id, source_key, created_at)
VALUES (?, ?, ?, ?, ?)`, thread.ThreadID, "legacy-projection-item", "other-canonical-item", "legacy-source", formatTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	second, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: thread.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: ProviderCodex, ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(),
		ExpectedProjectionKey: first.ProjectionKey, Result: project(),
	})
	if err != nil {
		t.Fatalf("alias collision blocked active rebuild: %v", err)
	}
	page, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: thread.ThreadID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Projection.Key != second.ProjectionKey || len(page.Items) != 1 {
		t.Fatalf("active projection = %#v", page)
	}
	if page.Projection.AliasCount != 1 || page.Projection.AliasConflictCount != 1 {
		t.Fatalf("alias counts = %d aliases, %d conflicts; want 1 each", page.Projection.AliasCount, page.Projection.AliasConflictCount)
	}
	var existing, candidate string
	if err := store.db.QueryRowContext(ctx, `
SELECT existing_canonical_item_id, candidate_canonical_item_id
FROM conversation_item_alias_conflicts WHERE thread_id = ?`, thread.ThreadID).Scan(&existing, &candidate); err != nil {
		t.Fatal(err)
	}
	if existing != "other-canonical-item" || candidate != page.Items[0].ID {
		t.Fatalf("conflict = existing %q candidate %q", existing, candidate)
	}
}

// This exercises the shipped v15 migration against a v14-shaped database.
// The attempted rebuild is the red proof: a canonical ncb_ reference cannot
// be inserted while the old global unique index remains. Reopening applies v15
// and the identical rebuild succeeds without losing the retained body.
func TestCanonicalBodyRefMigrationV15RepairsExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	metadata := testSource(root, "migration-v15.jsonl", "native-migration-v15", ThreadMain, nil)
	thread, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("source = %#v, %v, %v", stored, ok, err)
	}
	adapter := newTranscriptFixture(t, ProviderCodex)
	project := func() TranscriptProjectionResult {
		result, projectErr := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{
			Source: metadata.Source, NativeThreadID: "native-migration-v15", Reader: bytes.NewReader([]byte(codexLargeTurnJSONL())),
		})
		if projectErr != nil {
			t.Fatal(projectErr)
		}
		return result
	}
	first, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: thread.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: ProviderCodex, ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(), Result: project(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
DELETE FROM schema_migrations WHERE version = 15;
ALTER TABLE conversation_item_bodies RENAME TO conversation_item_bodies_v15_source;
CREATE TABLE conversation_item_bodies (
  projection_id INTEGER NOT NULL,
  item_id TEXT NOT NULL,
  content_ref TEXT NOT NULL,
  body_text TEXT,
  body_json BLOB,
  original_bytes INTEGER NOT NULL,
  stored_bytes INTEGER NOT NULL,
  truncated INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(projection_id, item_id),
  UNIQUE(content_ref),
  FOREIGN KEY(projection_id, item_id)
    REFERENCES conversation_items(projection_id, item_id) ON DELETE CASCADE
);
INSERT INTO conversation_item_bodies
SELECT * FROM conversation_item_bodies_v15_source;
DROP TABLE conversation_item_bodies_v15_source;`); err != nil {
		t.Fatal(err)
	}
	_, err = store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: thread.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: ProviderCodex, ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(),
		ExpectedProjectionKey: first.ProjectionKey, Result: project(),
	})
	if err == nil {
		t.Fatal("v14 global content_ref uniqueness accepted a canonical rebuild")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatalf("apply v15 migration: %v", err)
	}
	defer store.Close()
	second, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: thread.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: ProviderCodex, ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(),
		ExpectedProjectionKey: first.ProjectionKey, Result: project(),
	})
	if err != nil {
		t.Fatalf("v15 canonical rebuild: %v", err)
	}
	if second.ProjectionKey == first.ProjectionKey {
		t.Fatal("migration retry reused old projection")
	}
	var migrationCount, bodies int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 15`).Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_item_bodies`).Scan(&bodies); err != nil {
		t.Fatal(err)
	}
	if migrationCount != 1 || bodies != 2 {
		t.Fatalf("v15 migration count=%d bodies=%d, want 1 and 2", migrationCount, bodies)
	}
}

package history

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Absorbed-row aliases: when a rebuild joins an old row's source
// line into another item, as reader v36 does for Codex serialization twins,
// the old row's public id must still resolve to the surviving row.

const absorbedTurn = "753cd896-0780-53e6-9da0-91e68ad3d591"

// absorbedRollout is one real Codex turn. With equal answers on the event
// copy (line 4) and the response copy (line 5) the reader joins them; unequal
// answers keep two rows, which stands in for the old parser's projection.
func absorbedRollout(eventText, responseText string) string {
	return `{"type":"session_meta","payload":{"id":"native-absorbed","cwd":"/work/app","timestamp":"2026-09-15T20:13:00Z"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"` + absorbedTurn + `"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"user_message","message":"is the build finished?"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"` + absorbedTurn + `","item":{"type":"AgentMessage","id":"item-16","content":[{"type":"Text","text":"` + eventText + `"}],"phase":"final_answer"}}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","id":"msg_06704d457e13504cb653b37f41964dde","role":"assistant","content":[{"type":"output_text","text":"` + responseText + `"}],"phase":"final_answer"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"` + absorbedTurn + `"}}` + "\n"
}

type absorbedFixture struct {
	t        *testing.T
	store    *Store
	stored   StoredSource
	threadID string
}

func newAbsorbedFixture(t *testing.T) *absorbedFixture {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	updated := time.Date(2026, 9, 15, 20, 13, 0, 0, time.UTC)
	applied, err := store.ApplySource(context.Background(), testSource(root, "rollout-absorbed.jsonl", "native-absorbed", ThreadMain, &updated))
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(context.Background(), mustRootID(t, store, root), filepath.Join(root.Path, "rollout-absorbed.jsonl"))
	if err != nil || !ok {
		t.Fatalf("stored source = %v, %v", ok, err)
	}
	return &absorbedFixture{t: t, store: store, stored: stored, threadID: applied.ThreadID}
}

// rebuild parses raw with the production reader and publishes it as a full
// rebuild of the thread, as a parser-version backfill does.
func (f *absorbedFixture) rebuild(raw string) {
	f.t.Helper()
	ctx := context.Background()
	source := f.stored.Source
	source.Size = int64(len(raw))
	parsed, err := ProjectParsedFile(ctx, ParsedFileRequest{Source: source, Reader: strings.NewReader(raw), SpoolDir: f.t.TempDir()})
	if err != nil {
		f.t.Fatal(err)
	}
	defer parsed.Close()
	expected := ""
	if header, exists, err := f.store.LoadTranscriptCheckpointHeader(ctx, f.threadID); err == nil && exists {
		expected = header.ProjectionKey
	}
	if _, err := f.store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: f.threadID, SourceID: f.stored.ID, SourceGeneration: f.stored.Generation,
		Provider: ProviderCodex, ParserVersion: parsed.ParserVersion, CompletionRuleVersion: parsed.CompletionRuleVersion,
		ExpectedProjectionKey: expected, IndexedAt: time.Now().UTC(), Result: parsed.Transcript,
	}); err != nil {
		f.t.Fatal(err)
	}
}

// answers returns the active projection's agent-message ids by source line.
func (f *absorbedFixture) answers() map[int]string {
	f.t.Helper()
	rows, err := f.store.db.Query(`SELECT i.source_line, i.item_id FROM conversation_items i
JOIN conversation_projections p ON p.id = i.projection_id
WHERE p.thread_id = ? AND p.state = 'active' AND i.item_type = 'agent_message'`, f.threadID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	answers := map[int]string{}
	for rows.Next() {
		var line int
		var id string
		if err := rows.Scan(&line, &id); err != nil {
			f.t.Fatal(err)
		}
		answers[line] = id
	}
	return answers
}

func (f *absorbedFixture) aliasCount() int {
	f.t.Helper()
	var count int
	if err := f.store.db.QueryRow(`SELECT COUNT(*) FROM conversation_item_aliases WHERE thread_id = ?`, f.threadID).Scan(&count); err != nil {
		f.t.Fatal(err)
	}
	return count
}

func (f *absorbedFixture) projectionIDs() (older, active int64) {
	f.t.Helper()
	rows, err := f.store.db.Query(`SELECT id, state FROM conversation_projections WHERE thread_id = ? ORDER BY id`, f.threadID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			f.t.Fatal(err)
		}
		if state == "active" {
			active = id
		} else {
			older = id
		}
	}
	return older, active
}

const (
	joinedAnswer      = "Yes—it finished."
	oldEventAnswer    = "Yes—it finished (event copy)."
	oldResponseAnswer = "Yes—it finished (response copy)."
)

// TestRebuildAliasesAbsorbedTwinToSurvivingRow catches two failures of the
// retired copy's public id after a rebuild joins it into another row. It must
// still resolve when its source line is unchanged, or a pin or link on it is
// stranded; it must not resolve to the joined row when its line was rewritten,
// or the link silently changes meaning. Boundary: a full rebuild through
// ApplyTranscript and the store's ResolveTranscriptItemID. The surviving row
// keeps its own id, so it gets no alias.
func TestRebuildAliasesAbsorbedTwinToSurvivingRow(t *testing.T) {
	for _, tc := range []struct {
		name         string
		old          string
		wantResolved bool
	}{
		{"unchanged line 5", absorbedRollout(oldEventAnswer, joinedAnswer), true},
		{"rewritten line 5", absorbedRollout(joinedAnswer, oldResponseAnswer), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAbsorbedFixture(t)
			f.rebuild(tc.old)
			before := f.answers()
			survivor, retired := before[4], before[5]
			if len(before) != 2 || survivor == "" || retired == "" {
				t.Fatalf("old projection answers = %v, want two rows on lines 4 and 5", before)
			}
			f.rebuild(absorbedRollout(joinedAnswer, joinedAnswer))
			after := f.answers()
			if len(after) != 1 || after[4] != survivor {
				t.Fatalf("rebuilt answers = %v, want the one joined row %s", after, survivor)
			}
			resolved, _ := f.store.ResolveTranscriptItemID(context.Background(), f.threadID, retired)
			if got := resolved == survivor; got != tc.wantResolved {
				t.Fatalf("retired %s resolves to %q; resolves to the joined row = %t, want %t (aliases %d)", retired, resolved, got, tc.wantResolved, f.aliasCount())
			}
			if resolved, err := f.store.ResolveTranscriptItemID(context.Background(), f.threadID, survivor); err != nil || resolved != survivor {
				t.Fatalf("resolve survivor = %q, %v; want %s", resolved, err, survivor)
			}
		})
	}
}

// TestAbsorbedAliasSkipsAmbiguousOwners catches an alias guessed between two
// candidate rows. Boundary: the absorbed-alias query on the two real
// projections, after one more new item is linked to the retired copy's
// source line. Zero or several owners must give no alias.
func TestAbsorbedAliasSkipsAmbiguousOwners(t *testing.T) {
	f := newAbsorbedFixture(t)
	f.rebuild(absorbedRollout(oldEventAnswer, joinedAnswer))
	f.rebuild(absorbedRollout(joinedAnswer, joinedAnswer))
	older, active := f.projectionIDs()
	if older == 0 || active == 0 {
		t.Fatalf("projections old=%d active=%d", older, active)
	}
	tx, err := f.store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	aliases, err := absorbedTranscriptItemAliases(context.Background(), tx, older, active)
	if err != nil || len(aliases) != 1 {
		t.Fatalf("unambiguous aliases = %+v, %v; want one", aliases, err)
	}
	// Link the prompt row to the retired copy's line 5 observation too.
	if _, err := tx.Exec(`INSERT INTO conversation_item_observations(projection_id, item_id, observation_id)
SELECT ?, prompt.item_id, obs.id FROM conversation_items prompt, conversation_observations obs
WHERE prompt.projection_id = ? AND prompt.item_type = 'user_message' AND obs.projection_id = ? AND obs.source_line = 5`, active, active, active); err != nil {
		t.Fatal(err)
	}
	if aliases, err := absorbedTranscriptItemAliases(context.Background(), tx, older, active); err != nil || len(aliases) != 0 {
		t.Fatalf("ambiguous aliases = %+v, %v; want none", aliases, err)
	}
	// Zero owners: no new item holds the line at all.
	if _, err := tx.Exec(`DELETE FROM conversation_item_observations WHERE projection_id = ? AND observation_id IN (
SELECT id FROM conversation_observations WHERE projection_id = ? AND source_line = 5)`, active, active); err != nil {
		t.Fatal(err)
	}
	if aliases, err := absorbedTranscriptItemAliases(context.Background(), tx, older, active); err != nil || len(aliases) != 0 {
		t.Fatalf("ownerless aliases = %+v, %v; want none", aliases, err)
	}
}

// TestAbsorbedAliasNeverRewritesAnExistingAlias catches the new pass
// replacing an alias that already names another row. Boundary: the alias
// table and its conflict record after a full rebuild.
func TestAbsorbedAliasNeverRewritesAnExistingAlias(t *testing.T) {
	f := newAbsorbedFixture(t)
	f.rebuild(absorbedRollout(oldEventAnswer, joinedAnswer))
	retired := f.answers()[5]
	if _, err := f.store.db.Exec(`INSERT INTO conversation_item_aliases(thread_id, old_item_id, canonical_item_id, source_key, created_at)
VALUES (?, ?, 'nit_existing_target', 'existing-source', ?)`, f.threadID, retired, formatTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	f.rebuild(absorbedRollout(joinedAnswer, joinedAnswer))
	var canonical string
	if err := f.store.db.QueryRow(`SELECT canonical_item_id FROM conversation_item_aliases WHERE thread_id = ? AND old_item_id = ?`, f.threadID, retired).Scan(&canonical); err != nil || canonical != "nit_existing_target" {
		t.Fatalf("existing alias = %q, %v; want it unchanged", canonical, err)
	}
	var conflicts int
	if err := f.store.db.QueryRow(`SELECT COUNT(*) FROM conversation_item_alias_conflicts WHERE thread_id = ? AND old_item_id = ?`, f.threadID, retired).Scan(&conflicts); err != nil || conflicts != 1 {
		t.Fatalf("conflicts = %d, %v; want 1", conflicts, err)
	}
}

// TestRestoredRowResolvesToItself catches a row restored by a later parser
// still following the alias an earlier rebuild recorded when it absorbed that
// row: its links and evidence pointed at the earlier row. Boundary: three full
// rebuilds through ApplyTranscript (separate, absorbed, restored), then
// ResolveTranscriptItemID and LoadTranscriptEvidenceRef.
func TestRestoredRowResolvesToItself(t *testing.T) {
	f := newAbsorbedFixture(t)
	f.rebuild(absorbedRollout(oldEventAnswer, joinedAnswer))
	restored := f.answers()[5]
	f.rebuild(absorbedRollout(joinedAnswer, joinedAnswer))
	if answers := f.answers(); len(answers) != 1 {
		t.Fatalf("absorbing rebuild answers = %v, want one joined row", answers)
	}
	f.rebuild(absorbedRollout(joinedAnswer, oldResponseAnswer))
	if f.answers()[5] != restored {
		t.Fatalf("restoring rebuild answers = %v, want %s back on line 5", f.answers(), restored)
	}
	resolved, err := f.store.ResolveTranscriptItemID(context.Background(), f.threadID, restored)
	if err != nil || resolved != restored {
		t.Fatalf("resolve restored %s = %q, %v; want itself", restored, resolved, err)
	}
	evidence, err := f.store.LoadTranscriptEvidenceRef(context.Background(), f.threadID, restored)
	if err != nil || evidence.SourceLine != 5 {
		t.Fatalf("restored evidence = %+v, %v; want source line 5", evidence, err)
	}
}

package history

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindFileEvidenceBlobsUsesOnlyActiveProjectionAndAllRetainedFields(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`INSERT INTO source_roots(id, provider, root_kind, canonical_root) VALUES (1,'codex','sessions','/tmp/roots')`)
	exec(`INSERT INTO source_files(id, source_root_id, canonical_path, classification, status)
	      VALUES (1,1,'/tmp/roots/a.jsonl','session','present')`)
	exec(`INSERT INTO threads(id, source_root_id, provider, native_thread_id, kind, metadata_json, source_status)
	      VALUES ('nth_1',1,'codex','native-1','main','{}','present')`)

	addProjection := func(id int, key, state string) {
		exec(`INSERT INTO conversation_projections(
		        id, projection_key, thread_id, source_file_id, source_generation, provider,
		        parser_version, completion_rule_version, schema_version, taxonomy_version,
		        state, latest_seq, last_indexed_at)
		      VALUES (?,?,'nth_1',1,1,'codex','v5','v1',1,'v2',?,10,'2026-08-05T00:00:00Z')`, id, key, state)
		exec(`INSERT INTO conversation_turns(
		        projection_id, turn_key, ordinal, start_seq, end_seq, completion_seq,
		        status, completion_evidence)
		      VALUES (?,'turn-1',1,1,1,1,'completed','task_complete')`, id)
	}
	addItem := func(projectionID int, itemID, display, detail, body string) {
		exec(`INSERT INTO conversation_items(
		        projection_id, item_id, projected_seq, source_key, turn_key, turn_ordinal,
		        item_type, item_status, display_text, detail_json, source_line, source_row_type)
		      VALUES (?,?,1,?,'turn-1',1,'agent_message','completed',?,?,1,'response_item')`,
			projectionID, itemID, "source-"+itemID, display, []byte(detail))
		if body != "" {
			exec(`INSERT INTO conversation_item_bodies(
			        projection_id, item_id, content_ref, body_text, original_bytes, stored_bytes)
			      VALUES (?,?,?,?,?,?)`, projectionID, itemID, "body-"+itemID, body, len(body), len(body))
		}
	}

	addProjection(1, "ntp_active", "active")
	addItem(1, "active", "display /tmp/a/report.md", `{"path":"/tmp/b/report.md"}`, "body /tmp/c/report.md")
	addProjection(2, "ntp_dead", "superseded")
	addItem(2, "dead", "superseded /tmp/dead/report.md", `{}`, "")

	blobs, err := store.FindFileEvidenceBlobs(context.Background(), "nth_1", "report.md", 10, 4096)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(blobs, "\n")
	for _, want := range []string{"/tmp/a/report.md", "/tmp/b/report.md", "/tmp/c/report.md"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("evidence %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "/tmp/dead/report.md") {
		t.Fatalf("superseded projection leaked into evidence: %q", joined)
	}
	bounded, err := store.FindFileEvidenceBlobs(context.Background(), "nth_1", "report.md", 1, 8)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, blob := range bounded {
		total += len(blob)
	}
	if total == 0 || total > 8 {
		t.Fatalf("bounded evidence has %d bytes, want 1..8", total)
	}
}

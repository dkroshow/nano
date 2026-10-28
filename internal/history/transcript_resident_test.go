package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	parsecodex "github.com/dkroshow/nano/internal/transcriptparse/codex"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// codexWorkerRows is a Codex rollout in the shapes a long worker turn writes,
// with invented ids and text: a turn that completes, then a turn that keeps
// running, each step a reasoning record and its completion, a tool call and
// its output, and a token count.
func codexWorkerRows(steps int) []string {
	rows := []string{
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-a"}}`,
		`{"type":"response_item","payload":{"type":"message","id":"msg-a","role":"assistant","content":[{"type":"output_text","text":"First turn done"}]}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-a"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-b"}}`,
	}
	for step := 0; step < steps; step++ {
		rows = append(rows,
			fmt.Sprintf(`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-b","item":{"type":"Reasoning","id":"rs-%d","summary_text":["Step %d"]}}}`, step, step),
			fmt.Sprintf(`{"type":"response_item","payload":{"type":"reasoning","id":"rsr-%d","summary":[{"type":"summary_text","text":"Step %d"}],"encrypted_content":"opaque"}}`, step, step),
			fmt.Sprintf(`{"type":"response_item","payload":{"type":"custom_tool_call","id":"ctc-%d","status":"completed","call_id":"call-%d","name":"exec","input":"ls %d"}}`, step, step, step),
			fmt.Sprintf(`{"type":"response_item","payload":{"type":"custom_tool_call_output","id":"ctco-%d","call_id":"call-%d","output":[{"type":"input_text","text":"listing %d"}]}}`, step, step, step),
			fmt.Sprintf(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":0,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d},"last_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d},"model_context_window":258400}}}`, 100*(step+1), step+1, 101*(step+1), step+1, 100+step+1))
	}
	return rows
}

type codexAppendTarget struct {
	store               *Store
	threadID, path      string
	source              SourceCandidate
	sourceID, sourceGen int64
}

func newCodexAppendTarget(t *testing.T, store *Store, dir string) codexAppendTarget {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: dir}
	metadata := testSource(root, "thread.jsonl", "native-resident", ThreadMain, nil)
	applied, err := store.ApplySource(context.Background(), metadata)
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := store.LoadSource(context.Background(), mustRootID(t, store, root), metadata.Source.Path)
	if err != nil {
		t.Fatal(err)
	}
	return codexAppendTarget{store: store, threadID: applied.ThreadID, path: filepath.Join(dir, "thread.jsonl"), source: metadata.Source, sourceID: source.ID, sourceGen: source.Generation}
}

// apply saves content the way the catalog does: a rebuild the first time,
// then an append resumed from the checkpoint. It reports whether the append
// resumed from the store's in-memory continuation.
func (c codexAppendTarget) apply(t *testing.T, content []byte) bool {
	t.Helper()
	ctx := context.Background()
	if err := os.WriteFile(c.path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(c.path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	candidate := c.source
	candidate.Size = int64(len(content))
	request := ParsedFileRequest{Source: candidate, Format: "jsonl", File: file, Reader: bytes.NewReader(content)}
	mode, expected, resumed := TranscriptRebuild, "", false
	checkpoint, exists, err := c.store.LoadTranscriptCheckpointForAppend(ctx, c.threadID)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		mode, expected, resumed = TranscriptAppend, checkpoint.ProjectionKey, checkpoint.resident != nil
		request.Checkpoint, request.Reader = &checkpoint, bytes.NewReader(content[checkpoint.ReadOffset:])
	}
	parsed, err := ProjectParsedFile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	if _, err := c.store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: mode, ThreadID: c.threadID, SourceID: c.sourceID, SourceGeneration: c.sourceGen, Provider: ProviderCodex,
		ParserVersion: parsed.ParserVersion, CompletionRuleVersion: parsed.CompletionRuleVersion, ExpectedProjectionKey: expected, ExpectNoProjection: !exists,
		IndexedAt: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), Result: parsed.Transcript}); err != nil {
		t.Fatal(err)
	}
	return resumed
}

// savedTranscript digests every saved row of the store's one projection,
// with its continuation decoded so entry order does not matter.
func savedTranscript(t *testing.T, store *Store, threadID string) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	for name, query := range map[string]string{
		"items":      `SELECT * FROM conversation_items ORDER BY projected_seq`,
		"turns":      `SELECT * FROM conversation_turns ORDER BY turn_key`,
		"bodies":     `SELECT * FROM conversation_item_bodies ORDER BY 1, 2`,
		"values":     `SELECT * FROM conversation_parse_values ORDER BY 1`,
		"identities": `SELECT * FROM conversation_parse_identities ORDER BY 1, 2, 3`,
		"projection": `SELECT projection_key, complete_offset, complete_lines, item_count, latest_seq, read_offset FROM conversation_projections`,
	} {
		rows, err := store.db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		columns, _ := rows.Columns()
		digest := sha256.New()
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(digest, "%v\n", values)
		}
		rows.Close()
		out[name] = fmt.Sprintf("%x", digest.Sum(nil))
	}
	checkpoint, _, err := store.LoadTranscriptCheckpoint(ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	var continuation any
	if err := json.Unmarshal(checkpoint.ParserState, &continuation); err != nil {
		t.Fatal(err)
	}
	canonical, _ := json.Marshal(continuation)
	out["continuation"] = string(canonical)
	return out
}

// An append to a running Codex turn resumes from the continuation the store
// kept in memory and saves only what changed. Failure caught: a change the
// reader does not report, or a resumed save that differs from a whole save,
// so that after a restart in the middle of the turn Nano would continue from
// different state than it held. Boundary: the store's append path, against a
// copy reopened before every append (the restart path).
func TestTranscriptResidentAppendsMatchReopenedAppends(t *testing.T) {
	rows := codexWorkerRows(12)
	dir := t.TempDir()
	memoryPath, reopenedPath := filepath.Join(dir, "memory.sqlite"), filepath.Join(dir, "reopened.sqlite")
	store, err := Open(memoryPath)
	if err != nil {
		t.Fatal(err)
	}
	memory := newCodexAppendTarget(t, store, filepath.Join(dir, "memory"))
	memory.apply(t, []byte(strings.Join(rows[:6], "\n")+"\n"))
	store.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if raw, err := os.ReadFile(memoryPath + suffix); err == nil {
			if err := os.WriteFile(reopenedPath+suffix, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if memory.store, err = Open(memoryPath); err != nil {
		t.Fatal(err)
	}
	defer memory.store.Close()
	reopened := memory
	resumed := 0
	for end := 7; end <= len(rows); end++ {
		content := []byte(strings.Join(rows[:end], "\n") + "\n")
		if memory.apply(t, content) {
			resumed++
		}
		if reopened.store, err = Open(reopenedPath); err != nil {
			t.Fatal(err)
		}
		if reopened.apply(t, content) {
			t.Fatal("a reopened store resumed from memory")
		}
		reopened.store.Close()
	}
	if resumed != len(rows)-7 {
		t.Fatalf("%d of %d appends resumed from memory", resumed, len(rows)-6)
	}
	if reopened.store, err = Open(reopenedPath); err != nil {
		t.Fatal(err)
	}
	defer reopened.store.Close()
	want, got := savedTranscript(t, reopened.store, reopened.threadID), savedTranscript(t, memory.store, memory.threadID)
	for name := range want {
		if got[name] != want[name] {
			t.Errorf("%s saved from memory differs from a reopened store's", name)
		}
	}
}

// A token count on a long running Codex turn saves the new sample, not every
// earlier one: the turn row and its continuation entry carried the whole
// usage list, about 2.6 KB more per earlier count, so a worker turn of
// thousands of counts rewrote megabytes on each append. What may still grow
// is the turn's list of sample references, a few dozen bytes per count.
// Boundary: the statement bytes of one append, for a turn with few and with
// many earlier counts.
func TestTranscriptAppendTokenCountWritesAreBounded(t *testing.T) {
	registerCountingDriver(t)
	measure := func(steps int) int64 {
		t.Helper()
		dir := t.TempDir()
		store, err := openStore(countingDriverName, filepath.Join(dir, "history.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		target := newCodexAppendTarget(t, store, dir)
		rows := codexWorkerRows(steps + 1)
		target.apply(t, []byte(strings.Join(rows[:len(rows)-1], "\n")+"\n"))
		atomic.StoreInt64(&stmtArgumentBytes, 0)
		target.apply(t, []byte(strings.Join(rows, "\n")+"\n"))
		return atomic.LoadInt64(&stmtArgumentBytes)
	}
	small, large := measure(10), measure(400)
	t.Logf("append after 10 steps wrote %d bytes, after 400 steps %d", small, large)
	if perStep := (large - small) / 390; perStep > 256 {
		t.Fatalf("one token count's write grew by %d bytes per earlier count: %d bytes after 10 steps, %d after 400", perStep, small, large)
	}
}

// The store keeps a continuation in memory only for a running turn and
// within its per-thread and total bounds; past them a thread reads its saved
// rows, as before. Boundary: residentContinuations.settle.
func TestResidentContinuationsStayBounded(t *testing.T) {
	open, _, err := parsecodex.ParseFile(parsecodex.State{}, model.Input{Bytes: []byte(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t"}}` + "\n"), Source: model.SourceRef{Label: "f", Incarnation: "i"}})
	if err != nil {
		t.Fatal(err)
	}
	settled, _, err := parsecodex.ParseFile(open, model.Input{Bytes: []byte(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t"}}` + "\n"), Source: model.SourceRef{Label: "f", Incarnation: "i"}})
	if err != nil {
		t.Fatal(err)
	}
	copyOf := func(state parsecodex.State, bytes int) *residentContinuation {
		return &residentContinuation{reader: state, bytes: bytes, recorded: true}
	}
	var r residentContinuations
	r.settle("settled", copyOf(settled, 1), true)
	r.settle("failed", copyOf(open, 1), false)
	r.settle("too-large", copyOf(open, residentThreadBytes+1), true)
	if len(r.threads) != 0 {
		t.Fatalf("kept %d copies that should not be kept", len(r.threads))
	}
	share := residentTotalBytes / 3
	for _, thread := range []string{"a", "b", "c", "d"} {
		r.settle(thread, copyOf(open, share), true)
		time.Sleep(time.Millisecond)
	}
	if r.bytes > residentTotalBytes || len(r.threads) != 3 || r.threads["a"] != nil {
		t.Fatalf("kept %d bytes in %d copies (a kept: %v), bound %d", r.bytes, len(r.threads), r.threads["a"] != nil, residentTotalBytes)
	}
}

// A save resumed from memory holds only the changes since the copy it resumed
// from. Found in review: two appends prepared from one copy were both
// accepted, and the second, which never saw the first's new turn, left that
// turn on disk while memory dropped it; after a restart, late usage reopened
// the completed turn. Boundary: ApplyTranscript, saving the shorter append
// and then the longer one prepared from the same copy.
func TestTranscriptResumedSaveRejectsAStaleBase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	target := newCodexAppendTarget(t, store, filepath.Join(dir, "source"))
	seed := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"base"}}` + "\n"
	target.apply(t, []byte(seed))
	checkpoint, _, err := store.LoadTranscriptCheckpointForAppend(ctx, target.threadID)
	if err != nil || checkpoint.resident == nil {
		t.Fatalf("no copy in memory to resume from: %v", err)
	}
	first := `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"base"}}` + "\n" + `{"type":"event_msg","payload":{"type":"task_started","turn_id":"middle"}}` + "\n"
	second := first + `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"middle"}}` + "\n" + `{"type":"event_msg","payload":{"type":"task_started","turn_id":"last"}}` + "\n"
	save := func(appended string) error {
		candidate := target.source
		candidate.Size = int64(len(seed) + len(appended))
		parsed, err := ProjectParsedFile(ctx, ParsedFileRequest{Source: candidate, Reader: strings.NewReader(appended), Checkpoint: &checkpoint, SpoolDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		defer parsed.Close()
		_, err = store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptAppend, ThreadID: target.threadID, SourceID: target.sourceID, SourceGeneration: target.sourceGen, Provider: ProviderCodex,
			ParserVersion: parsed.ParserVersion, CompletionRuleVersion: parsed.CompletionRuleVersion, ExpectedProjectionKey: checkpoint.ProjectionKey, Result: parsed.Transcript})
		return err
	}
	if err := save(first); err != nil {
		t.Fatal(err)
	}
	if err := save(second); !errors.Is(err, ErrTranscriptChanged) {
		t.Fatalf("a save resumed from a superseded copy = %v, want ErrTranscriptChanged", err)
	}
}

// A zero-byte append saves nothing, so its proposed copy must not replace
// the saved one. Found in review: resuming a checkpoint saved before usage
// samples left Turns moved them in memory; a zero-byte append then published
// that copy, the next append saved only its new sample, and after a restart
// the turn had lost its earlier ones. Boundary: the store's append path,
// from a checkpoint in the earlier layout through a restart.
func TestTranscriptNoopAppendKeepsTheSavedCopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	target := newCodexAppendTarget(t, store, filepath.Join(dir, "source"))
	count := func(last, total int) string {
		return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":%d},"total_token_usage":{"input_tokens":%d}}}}`+"\n", last, total)
	}
	seed := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"t"}}` + "\n" + count(1, 1)
	target.apply(t, []byte(seed))
	// Save the checkpoint the way the previous reader did: version 22, usage
	// samples inside their turn.
	checkpoint, _, err := store.LoadTranscriptCheckpoint(ctx, target.threadID)
	if err != nil {
		t.Fatal(err)
	}
	var file parsedFileCheckpoint
	if err := json.Unmarshal(checkpoint.ParserState, &file); err != nil {
		t.Fatal(err)
	}
	var older map[string]json.RawMessage
	if err := json.Unmarshal(file.Reader, &older); err != nil {
		t.Fatal(err)
	}
	var turns map[string]model.Turn
	var usage map[string]model.UsageObservation
	var counts map[string]int
	json.Unmarshal(older["turns"], &turns)
	json.Unmarshal(older["usage"], &usage)
	json.Unmarshal(older["usageCounts"], &counts)
	for key, turn := range turns {
		for i := 0; i < counts[key]; i++ {
			turn.UsageObservations = append(turn.UsageObservations, usage[fmt.Sprintf("%s#%d", key, i)])
		}
		turns[key] = turn
	}
	older["turns"], _ = json.Marshal(turns)
	older["version"] = json.RawMessage(`22`)
	delete(older, "usage")
	delete(older, "usageCounts")
	file.Reader, _ = json.Marshal(older)
	full, _ := json.Marshal(file)
	base, entries, err := splitTranscriptContinuation(full)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var projection int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM conversation_projections WHERE state='active'`).Scan(&projection); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversation_projections SET parser_state_json=? WHERE id=?`, base, projection); err != nil {
		t.Fatal(err)
	}
	if err := syncContinuationEntries(ctx, tx, projection, entries); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	store.resident.settle(target.threadID, nil, false)
	target.apply(t, []byte(seed))
	target.apply(t, []byte(seed+count(2, 3)))
	store.resident.settle(target.threadID, nil, false) // a restart
	target.apply(t, []byte(seed+count(2, 3)+count(3, 6)))
	var raw []byte
	if err := store.db.QueryRowContext(ctx, `SELECT parsed_json FROM conversation_turns WHERE native_turn_id='t'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	read, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback()
	turn, err := decodeParsedTurnTx(ctx, read, projection, raw)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Usage == nil || turn.Usage.Input == nil || *turn.Usage.Input != 6 {
		t.Fatalf("turn usage after a zero-byte append and a restart = %+v, want input 6", turn.Usage)
	}
}

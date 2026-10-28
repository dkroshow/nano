package threadindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

func TestFrozenClaudeReplayThroughCatalogFileCutover(t *testing.T) {
	sourcePath := os.Getenv("NANO_LATE_REPLAY_FIXTURE")
	if sourcePath == "" {
		t.Skip("set NANO_LATE_REPLAY_FIXTURE to frozen receipt 00245-0821da629feb")
	}
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != "0821da629feb99e59581926dca9c15712d1a569cc8c34ec58ffa7b85b73212e0" {
		t.Fatal("frozen late-replay source changed")
	}
	lines := bytes.Split(raw, []byte{'\n'})
	var target struct {
		UUID string `json:"uuid"`
	}
	if json.Unmarshal(lines[987], &target) != nil || target.UUID == "" {
		t.Fatal("frozen target missing")
	}
	var replay struct {
		UUID string `json:"uuid"`
	}
	if json.Unmarshal(lines[1228], &replay) != nil || replay.UUID != target.UUID {
		t.Fatal("frozen replay does not repeat the target UUID")
	}
	firstHash, replayHash := sha256.Sum256(lines[987]), sha256.Sum256(lines[1228])
	root := history.SourceRoot{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: t.TempDir()}
	path := filepath.Join(root.Path, "1dea94cf-56df-5ff7-ac8c-7e149364bf92.jsonl")
	database := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := history.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	persisted, err := store.EnsureRoot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{store: store, hub: newCatalogHub(), transcriptChanges: make(map[string]*pendingTranscriptChange)}
	service.transcriptChangedCond = sync.NewCond(&service.transcriptChangedMu)
	var initialHash string
	var finalCheckpoint history.TranscriptCheckpoint
	reader, err := sql.Open("sqlite", "file:"+filepath.ToSlash(database)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	started := time.Now()
	for _, end := range []int{1101, 1229} {
		flags := os.O_CREATE | os.O_WRONLY
		start := 0
		if end == 1229 {
			flags = os.O_APPEND | os.O_WRONLY
			start = 1101
		}
		file, err := os.OpenFile(path, flags, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.Write(append(bytes.Join(lines[start:end], []byte{'\n'}), '\n'))
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatal("write disposable prefix", writeErr, closeErr)
		}
		if _, err := service.refreshRoot(context.Background(), persisted, catalogScanRecovery, nil, nil); err != nil {
			t.Fatal(err)
		}
		stored, exists, err := store.LoadSource(context.Background(), persisted.ID, path)
		if err != nil || !exists {
			t.Fatal("frozen source absent", err)
		}
		checkpoint, exists, err := store.LoadTranscriptCheckpointHeader(context.Background(), stored.ThreadID)
		if err != nil || !exists || checkpoint.ErrorSummary != nil || checkpoint.CompleteLines != end {
			t.Fatalf("frozen cutover checkpoint: exists=%t lines=%d error=%v err=%v", exists, checkpoint.CompleteLines, checkpoint.ErrorSummary, err)
		}
		if end == 1229 && checkpoint.ProjectionKey != finalCheckpoint.ProjectionKey {
			t.Fatal("append rebuilt instead of restoring continuation")
		}
		finalCheckpoint = checkpoint
		var seq int64
		var sourceKey, text string
		var ordinal, line int
		err = reader.QueryRow(`SELECT i.projected_seq,i.source_key,i.turn_ordinal,i.source_line,i.display_text FROM conversation_items i JOIN conversation_item_observations m ON m.projection_id=i.projection_id AND m.item_id=i.item_id JOIN conversation_observations o ON o.projection_id=m.projection_id AND o.id=m.observation_id JOIN conversation_projections p ON p.id=i.projection_id WHERE p.projection_key=? AND o.source_line=988 AND o.source_sha256=? LIMIT 1`, checkpoint.ProjectionKey, firstHash[:]).Scan(&seq, &sourceKey, &ordinal, &line, &text)
		if err != nil || line != 988 {
			t.Fatalf("frozen target source line=%d err=%v", line, err)
		}
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%d:%s", seq, sourceKey, ordinal, text))))
		if initialHash == "" {
			initialHash = digest
		} else if digest != initialHash {
			t.Fatal("late replay changed first view content or position")
		}
	}
	var rows, observations, aliases, replays, total int
	var itemID string
	err = reader.QueryRow(`SELECT COUNT(DISTINCT m.item_id),COUNT(DISTINCT o.id),MIN(m.item_id) FROM conversation_observations o JOIN conversation_item_observations m ON m.projection_id=o.projection_id AND m.observation_id=o.id JOIN conversation_projections p ON p.id=o.projection_id WHERE p.projection_key=? AND ((o.source_line=988 AND o.source_sha256=?) OR (o.source_line=1229 AND o.source_sha256=?))`, finalCheckpoint.ProjectionKey, firstHash[:], replayHash[:]).Scan(&rows, &observations, &itemID)
	if err != nil || rows != 1 || observations != 2 {
		t.Fatalf("target rows=%d observations=%d err=%v", rows, observations, err)
	}
	err = reader.QueryRow(`SELECT COUNT(*) FROM conversation_parse_identities a JOIN conversation_projections p ON p.id=a.projection_id WHERE p.projection_key=? AND a.item_id=? AND a.identity_kind='local'`, finalCheckpoint.ProjectionKey, itemID).Scan(&aliases)
	if err != nil || aliases < 1 || aliases > 2 {
		t.Fatalf("aliases=%d err=%v", aliases, err)
	}
	err = reader.QueryRow(`SELECT COUNT(*) FROM conversation_relations r JOIN conversation_observations ob ON ob.projection_id=r.projection_id AND ob.key_id=r.from_observation_value_id JOIN conversation_item_observations o ON o.projection_id=ob.projection_id AND o.observation_id=ob.id JOIN conversation_projections p ON p.id=r.projection_id WHERE p.projection_key=? AND o.item_id=? AND r.relation_kind='replay_of'`, finalCheckpoint.ProjectionKey, itemID).Scan(&replays)
	if err != nil || replays == 0 {
		t.Fatalf("replay relations=%d err=%v", replays, err)
	}
	err = reader.QueryRow(`SELECT item_count FROM conversation_projections WHERE projection_key=?`, finalCheckpoint.ProjectionKey).Scan(&total)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("production_file_rows=%d target_rows=%d observations=%d local_aliases=%d replay_of=true target_view_sha256=%s seconds=%.3f", total, rows, observations, aliases, initialHash, time.Since(started).Seconds())
}

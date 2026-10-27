package history

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestParsedTranscriptPersistsUnfinishedReadProgress(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "pending")
	request := TranscriptApplyRequest{Mode: TranscriptRebuild, Provider: ProviderClaude, ParserVersion: "pending-test", CompletionRuleVersion: "pending-test"}
	if err := store.db.QueryRow(`SELECT thread_id,source_file_id,source_generation FROM conversation_projections WHERE id=?`, projection).Scan(&request.ThreadID, &request.SourceID, &request.SourceGeneration); err != nil {
		t.Fatal(err)
	}
	request.Result = TranscriptProjectionResult{ProjectionKey: "pending-state", ReadOffset: 3, ParserState: []byte(`{"pending":"abc"}`), parsedBatches: func(yield func(model.Batch) error) error { return yield(model.Batch{}) }}
	ctx := context.Background()
	if _, err := store.ApplyTranscript(ctx, request); err != nil {
		t.Fatal(err)
	}
	request.Mode, request.ExpectedProjectionKey = TranscriptAppend, request.Result.ProjectionKey
	request.Result.ReadOffset, request.Result.ParserState = 4, []byte(`{"pending":"abcd"}`)
	called := 0
	request.Result.parsedBatches = func(yield func(model.Batch) error) error { called++; return yield(model.Batch{}) }
	got, err := store.ApplyTranscript(ctx, request)
	if err != nil || !got.Changed || called != 1 || got.ItemCount != 0 {
		t.Fatalf("unfinished append did not commit continuation: %+v called=%d err=%v", got, called, err)
	}
	checkpoint, exists, err := store.LoadTranscriptCheckpoint(ctx, request.ThreadID)
	if err != nil || !exists || checkpoint.ReadOffset != 4 || checkpoint.CompleteOffset != 0 || checkpoint.CompleteLines != 0 || string(checkpoint.ParserState) != string(request.Result.ParserState) {
		t.Fatalf("read and complete offsets collapsed: %+v err=%v", checkpoint, err)
	}
	got, err = store.ApplyTranscript(ctx, request)
	if err != nil || got.Changed || called != 1 {
		t.Fatalf("read checkpoint retry rewrote state: %+v called=%d err=%v", got, called, err)
	}
	request.Result.ReadOffset = 1
	request.Result.CompleteOffset = 2
	if _, err := store.ApplyTranscript(ctx, request); !errors.Is(err, ErrInvalidTranscriptPage) {
		t.Fatalf("read offset before complete boundary accepted: %v", err)
	}
}

func TestTranscriptCheckpointHeadersNeverReadStateColumn(t *testing.T) {
	probe, err := sql.Open("sqlite", "file:checkpoint-probe?mode=memory")
	if err != nil {
		t.Fatal(err)
	}
	base := probe.Driver()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	var refuse atomic.Bool
	name := fmt.Sprintf("checkpoint-state-guard-%d", checkpointGuardNames.Add(1))
	sql.Register(name, checkpointGuardDriver{Driver: base, refuse: &refuse})
	store, err := openStore(name, filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "headers")
	var thread string
	var root int64
	if err := store.db.QueryRow(`SELECT p.thread_id,t.source_root_id FROM conversation_projections p JOIN threads t ON t.id=p.thread_id WHERE p.id=?`, projection).Scan(&thread, &root); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE conversation_projections SET complete_offset=10,read_offset=12,complete_lines=2,parser_state_json=? WHERE id=?`, []byte(`{"pending":"ab"}`), projection); err != nil {
		t.Fatal(err)
	}
	refuse.Store(true)
	ctx := context.Background()
	header, exists, err := store.LoadTranscriptCheckpointHeader(ctx, thread)
	if err != nil || !exists || len(header.ParserState) != 0 || header.ReadOffset != 12 || header.CompleteOffset != 10 {
		t.Fatalf("single header read parser state or lost guards: %+v %v", header, err)
	}
	headers, err := store.LoadTranscriptCheckpointHeadersByRoot(ctx, root)
	if err != nil || len(headers) != 1 || headers[thread].ProjectionKey != header.ProjectionKey || len(headers[thread].ParserState) != 0 || headers[thread].ReadOffset != 12 {
		t.Fatalf("root headers read parser state or lost guards: %v", err)
	}
	if _, _, err := store.LoadTranscriptCheckpoint(ctx, thread); err == nil {
		t.Fatal("test guard did not reject the parser state column")
	}
	refuse.Store(false)
	full, _, err := store.LoadTranscriptCheckpoint(ctx, thread)
	if err != nil || string(full.ParserState) != `{"pending":"ab"}` || full.ReadOffset != header.ReadOffset {
		t.Fatalf("eligible append cannot load full continuation: %v", err)
	}
	// Previous binaries insert the default zero. It means complete_offset,
	// preserving their append boundary while the new column is unused.
	if _, err := store.db.Exec(`UPDATE conversation_projections SET read_offset=0 WHERE id=?`, projection); err != nil {
		t.Fatal(err)
	}
	full, _, err = store.LoadTranscriptCheckpoint(ctx, thread)
	if err != nil || full.ReadOffset != 10 {
		t.Fatalf("legacy read boundary=%d err=%v", full.ReadOffset, err)
	}
}

var checkpointGuardNames atomic.Uint64

type checkpointGuardDriver struct {
	driver.Driver
	refuse *atomic.Bool
}

func (d checkpointGuardDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return checkpointGuardConn{Conn: conn, refuse: d.refuse}, nil
}

type checkpointGuardConn struct {
	driver.Conn
	refuse *atomic.Bool
}

func (c checkpointGuardConn) Prepare(query string) (driver.Stmt, error) {
	if c.refuse.Load() && strings.Contains(strings.ToLower(query), "parser_state_json") {
		return nil, errors.New("test forbids reading parser continuation")
	}
	return c.Conn.Prepare(query)
}

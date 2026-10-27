package transcriptparse_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/claude"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// This opt-in test uses the frozen private source that produced the 253 MB
// checkpoint. Its bytes never become repository fixtures or test output.
func TestClaudeLargestCorpusCheckpointBound(t *testing.T) {
	path := os.Getenv("NANO_CHECKPOINT_FIXTURE")
	if path == "" {
		t.Skip("set NANO_CHECKPOINT_FIXTURE to the frozen sample-task largest-checkpoint source")
	}
	if !filepath.IsAbs(path) {
		t.Fatal("fixture path must be absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(hash.Sum(nil)) != "ca82a60316f5f1c19852e5960739969dc8b254b50bf77ca7cd1f0ea7e1bc96dd" {
		t.Fatal("fixture is not the frozen largest-checkpoint source")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var state claude.State
	input := model.Input{Source: model.SourceRef{Label: "largest-checkpoint", Incarnation: "frozen-1"}}
	buffer := make([]byte, 4<<20)
	observations, changes := 0, 0
	oversized := 0
	started := time.Now()
	for {
		n, readErr := file.Read(buffer)
		if readErr != nil && readErr != io.EOF {
			t.Fatal(readErr)
		}
		input.Bytes = buffer[:n]
		var batch model.Batch
		state, batch, err = claude.ParseFile(state, input)
		if err != nil {
			t.Fatal(err)
		}
		for _, diagnostic := range batch.Diagnostics {
			if diagnostic.Code == "continuation_limit_exceeded" {
				oversized++
			}
		}
		observations += len(batch.Observations)
		changes += len(batch.Changes)
		if readErr == io.EOF {
			break
		}
	}
	t.Logf("parse=%s oversized-open-batches=%d", time.Since(started), oversized)
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("checkpoint=%d bytes; retained items=%d turns=%d; emitted observations=%d changes=%d", len(raw), len(state.Items), len(state.Turns), observations, changes)
	if len(raw) > 4<<20 {
		t.Fatalf("open-file continuation checkpoint is %d bytes; ceiling is %d", len(raw), 4<<20)
	}
	if state.Framing.Closed {
		t.Fatal("file was artificially closed to satisfy the bound")
	}
	restoreStart := time.Now()
	var restored claude.State
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	restoreElapsed := time.Since(restoreStart)
	input.Bytes = []byte(`{"type":"user","uuid":"bound-append","message":{"content":"Continue the bound check"}}` + "\n")
	appendStart := time.Now()
	appended, batch, err := claude.ParseFile(restored, input)
	if err != nil {
		t.Fatal(err)
	}
	appendElapsed := time.Since(appendStart)
	continuous, continuousBatch, err := claude.ParseFile(state, input)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(struct {
		State claude.State
		Batch model.Batch
	}{appended, batch})
	b, _ := json.Marshal(struct {
		State claude.State
		Batch model.Batch
	}{continuous, continuousBatch})
	if !bytes.Equal(a, b) {
		t.Fatal("restoring bounded continuation changed a semantic append")
	}
	if len(appended.Turns) != 1 || len(appended.Items) != 1 {
		t.Fatal("appended prompt retained the preceding settled turn")
	}
	t.Logf("restore=%s semantic-append=%s", restoreElapsed, appendElapsed)
}

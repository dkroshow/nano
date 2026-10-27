package history

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	parseclaude "github.com/dkroshow/nano/internal/transcriptparse/claude"
	parsecodex "github.com/dkroshow/nano/internal/transcriptparse/codex"
	parsegrok "github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type differentialCheckpointCost struct {
	JSONBytes      int     `json:"jsonBytes"`
	GZIPBytes      int     `json:"gzipBytes"`
	WholeDocument  bool    `json:"wholeDocument,omitempty"`
	EncodeSeconds  float64 `json:"encodeSeconds"`
	DecodeSeconds  float64 `json:"decodeSeconds"`
	RestoreSeconds float64 `json:"restoreSeconds"`
	AppendSeconds  float64 `json:"appendSeconds"`
}

// Measure the ten largest indexed sources per provider. Auxiliary documents
// remain in the corpus comparison but are not history transcript checkpoints.
func measureDifferentialCorpusCheckpoints(t *testing.T, dir string, manifest differentialManifest) {
	t.Helper()
	type receipt struct {
		Source   differentialSource         `json:"source"`
		Versions model.Versions             `json:"versions"`
		Cost     differentialCheckpointCost `json:"cost"`
	}
	var results []receipt
	for _, provider := range []Provider{ProviderClaude, ProviderCodex, ProviderGrok} {
		var sources []differentialSource
		for _, source := range manifest.Sources {
			if source.Source.Root.Provider == provider && !source.Auxiliary {
				sources = append(sources, source)
			}
		}
		sort.Slice(sources, func(i, j int) bool { return sources[i].Source.Size > sources[j].Source.Size })
		if len(sources) > 10 {
			sources = sources[:10]
		}
		for _, source := range sources {
			file, err := os.Open(filepath.Join(dir, source.Snapshot))
			if err != nil {
				t.Fatal(err)
			}
			if err = verifyDifferentialSnapshot(file, source); err != nil {
				file.Close()
				t.Fatal(err)
			}
			snapshot, versions, err := moduleDifferentialSnapshot(io.NewSectionReader(file, 0, source.Source.Size), source, true)
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
			results = append(results, receipt{Source: source, Versions: versions, Cost: *snapshot.CheckpointCost})
			t.Logf("%s source=%d checkpoint=%d gzip=%d restore=%.3fs append=%.3fs", provider, source.Source.Size, snapshot.CheckpointCost.JSONBytes, snapshot.CheckpointCost.GZIPBytes, snapshot.CheckpointCost.RestoreSeconds, snapshot.CheckpointCost.AppendSeconds)
		}
	}
	raw, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "checkpoint-cost.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(append(raw, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func measureDifferentialCheckpoint(raw []byte, input model.Input) (differentialCheckpointCost, error) {
	cost := differentialCheckpointCost{JSONBytes: len(raw)}
	start := time.Now()
	var buffer bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)
	if err != nil {
		return cost, err
	}
	if _, err = writer.Write(raw); err != nil {
		return cost, err
	}
	if err = writer.Close(); err != nil {
		return cost, err
	}
	cost.GZIPBytes = buffer.Len()
	cost.EncodeSeconds = time.Since(start).Seconds()
	start = time.Now()
	reader, err := gzip.NewReader(&buffer)
	if err != nil {
		return cost, err
	}
	restored, err := io.ReadAll(reader)
	if err != nil {
		return cost, err
	}
	if err = reader.Close(); err != nil {
		return cost, err
	}
	cost.DecodeSeconds = time.Since(start).Seconds()
	if !bytes.Equal(restored, raw) {
		return cost, fmt.Errorf("compressed checkpoint changed native state")
	}
	// A blank appended row measures the minimum restore/copy cost. Real semantic
	// updates add work; this is not a throughput claim for arbitrary native rows.
	input.Bytes = []byte("\n")
	input.Closed = false
	start = time.Now()
	switch input.Scope.Provider {
	case model.ProviderClaude:
		var state parseclaude.State
		if err = json.Unmarshal(restored, &state); err != nil {
			return cost, err
		}
		cost.RestoreSeconds = time.Since(start).Seconds()
		start = time.Now()
		_, _, err = parseclaude.ParseFile(state, input)
	case model.ProviderCodex:
		var state parsecodex.State
		if err = json.Unmarshal(restored, &state); err != nil {
			return cost, err
		}
		cost.RestoreSeconds = time.Since(start).Seconds()
		start = time.Now()
		_, _, err = parsecodex.ParseFile(state, input)
	case model.ProviderGrok:
		var state parsegrok.State
		if err = json.Unmarshal(restored, &state); err != nil {
			return cost, err
		}
		cost.RestoreSeconds = time.Since(start).Seconds()
		if state.DocumentClosed {
			cost.WholeDocument = true
			return cost, nil
		}
		start = time.Now()
		_, _, err = parsegrok.ParseFile(state, input)
	default:
		return cost, fmt.Errorf("unknown checkpoint provider")
	}
	cost.AppendSeconds = time.Since(start).Seconds()
	return cost, err
}

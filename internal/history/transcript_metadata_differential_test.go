package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	parseclaude "github.com/dkroshow/nano/internal/transcriptparse/claude"
	parsecodex "github.com/dkroshow/nano/internal/transcriptparse/codex"
	parsegrok "github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type catalogDifferentialReceipt struct {
	Index        int                  `json:"index"`
	Provider     Provider             `json:"provider"`
	SHA256       string               `json:"sha256"`
	LegacyFields map[string]string    `json:"legacyFields"`
	Differences  map[string][2]string `json:"differences,omitempty"`
	LegacyError  bool                 `json:"legacyError,omitempty"`
	ModuleError  bool                 `json:"moduleError,omitempty"`
}

// This audit reuses the frozen bounded corpus. Receipts contain field names and
// hashes, never catalog values. A successful capture is not a parity assertion.
func TestCatalogDifferentialCapture(t *testing.T) {
	dir, report := os.Getenv("NANO_METADATA_DIFFERENTIAL_DIR"), os.Getenv("NANO_METADATA_DIFFERENTIAL_REPORT")
	if dir == "" {
		t.Skip("opt-in audit against a local corpus; set NANO_METADATA_DIFFERENTIAL_DIR and NANO_METADATA_DIFFERENTIAL_REPORT to run")
	}
	if !filepath.IsAbs(dir) || !filepath.IsAbs(report) {
		t.Fatal("absolute corpus directory and new report path required")
	}
	output, err := os.OpenFile(report, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest differentialManifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	hashes := map[string]string{}
	for _, entry := range manifest.Sources {
		hashes[filepath.Clean(filepath.Join(dir, entry.Snapshot))] = entry.SHA256
	}
	oracle := loadCatalogDifferentialOracle(t, manifest)
	result := struct {
		Seconds    float64                      `json:"seconds"`
		Compared   map[Provider]int             `json:"compared"`
		Mismatched map[Provider]int             `json:"mismatched"`
		Auxiliary  int                          `json:"auxiliaryNotIndexed"`
		Receipts   []catalogDifferentialReceipt `json:"receipts"`
	}{Compared: map[Provider]int{}, Mismatched: map[Provider]int{}}
	started := time.Now()
	for i, entry := range manifest.Sources {
		if entry.Auxiliary {
			result.Auxiliary++
			continue
		}
		path := filepath.Join(dir, entry.Snapshot)
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = verifyCatalogFrozenSource(file, entry.SHA256); err != nil {
			file.Close()
			t.Fatal(err)
		}
		source := entry.Source
		source.Path = path
		if err = file.Close(); err != nil {
			t.Fatal(err)
		}
		if source.Root.Provider == ProviderGrok && filepath.Base(path) == "updates.jsonl" {
			path = filepath.Join(filepath.Dir(path), "summary.json")
		}
		file, err = os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = verifyCatalogFrozenSource(file, hashes[filepath.Clean(path)]); err != nil {
			file.Close()
			t.Fatal(err)
		}
		current, parseErr := catalogModuleFile(context.Background(), entry.Source, file)
		if err = file.Close(); err != nil {
			t.Fatal(err)
		}
		record := oracle[i]
		record.ModuleError = parseErr != nil
		if parseErr == nil {
			record.Differences = catalogDigestDifferences(record.LegacyFields, catalogFieldDigests(current))
		}
		result.Compared[record.Provider]++
		if len(record.Differences) != 0 || record.LegacyError || record.ModuleError {
			result.Mismatched[record.Provider]++
		}
		result.Receipts = append(result.Receipts, record)
		if len(result.Receipts)%100 == 0 {
			t.Logf("captured %d indexed sources in %s; unequal %v", len(result.Receipts), time.Since(started).Round(time.Second), result.Mismatched)
		}
	}
	result.Seconds = time.Since(started).Seconds()
	if err = json.NewEncoder(output).Encode(result); err != nil {
		t.Fatal(err)
	}
	t.Logf("catalog capture: compared=%v unequal=%v auxiliary=%d seconds=%.3f; differences require reconciliation", result.Compared, result.Mismatched, result.Auxiliary, result.Seconds)
}

// The old adapter was removed after capture. Require an explicit frozen oracle
// and check every indexed source before comparing current module output.
func loadCatalogDifferentialOracle(t *testing.T, manifest differentialManifest) map[int]catalogDifferentialReceipt {
	t.Helper()
	path := os.Getenv("NANO_METADATA_DIFFERENTIAL_ORACLE")
	if !filepath.IsAbs(path) {
		t.Fatal("absolute NANO_METADATA_DIFFERENTIAL_ORACLE from the pre-removal capture is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Receipts []catalogDifferentialReceipt `json:"receipts"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	oracle := make(map[int]catalogDifferentialReceipt)
	for _, receipt := range report.Receipts {
		if receipt.Index < 0 || receipt.Index >= len(manifest.Sources) {
			t.Fatal("catalog oracle index outside manifest")
		}
		entry := manifest.Sources[receipt.Index]
		_, duplicate := oracle[receipt.Index]
		if duplicate || entry.Auxiliary || receipt.Provider != entry.Source.Root.Provider || receipt.SHA256 != entry.SHA256 || receipt.LegacyError || len(receipt.LegacyFields) == 0 {
			t.Fatal("catalog oracle is incomplete, duplicated or belongs to another source")
		}
		oracle[receipt.Index] = receipt
	}
	for index, entry := range manifest.Sources {
		if _, ok := oracle[index]; !entry.Auxiliary && !ok {
			t.Fatal("catalog oracle omits an indexed source")
		}
	}
	return oracle
}

func catalogDigestDifferences(before, after map[string]string) map[string][2]string {
	differences := map[string][2]string{}
	for key, value := range before {
		if value != after[key] {
			differences[key] = [2]string{value, after[key]}
		}
	}
	for key, value := range after {
		if value != before[key] {
			differences[key] = [2]string{before[key], value}
		}
	}
	return differences
}

func verifyCatalogFrozenSource(file *os.File, expected string) error {
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if expected == "" || fmt.Sprintf("%x", hash.Sum(nil)) != expected {
		return fmt.Errorf("frozen source hash mismatch")
	}
	_, err := file.Seek(0, io.SeekStart)
	return err
}

func catalogModuleFile(ctx context.Context, source SourceCandidate, file *os.File) (ThreadProjection, error) {
	info, err := file.Stat()
	if err != nil {
		return ThreadProjection{}, err
	}
	state := newTranscriptMetadataState(source.Root.Provider, source, nil)
	var claudeState parseclaude.State
	var codexState parsecodex.State
	var grokState parsegrok.State
	buffer := make([]byte, 4<<20)
	var offset int64
	for {
		if err = ctx.Err(); err != nil {
			return ThreadProjection{}, err
		}
		n, readErr := file.Read(buffer)
		if readErr != nil && readErr != io.EOF {
			return ThreadProjection{}, readErr
		}
		offset += int64(n)
		input := model.Input{Bytes: buffer[:n], Closed: offset == info.Size(), Source: model.SourceRef{Label: "catalog", Incarnation: "frozen"}}
		var batch model.Batch
		switch source.Root.Provider {
		case ProviderClaude:
			claudeState, batch, err = parseclaude.ParseFile(claudeState, input)
		case ProviderCodex:
			codexState, batch, err = parsecodex.ParseFile(codexState, input)
		case ProviderGrok:
			input.Format = "json"
			grokState, batch, err = parsegrok.ParseFile(grokState, input)
		}
		if err != nil {
			return ThreadProjection{}, err
		}
		for _, obs := range batch.Observations {
			applyTranscriptMetadata(&state, obs)
		}
		if input.Closed {
			break
		}
	}
	if state.projection.UpdatedAt == nil {
		state.projection.UpdatedAt = cloneTime(state.lastActivity)
	}
	applyFilenameFallback(&state, source)
	return state.projection, nil
}

func catalogFieldDifferences(old, current ThreadProjection) map[string][2]string {
	before, after := catalogFields(old), catalogFields(current)
	keys := map[string]bool{}
	for key := range before {
		keys[key] = true
	}
	for key := range after {
		keys[key] = true
	}
	differences := map[string][2]string{}
	for key := range keys {
		if !bytes.Equal(before[key], after[key]) {
			differences[key] = [2]string{fmt.Sprintf("%x", sha256.Sum256(before[key])), fmt.Sprintf("%x", sha256.Sum256(after[key]))}
		}
	}
	return differences
}

func catalogFields(projection ThreadProjection) map[string]json.RawMessage {
	raw, _ := json.Marshal(projection)
	var values map[string]json.RawMessage
	_ = json.Unmarshal(raw, &values)
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(values["metadata"], &metadata)
	delete(values, "metadata")
	for key, value := range metadata {
		values["metadata."+key] = value
	}
	return values
}

func catalogFieldDigests(projection ThreadProjection) map[string]string {
	fields := map[string]string{}
	for key, value := range catalogFields(projection) {
		fields[key] = fmt.Sprintf("%x", sha256.Sum256(value))
	}
	return fields
}

func TestCatalogDifferentialRejectsChangedFrozenInput(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "source")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	raw := []byte("original source\n")
	if _, err = file.Write(raw); err != nil {
		t.Fatal(err)
	}
	expected := fmt.Sprintf("%x", sha256.Sum256(raw))
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err = verifyCatalogFrozenSource(file, expected); err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt([]byte("changed"), 0); err != nil {
		t.Fatal(err)
	}
	if err = verifyCatalogFrozenSource(file, expected); err == nil {
		t.Fatal("changed source was accepted as the frozen oracle")
	}
}

func TestCatalogDifferenceReceiptOmitsNativeValues(t *testing.T) {
	old := ThreadProjection{NativeThreadID: "native-private-identity"}
	current := old
	current.Title = stringPointer("private conversation title")
	setKnown(&current.Metadata.Model, "private model name", nil)
	diff := catalogFieldDifferences(old, current)
	if len(diff) != 2 || diff["title"][1] == "" || diff["metadata.model"][1] == "" {
		t.Fatal("receipt did not identify exactly the changed fields")
	}
	raw, err := json.Marshal(diff)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("private")) || bytes.Contains(raw, []byte("nativeThreadId")) {
		t.Fatal("receipt leaked native values or reported an unchanged field")
	}
}

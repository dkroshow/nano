package history

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type convertedDifferentialEvidence struct {
	Parser             string               `json:"parser"`
	CatalogDifferences map[string][2]string `json:"catalogDifferences,omitempty"`
	CatalogFields      map[string]string    `json:"catalogFields,omitempty"`
	Rows               map[string]int       `json:"rows"`
}

// Each source uses a disposable database. The old receipt is immutable; only
// the new side changes to ProjectParsedFile -> ApplyTranscript -> ReadTranscript.
func captureConvertedDifferential(t *testing.T, dir string, manifest differentialManifest) {
	oracle := loadCatalogDifferentialOracle(t, manifest)
	out := os.Getenv("NANO_DIFFERENTIAL_CONVERTED_DIR")
	if !filepath.IsAbs(out) || filepath.Clean(out) == filepath.Clean(dir) {
		t.Fatal("a separate absolute NANO_DIFFERENTIAL_CONVERTED_DIR is required")
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	for index, entry := range manifest.Sources {
		if only := os.Getenv("NANO_DIFFERENTIAL_ONLY"); only != "" && !strings.Contains(entry.Source.Path, only) {
			continue
		}
		name := fmt.Sprintf("%05d-%s.json.gz", index, entry.SHA256[:12])
		t.Run(name, func(t *testing.T) {
			file, err := os.Open(filepath.Join(dir, entry.Snapshot))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err = verifyDifferentialSnapshot(file, entry); err != nil {
				t.Fatal(err)
			}
			readReceipt := func(path string) differentialReceipt {
				f, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				gz, err := gzip.NewReader(f)
				if err != nil {
					t.Fatal(err)
				}
				defer gz.Close()
				var receipt differentialReceipt
				if err = json.NewDecoder(gz).Decode(&receipt); err != nil {
					t.Fatal(err)
				}
				if _, err = io.Copy(io.Discard, gz); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(receipt.Source, entry) {
					t.Fatal("receipt source mismatch")
				}
				return receipt
			}
			parser, _, err := ParsedFileVersions(entry.Source.Root.Provider)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(out, name)
			if _, err := os.Stat(target); err == nil {
				prior := readReceipt(target)
				if prior.Converted == nil || prior.Converted.Parser != parser {
					t.Fatal("stale converted receipt; choose a new output directory")
				}
				return
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
			receipt := readReceipt(filepath.Join(dir, name))
			parseEntry := entry
			parseEntry.Source.Path = filepath.Join(dir, entry.Snapshot)
			parseEntry.Source.Root.Path = filepath.Join(dir, "sources", string(entry.Source.Root.Kind))
			info, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			parseEntry.Source.Device, parseEntry.Source.Inode = fileIdentity(info)
			started := time.Now()
			current, evidence, versions, err := convertedDifferentialSnapshot(context.Background(), file, parseEntry, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if !entry.Auxiliary {
				evidence.CatalogDifferences = catalogDigestDifferences(oracle[index].LegacyFields, evidence.CatalogFields)
			}
			current.Seconds = time.Since(started).Seconds()
			receipt.Module, receipt.Converted, receipt.Versions = current, evidence, versions
			receipt.Differences = nil
			if !reflect.DeepEqual(receipt.Legacy.Items, current.Items) {
				receipt.Differences = append(receipt.Differences, "items")
			}
			if !reflect.DeepEqual(receipt.Legacy.Turns, current.Turns) {
				receipt.Differences = append(receipt.Differences, "turns")
			}
			if receipt.Legacy.CompleteOffset != current.CompleteOffset || receipt.Legacy.CompleteLines != current.CompleteLines {
				receipt.Differences = append(receipt.Differences, "framing")
			}
			receipt.Differences = append(receipt.Differences, "diagnostic-schema")
			if len(evidence.CatalogDifferences) != 0 {
				receipt.Differences = append(receipt.Differences, "catalog")
			}
			if err = verifyDifferentialSnapshot(file, entry); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			gz := gzip.NewWriter(f)
			encodeErr, closeErr, fileErr := json.NewEncoder(gz).Encode(receipt), gz.Close(), f.Close()
			if encodeErr != nil || closeErr != nil || fileErr != nil {
				t.Fatalf("receipt write: %v %v %v", encodeErr, closeErr, fileErr)
			}
			t.Logf("converted in %.3fs; differences require reconciliation: %v", current.Seconds, receipt.Differences)
		})
	}
}

func convertedDifferentialSnapshot(ctx context.Context, file *os.File, entry differentialSource, scratch string) (differentialSnapshot, *convertedDifferentialEvidence, model.Versions, error) {
	var snapshot differentialSnapshot
	evidence := &convertedDifferentialEvidence{Rows: map[string]int{}}
	reader, err := newModuleFileReader(entry.Source.Root.Provider, nil)
	if err != nil {
		return snapshot, evidence, model.Versions{}, err
	}
	batch, err := reader.feed(model.Input{Source: model.SourceRef{Label: "versions", Incarnation: "versions"}})
	versions := batch.Versions
	if err != nil {
		return snapshot, evidence, versions, err
	}
	req := ParsedFileRequest{Source: entry.Source, Reader: io.NewSectionReader(file, 0, entry.Source.Size), SpoolDir: scratch}
	if entry.Source.Root.Provider == ProviderGrok && filepath.Ext(entry.Source.Path) == ".json" {
		req.Format = "json"
	}
	if !entry.Auxiliary && entry.Source.Root.Provider == ProviderGrok && filepath.Base(entry.Source.Path) == "updates.jsonl" {
		path := filepath.Join(filepath.Dir(entry.Source.Path), "summary.json")
		summary, openErr := os.Open(path)
		if openErr != nil {
			return snapshot, evidence, versions, openErr
		}
		defer summary.Close()
		source := entry.Source
		source.Path = path
		req.Catalog = &ParsedCatalogInput{Source: source, Reader: summary, Format: "json"}
	}
	parsed, err := ProjectParsedFile(ctx, req)
	if err != nil {
		return snapshot, evidence, versions, err
	}
	defer parsed.Close()
	evidence.Parser = parsed.ParserVersion
	evidence.CatalogFields = catalogFieldDigests(parsed.Metadata.Projection)
	store, err := Open(filepath.Join(scratch, "history.sqlite"))
	if err != nil {
		return snapshot, evidence, versions, err
	}
	defer store.Close()
	metadata := parsed.Metadata
	// Auxiliary/empty sources need a consumer identity to exercise storage.
	// This harness-only identity never enters parser input or catalog comparison.
	if metadata.Projection.NativeThreadID == "" {
		metadata.Projection.NativeThreadID = "differential"
		metadata.Projection.IdentityEvidence = EvidenceFilenameFallback
	}
	applied, err := store.ApplySource(ctx, SourceProjection{Source: entry.Source, Result: &metadata, ParserVersion: parsed.ParserVersion, Status: SourcePresent, IndexedAt: time.Unix(200, 0)})
	if err != nil {
		return snapshot, evidence, versions, err
	}
	root, err := store.EnsureRoot(ctx, entry.Source.Root)
	if err != nil {
		return snapshot, evidence, versions, err
	}
	source, exists, err := store.LoadSource(ctx, root.ID, entry.Source.Path)
	if err != nil || !exists {
		return snapshot, evidence, versions, fmt.Errorf("load converted source: exists=%t err=%w", exists, err)
	}
	_, err = store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: source.ID, SourceGeneration: source.Generation, Provider: entry.Source.Root.Provider, ParserVersion: parsed.ParserVersion, CompletionRuleVersion: parsed.CompletionRuleVersion, Result: parsed.Transcript})
	if err != nil {
		return snapshot, evidence, versions, err
	}
	result := TranscriptProjectionResult{CompleteOffset: parsed.Transcript.CompleteOffset, CompleteLines: parsed.Transcript.CompleteLines, ParserState: parsed.Transcript.ParserState}
	turns := map[int]TranscriptTurn{}
	var before int64
	for {
		page, readErr := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: applied.ThreadID, BeforeSeq: before, Limit: MaxTranscriptPageLimit})
		if readErr != nil {
			return snapshot, evidence, versions, readErr
		}
		result.Items = append(result.Items, page.Items...)
		for _, turn := range page.Turns {
			turns[turn.Ordinal] = turn
		}
		if !page.HasMoreBefore {
			break
		}
		if page.StartSeq <= 0 || (before != 0 && page.StartSeq >= before) {
			return snapshot, evidence, versions, fmt.Errorf("converted pagination failed to advance")
		}
		before = page.StartSeq
	}
	sort.Slice(result.Items, func(i, j int) bool { return result.Items[i].ProjectedSeq < result.Items[j].ProjectedSeq })
	for _, turn := range turns {
		result.Turns = append(result.Turns, turn)
	}
	sort.Slice(result.Turns, func(i, j int) bool { return result.Turns[i].Ordinal < result.Turns[j].Ordinal })
	for i := range result.Turns {
		turn := &result.Turns[i]
		// The transcript API omits these usage fields. Compare the columns
		// consumed by ThreadTurnUsage with the old parser's returned fields.
		if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(usage_model,''), COALESCE(usage_effort,''), COALESCE(usage_service_tier,'') FROM conversation_turns WHERE turn_key=?`, turn.Key).Scan(&turn.Model, &turn.Effort, &turn.ServiceTier); err != nil {
			return snapshot, evidence, versions, err
		}
	}
	for i := range result.Items {
		item := &result.Items[i]
		if err := store.db.QueryRowContext(ctx, `SELECT source_line, COALESCE(source_row_type,''), COALESCE(source_payload_type,''), source_line_sha256, emission_index FROM conversation_items WHERE item_id=?`, item.ID).Scan(&item.SourceLine, &item.SourceRowType, &item.SourcePayloadType, &item.SourceLineSHA256, &item.EmissionIndex); err != nil {
			return snapshot, evidence, versions, err
		}
		if item.ContentRef != "" {
			body, bodyErr := store.LoadTranscriptBody(ctx, applied.ThreadID, item.ID)
			if bodyErr != nil {
				return snapshot, evidence, versions, bodyErr
			}
			result.Bodies = append(result.Bodies, TranscriptBody{ItemID: item.ID, Text: body.Text})
		}
	}
	snapshot = viewDifferentialSnapshot(result)
	var diagnostics []model.Diagnostic
	rows, err := store.db.QueryContext(ctx, `SELECT diagnostic_json FROM conversation_parse_diagnostics ORDER BY id`)
	if err != nil {
		return snapshot, evidence, versions, err
	}
	for rows.Next() {
		var raw []byte
		var diagnostic model.Diagnostic
		if err = rows.Scan(&raw); err == nil {
			err = json.Unmarshal(raw, &diagnostic)
		}
		if err != nil {
			rows.Close()
			return snapshot, evidence, versions, err
		}
		diagnostics = append(diagnostics, diagnostic)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return snapshot, evidence, versions, err
	}
	snapshot.Diagnostics = diagnostics
	for _, table := range []string{"conversation_items", "conversation_turns", "conversation_observations", "conversation_relations", "conversation_parse_diagnostics"} {
		var count int
		if err = store.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			return snapshot, evidence, versions, err
		}
		evidence.Rows[table] = count
	}
	return snapshot, evidence, versions, nil
}

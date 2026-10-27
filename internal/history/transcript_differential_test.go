package history

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	parseclaude "github.com/dkroshow/nano/internal/transcriptparse/claude"
	parsecodex "github.com/dkroshow/nano/internal/transcriptparse/codex"
	parsegrok "github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// This opt-in capture runs beside the old adapters before their removal. Its
// content-free receipts survive the cut-over: a later run must match the same
// frozen source hashes before comparing results. No live database is opened.
type differentialSource struct {
	Source    SourceCandidate `json:"source"`
	SHA256    string          `json:"sha256"`
	Auxiliary bool            `json:"auxiliary,omitempty"`
	Snapshot  string          `json:"snapshot"`
}
type differentialManifest struct {
	Rule    string               `json:"rule"`
	Counts  map[Provider]int     `json:"counts"`
	Sources []differentialSource `json:"sources"`
}
type differentialItem struct {
	Line       int             `json:"line"`
	Emission   int             `json:"emission"`
	Row        string          `json:"row,omitempty"`
	Payload    string          `json:"payload,omitempty"`
	SourceHash string          `json:"sourceHash,omitempty"`
	NativeID   string          `json:"nativeId,omitempty"`
	Turn       int             `json:"turn"`
	Type       string          `json:"type"`
	Role       string          `json:"role,omitempty"`
	Status     string          `json:"status"`
	TextHash   string          `json:"textHash"`
	TextBytes  int             `json:"textBytes"`
	Tool       string          `json:"tool,omitempty"`
	Call       string          `json:"call,omitempty"`
	Detail     json.RawMessage `json:"detail,omitempty"`
	CreatedAt  *time.Time      `json:"createdAt,omitempty"`
	Phase      string          `json:"phase,omitempty"`
	Tags       []string        `json:"tags,omitempty"`
	Synthetic  bool            `json:"synthetic,omitempty"`
	Sidechain  bool            `json:"sidechain,omitempty"`
	Final      bool            `json:"final,omitempty"`
}
type differentialTurn struct {
	Ordinal     int                   `json:"ordinal"`
	NativeID    string                `json:"nativeId,omitempty"`
	Start       int64                 `json:"start"`
	End         int64                 `json:"end"`
	Status      string                `json:"status"`
	Completion  string                `json:"completion"`
	Final       int                   `json:"final"`
	StartedAt   *time.Time            `json:"startedAt,omitempty"`
	CompletedAt *time.Time            `json:"completedAt,omitempty"`
	Duration    int64                 `json:"duration"`
	Config      json.RawMessage       `json:"config,omitempty"`
	Usage       *TranscriptTokenUsage `json:"usage,omitempty"`
	Model       string                `json:"model,omitempty"`
	Effort      string                `json:"effort,omitempty"`
	Tier        string                `json:"tier,omitempty"`
}
type differentialSnapshot struct {
	Items           []differentialItem          `json:"items"`
	Turns           []differentialTurn          `json:"turns"`
	Diagnostics     any                         `json:"diagnostics"`
	CompleteOffset  int64                       `json:"completeOffset"`
	CompleteLines   int                         `json:"completeLines"`
	CheckpointBytes int                         `json:"checkpointBytes"`
	CheckpointCost  *differentialCheckpointCost `json:"checkpointCost,omitempty"`
	Seconds         float64                     `json:"seconds"`
}
type differentialReceipt struct {
	Source      differentialSource             `json:"source"`
	Legacy      differentialSnapshot           `json:"legacy"`
	Module      differentialSnapshot           `json:"module"`
	Versions    model.Versions                 `json:"versions"`
	Differences []string                       `json:"differences"`
	Converted   *convertedDifferentialEvidence `json:"converted,omitempty"`
}

// TestTranscriptDifferentialCapture requires both an explicit mode and an output
// directory. A regular suite never walks private provider roots. The manifest
// captures all Claude/Grok files, plus ceil(N/5) Codex rollouts ranked by digest.
func TestTranscriptDifferentialCapture(t *testing.T) {
	mode, dir := os.Getenv("NANO_DIFFERENTIAL_MODE"), os.Getenv("NANO_DIFFERENTIAL_DIR")
	if mode == "" {
		t.Skip("set NANO_DIFFERENTIAL_MODE and NANO_DIFFERENTIAL_DIR for the read-only corpus audit")
	}
	if dir == "" || !filepath.IsAbs(dir) {
		t.Fatal("an absolute NANO_DIFFERENTIAL_DIR is required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if mode == "manifest" {
		if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
			t.Fatal("manifest already exists or cannot be inspected; choose a new output directory")
		}
		manifest, err := freezeDifferentialManifest(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(manifestPath, append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("frozen %d sources; denominators %v", len(manifest.Sources), manifest.Counts)
		return
	}
	if mode != "cost" && mode != "converted" {
		t.Fatal("mode must be manifest, converted or cost; old capture is retained at commit 2bf54c8")
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest differentialManifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if mode == "cost" {
		measureDifferentialCorpusCheckpoints(t, dir, manifest)
		return
	}
	if mode == "converted" {
		captureConvertedDifferential(t, dir, manifest)
		return
	}
}

func freezeDifferentialManifest(ctx context.Context, dir string) (differentialManifest, error) {
	manifest := differentialManifest{Rule: "all Claude JSONL and Grok JSON/JSONL; Codex ceil(N/5) by SHA-256(root kind + slash-relative path)", Counts: map[Provider]int{}}
	roots, err := ResolveStandardRoots(RootOptions{})
	if err != nil {
		return manifest, err
	}
	var candidates []differentialSource
	for _, root := range roots {
		discovered, err := DiscoverRoot(ctx, root)
		if err != nil {
			return manifest, err
		}
		if discovered.Summary.Total != 0 {
			return manifest, fmt.Errorf("discovery diagnostics for %s: %+v", root.Kind, discovered.Summary)
		}
		known := map[string]bool{}
		for _, source := range discovered.Candidates {
			candidates = append(candidates, differentialSource{Source: source})
			known[source.Path] = true
		}
		if root.Provider != ProviderGrok {
			continue
		}
		err = filepath.WalkDir(root.Path, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || known[path] {
				return nil
			}
			ext := filepath.Ext(path)
			if ext != ".json" && ext != ".jsonl" {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root.Path, path)
			if err != nil {
				return err
			}
			device, inode := fileIdentity(info)
			candidates = append(candidates, differentialSource{Source: SourceCandidate{Root: root, Path: path, RelativePath: rel, Kind: classifyPath(root, rel), Size: info.Size(), ModTime: info.ModTime(), Device: device, Inode: inode}, Auxiliary: true})
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return manifest, err
		}
	}
	for _, candidate := range candidates {
		manifest.Counts[candidate.Source.Root.Provider]++
	}
	candidates = selectDifferentialSources(candidates)

	for _, candidate := range candidates {
		candidate.Snapshot = filepath.Join("sources", string(candidate.Source.Root.Kind), candidate.Source.RelativePath)
		target := filepath.Join(dir, candidate.Snapshot)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return manifest, err
		}
		digest, err := freezeDifferentialSource(candidate.Source, target)
		if err != nil {
			return manifest, err
		}
		candidate.SHA256 = digest
		manifest.Sources = append(manifest.Sources, candidate)
	}

	return manifest, nil
}
func selectDifferentialSources(candidates []differentialSource) []differentialSource {
	var codex, selected []differentialSource
	for _, candidate := range candidates {
		if candidate.Source.Root.Provider == ProviderCodex {
			codex = append(codex, candidate)
		} else {
			selected = append(selected, candidate)
		}
	}
	rank := func(source differentialSource) string {
		return differentialHash([]byte(string(source.Source.Root.Kind) + "/" + filepath.ToSlash(source.Source.RelativePath)))
	}
	sort.Slice(codex, func(i, j int) bool { return rank(codex[i]) < rank(codex[j]) })
	selected = append(selected, codex[:(len(codex)+4)/5]...)
	sort.Slice(selected, func(i, j int) bool { return selected[i].Source.Path < selected[j].Source.Path })
	return selected
}
func hashDifferentialPrefix(file *os.File, size int64) (string, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.NewSectionReader(file, 0, size))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("source shortened: got %d of %d bytes", n, size)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func verifyDifferentialSource(file *os.File, entry differentialSource) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	device, inode := fileIdentity(info)
	if device != entry.Source.Device || inode != entry.Source.Inode {
		return fmt.Errorf("source replaced after manifest freeze")
	}
	digest, err := hashDifferentialPrefix(file, entry.Source.Size)
	if err != nil {
		return err
	}
	if digest != entry.SHA256 {
		return fmt.Errorf("source prefix changed after manifest freeze")
	}
	return nil
}
func differentialHash(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }

func viewDifferentialSnapshot(result TranscriptProjectionResult) differentialSnapshot {
	snapshot := differentialSnapshot{CompleteOffset: result.CompleteOffset, CompleteLines: result.CompleteLines, CheckpointBytes: len(result.ParserState), Diagnostics: result.Diagnostics}
	bodies := map[string]string{}
	for _, body := range result.Bodies {
		bodies[body.ItemID] = body.Text
	}
	refs := map[string]int{}
	for index, item := range result.Items {
		text := item.Text
		if body, ok := bodies[item.ID]; ok {
			text = body
		}
		refs[item.ID] = index + 1
		snapshot.Items = append(snapshot.Items, differentialItem{Line: item.SourceLine, Emission: item.EmissionIndex, Row: item.SourceRowType, Payload: item.SourcePayloadType, SourceHash: item.SourceLineSHA256, NativeID: item.NativeItemID, Turn: item.TurnOrdinal, Type: string(item.Type), Role: string(item.Role), Status: string(item.Status), TextHash: differentialHash([]byte(text)), TextBytes: len(text), Tool: item.ToolName, Call: item.CallID, Detail: differentialJSON(item.Detail), CreatedAt: item.CreatedAt, Phase: item.Phase, Tags: item.FilterTags, Synthetic: item.Synthetic, Sidechain: item.Sidechain, Final: item.IsFinalAnswer})
	}
	for _, turn := range result.Turns {
		snapshot.Turns = append(snapshot.Turns, differentialTurn{Ordinal: turn.Ordinal, NativeID: turn.NativeTurnID, Start: turn.StartSeq, End: turn.EndSeq, Status: string(turn.Status), Completion: string(turn.CompletionEvidence), Final: refs[turn.FinalItemID], StartedAt: turn.StartedAt, CompletedAt: turn.CompletedAt, Duration: turn.DurationMS, Config: differentialJSON(turn.Config), Usage: differentialUsage(turn.Usage), Model: turn.Model, Effort: turn.Effort, Tier: turn.ServiceTier})
	}
	return snapshot
}
func moduleDifferentialSnapshot(reader io.Reader, entry differentialSource, measure bool) (differentialSnapshot, model.Versions, error) {
	var snapshot differentialSnapshot
	var versions model.Versions
	var cs parsecodex.State
	var as parseclaude.State
	var gs parsegrok.State
	var diagnostics []model.Diagnostic
	items := map[string]model.Item{}
	turns := map[string]model.Turn{}
	input := model.Input{Source: model.SourceRef{Label: "differential", Incarnation: "1"}, Scope: model.Scope{Provider: model.Provider(entry.Source.Root.Provider)}}
	if entry.Source.Root.Provider == ProviderGrok && filepath.Ext(entry.Source.Path) == ".json" {
		input.Format = "json"
	}
	buffer := make([]byte, 4<<20)
	for {
		n, readErr := reader.Read(buffer)
		if readErr != nil && readErr != io.EOF {
			return snapshot, versions, readErr
		}
		if n == 0 && readErr == nil {
			continue
		}
		input.Bytes = buffer[:n]
		input.Closed = readErr == io.EOF && input.Format == "json"
		var batch model.Batch
		var err error
		switch entry.Source.Root.Provider {
		case ProviderCodex:
			cs, batch, err = parsecodex.ParseFile(cs, input)
		case ProviderClaude:
			as, batch, err = parseclaude.ParseFile(as, input)
		case ProviderGrok:
			gs, batch, err = parsegrok.ParseFile(gs, input)
		default:
			return snapshot, versions, fmt.Errorf("unsupported provider")
		}
		if err != nil {
			return snapshot, versions, err
		}
		// Emitted rows belong to the consumer. A continuation checkpoint may
		// discard settled items and turns without removing them from this view.
		for _, change := range batch.Changes {
			if change.Item != nil {
				items[change.Item.ID] = *change.Item
			}
			if change.Turn != nil {
				turns[change.Turn.Key] = *change.Turn
			}
		}
		versions = batch.Versions
		diagnostics = append(diagnostics, batch.Diagnostics...)
		if readErr == io.EOF {
			break
		}
	}
	var checkpoint any
	switch entry.Source.Root.Provider {
	case ProviderCodex:
		checkpoint = cs
		snapshot.CompleteOffset = cs.Framing.Checkpoint
		snapshot.CompleteLines = int(cs.Framing.Line) - 1
	case ProviderClaude:
		checkpoint = as
		snapshot.CompleteOffset = as.Framing.Checkpoint
		snapshot.CompleteLines = int(as.Framing.Line) - 1
	case ProviderGrok:
		checkpoint = gs
		snapshot.CompleteOffset = gs.Framing.Checkpoint
		snapshot.CompleteLines = int(gs.Framing.Line) - 1
		if input.Format == "json" {
			snapshot.CompleteOffset = gs.DocumentBytes
			snapshot.CompleteLines = 0
		}
	}
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		return snapshot, versions, err
	}
	snapshot.CheckpointBytes = len(raw)
	if measure {
		cost, err := measureDifferentialCheckpoint(raw, input)
		if err != nil {
			return snapshot, versions, err
		}
		snapshot.CheckpointCost = &cost
	}
	snapshot.Diagnostics = diagnostics
	ordered := make([]model.Item, 0, len(items))
	for _, item := range items {
		ordered = append(ordered, item)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].SourceSeq < ordered[j].SourceSeq })
	refs := map[string]int{}
	for index, item := range ordered {
		refs[item.ID] = index + 1
		snapshot.Items = append(snapshot.Items, differentialItem{Line: item.SourceLine, Emission: item.EmissionIndex, Row: item.SourceRowType, Payload: item.SourcePayloadType, SourceHash: item.SourceLineSHA256, NativeID: item.NativeItemID, Turn: item.TurnOrdinal, Type: string(item.Type), Role: string(item.Role), Status: string(item.Status), TextHash: differentialHash([]byte(item.Text)), TextBytes: len(item.Text), Tool: item.ToolName, Call: item.CallID, Detail: differentialJSON(item.Detail), CreatedAt: item.CreatedAt, Phase: item.Phase, Tags: item.FilterTags, Synthetic: item.Synthetic, Sidechain: item.Sidechain, Final: item.IsFinalAnswer})
	}
	orderedTurns := make([]model.Turn, 0, len(turns))
	for _, turn := range turns {
		orderedTurns = append(orderedTurns, turn)
	}
	sort.Slice(orderedTurns, func(i, j int) bool { return orderedTurns[i].Ordinal < orderedTurns[j].Ordinal })
	for _, turn := range orderedTurns {
		var usage *TranscriptTokenUsage
		if turn.Usage != nil {
			value := TranscriptTokenUsage(*turn.Usage)
			usage = &value
		}
		snapshot.Turns = append(snapshot.Turns, differentialTurn{Ordinal: turn.Ordinal, NativeID: turn.NativeTurnID, Start: turn.StartSourceSeq, End: turn.EndSourceSeq, Status: string(turn.Status), Completion: string(turn.CompletionEvidence), Final: refs[turn.FinalItemID], StartedAt: turn.StartedAt, CompletedAt: turn.CompletedAt, Duration: turn.DurationMS, Config: differentialJSON(turn.Config), Usage: differentialUsage(usage), Model: turn.Model, Effort: turn.Effort, Tier: turn.ServiceTier})
	}
	return snapshot, versions, nil
}

func TestDifferentialSelectionIsDeterministic(t *testing.T) {
	var sources []differentialSource
	for i := 0; i < 11; i++ {
		sources = append(sources, differentialSource{Source: SourceCandidate{Root: SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions}, Path: fmt.Sprintf("%d", i), RelativePath: fmt.Sprintf("rollout-%d.jsonl", i)}})
	}
	sources = append(sources, differentialSource{Source: SourceCandidate{Root: SourceRoot{Provider: ProviderClaude}, Path: "claude"}}, differentialSource{Source: SourceCandidate{Root: SourceRoot{Provider: ProviderGrok}, Path: "grok"}})
	a := selectDifferentialSources(sources)
	for i, j := 0, len(sources)-1; i < j; i, j = i+1, j-1 {
		sources[i], sources[j] = sources[j], sources[i]
	}
	b := selectDifferentialSources(sources)
	if len(a) != 5 || !reflect.DeepEqual(a, b) {
		t.Fatalf("selection must contain three Codex and every Claude/Grok independent of discovery order")
	}
	counts := map[Provider]int{}
	for _, entry := range a {
		counts[entry.Source.Root.Provider]++
	}
	if counts[ProviderCodex] != 3 || counts[ProviderClaude] != 1 || counts[ProviderGrok] != 1 {
		t.Fatal(counts)
	}
}
func TestDifferentialFrozenPrefixRejectsRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.jsonl")
	if err := os.WriteFile(path, []byte("abc\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	device, inode := fileIdentity(info)
	entry := differentialSource{Source: SourceCandidate{Size: 4, Device: device, Inode: inode}, SHA256: differentialHash([]byte("abc\n"))}
	if err = verifyDifferentialSource(file, entry); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("abc\ndef\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = verifyDifferentialSource(file, entry); err != nil {
		t.Fatal("append must preserve frozen prefix", err)
	}
	if err = os.WriteFile(path, []byte("xyz\ndef\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = verifyDifferentialSource(file, entry); err == nil {
		t.Fatal("rewrite must invalidate frozen prefix")
	}
}

func differentialJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) == nil {
		raw, _ = json.Marshal(value)
	}
	encoded, _ := json.Marshal(map[string]string{"sha256": differentialHash(raw)})
	return encoded
}
func differentialUsage(usage *TranscriptTokenUsage) *TranscriptTokenUsage {
	if usage == nil {
		return nil
	}
	copy := *usage
	copy.Native = differentialJSON(copy.Native)
	return &copy
}
func verifyDifferentialReceipt(path string, source differentialSource) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	var receipt differentialReceipt
	if err = json.NewDecoder(gz).Decode(&receipt); err != nil {
		return err
	}
	if !reflect.DeepEqual(receipt.Source, source) {
		return fmt.Errorf("receipt does not match frozen source")
	}
	if _, err = io.Copy(io.Discard, gz); err != nil {
		return err
	}
	return nil
}

func freezeDifferentialSource(source SourceCandidate, target string) (string, error) {
	input, err := os.Open(source.Path)
	if err != nil {
		return "", err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return "", err
	}
	device, inode := fileIdentity(info)
	if device != source.Device || inode != source.Inode {
		return "", fmt.Errorf("source replaced during discovery")
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(output, h), io.NewSectionReader(input, 0, source.Size))
	closeErr := output.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if n != source.Size {
		return "", fmt.Errorf("source shortened during freeze")
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if err = verifyDifferentialSource(input, differentialSource{Source: source, SHA256: digest}); err != nil {
		return "", err
	}
	return digest, nil
}
func verifyDifferentialSnapshot(file *os.File, entry differentialSource) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != entry.Source.Size {
		return fmt.Errorf("snapshot size changed")
	}
	digest, err := hashDifferentialPrefix(file, entry.Source.Size)
	if err != nil {
		return err
	}
	if digest != entry.SHA256 {
		return fmt.Errorf("snapshot hash changed")
	}
	return nil
}

func TestDifferentialSnapshotSurvivesSourceGrowth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.jsonl")
	target := filepath.Join(dir, "snapshot.jsonl")
	if err := os.WriteFile(path, []byte("abc\n"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	device, inode := fileIdentity(info)
	source := SourceCandidate{Path: path, Size: 4, Device: device, Inode: inode}
	digest, err := freezeDifferentialSource(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("replacement\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err = verifyDifferentialSnapshot(file, differentialSource{Source: source, SHA256: digest}); err != nil {
		t.Fatal(err)
	}
	if _, err = freezeDifferentialSource(source, target); err == nil {
		t.Fatal("existing snapshot must not be overwritten")
	}
}

func TestDifferentialJSONPreservesLargeNativeCounters(t *testing.T) {
	a := differentialJSON(json.RawMessage(`{"count":9007199254740992,"x":true}`))
	b := differentialJSON(json.RawMessage(`{"x":true,"count":9007199254740993}`))
	if bytes.Equal(a, b) {
		t.Fatal("distinct large native counters collapsed")
	}
	c := differentialJSON(json.RawMessage(`{ "x": true, "count": 9007199254740992 }`))
	if !bytes.Equal(a, c) {
		t.Fatal("object formatting created a false difference")
	}
}

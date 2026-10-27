package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/transcriptparse/claude"
	"github.com/dkroshow/nano/internal/transcriptparse/codex"
	"github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// Fixture views use the production file/store and live conversion paths.
// The frozen old views live in testdata/transcript-compatibility.json.
type fixtureDifferentialView struct {
	Type, Role, Status, TextHash, NativeID, NativeTurnID, Tool, Call, Phase string
	TextBytes                                                               int
	CreatedAt                                                               string
	rawText                                                                 string
}

func fixtureNativeInput(provider, name string) bool {
	// Claude worker sidecars supply caller context; its reader accepts JSONL.
	return strings.HasSuffix(name, ".jsonl") || provider == "grok" && (name == "input.json" || name == "input.recipe.json" || name == "meta.json" || strings.HasSuffix(name, ".meta.json") || strings.HasSuffix(name, "-summary.json"))
}

func fixtureReaderMode(provider, caseName, name string, raw []byte) string {
	if name != "input.recipe.json" && strings.HasSuffix(name, ".json") {
		return "document"
	}
	if strings.Contains(name, "wire") || strings.Contains(caseName, "wire") || provider == "codex" && (strings.HasPrefix(caseName, "request-") || strings.HasPrefix(caseName, "edge-response-")) {
		return "wire"
	}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		var fields map[string]json.RawMessage
		if json.Unmarshal(line, &fields) == nil && (fields["method"] != nil || fields["jsonrpc"] != nil || fields["msg"] != nil || fields["dir"] != nil && fields["line"] != nil) {
			return "wire"
		}
	}
	return "file"
}

func fixtureDigest(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }

func fixtureTime(at *time.Time) string {
	if at == nil {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

// fixtureModuleView is a fixture's saved view: what the catalog serves for a
// file or document fixture. A wire fixture is only parsed, frame by frame as
// a live adapter reads it; no surface presents wire rows, so it has no view.
func fixtureModuleView(t *testing.T, provider, mode string, raw []byte) []fixtureDifferentialView {
	t.Helper()
	if mode != "wire" {
		return fixtureConvertedFileView(t, provider, mode, raw)
	}
	var cs codex.State
	var as claude.State
	var gs grok.State
	input := model.Input{Source: model.SourceRef{Label: "fixture", Incarnation: "1"}, Closed: true}
	for _, chunk := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(chunk)) == 0 {
			continue
		}
		input.Bytes = chunk
		var err error
		switch provider {
		case "codex":
			cs, _, err = codex.ParseWire(cs, input)
		case "claude":
			as, _, err = claude.ParseWire(as, input)
		case "grok":
			gs, _, err = grok.ParseWire(gs, input)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return nil
}

func TestLiveAdaptersReturnTheParserBatch(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {
		t.Run(provider, func(t *testing.T) {
			var adapter liveAdapter
			switch provider {
			case "codex":
				adapter = newCodexLiveAdapter()
			case "claude":
				adapter = newClaudeLiveAdapter()
			case "grok":
				adapter = newGrokLiveAdapter()
			}
			result := adapter.Apply(protocol.Recv, []byte(`{"not":"a semantic provider frame"}`), time.Unix(200, 0).UTC())
			if result.Versions.Schema == "" {
				t.Fatal("adapter discarded the normalized parser batch")
			}
		})
	}
}

func fixtureConvertedFileView(t *testing.T, provider, mode string, raw []byte) []fixtureDifferentialView {
	t.Helper()
	ctx, scratch := context.Background(), t.TempDir()
	root := history.SourceRoot{Provider: history.Provider(provider), Path: scratch}
	switch provider {
	case "codex":
		root.Kind = history.RootCodexSessions
	case "claude":
		root.Kind = history.RootClaudeProjects
	case "grok":
		root.Kind = history.RootGrokSessions
	}
	source := history.SourceCandidate{Root: root, Path: filepath.Join(scratch, "input.jsonl"), RelativePath: "input.jsonl", Kind: history.ThreadMain, Size: int64(len(raw)), ModTime: time.Unix(200, 0)}
	format := "jsonl"
	if mode == "document" {
		format = "json"
	}
	parsed, err := history.ProjectParsedFile(ctx, history.ParsedFileRequest{Source: source, Reader: bytes.NewReader(raw), Format: format, SpoolDir: scratch})
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	store, err := history.Open(filepath.Join(scratch, "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	metadata := parsed.Metadata
	if metadata.Projection.NativeThreadID == "" {
		metadata.Projection.NativeThreadID = "fixture"
		metadata.Projection.IdentityEvidence = history.EvidenceFilenameFallback
	}
	applied, err := store.ApplySource(ctx, history.SourceProjection{Source: source, Result: &metadata, ParserVersion: parsed.ParserVersion, Status: history.SourcePresent, IndexedAt: time.Unix(200, 0)})
	if err != nil {
		t.Fatal(err)
	}
	persistedRoot, err := store.EnsureRoot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	stored, exists, err := store.LoadSource(ctx, persistedRoot.ID, source.Path)
	if err != nil || !exists {
		t.Fatalf("source exists=%t err=%v", exists, err)
	}
	_, err = store.ApplyTranscript(ctx, history.TranscriptApplyRequest{Mode: history.TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation, Provider: root.Provider, ParserVersion: parsed.ParserVersion, CompletionRuleVersion: parsed.CompletionRuleVersion, Result: parsed.Transcript})
	if err != nil {
		t.Fatal(err)
	}
	var items []history.TranscriptItem
	turns := map[string]string{}
	var before int64
	for {
		page, err := store.ReadTranscript(ctx, history.TranscriptReadRequest{ThreadID: applied.ThreadID, BeforeSeq: before, Limit: history.MaxTranscriptPageLimit})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, page.Items...)
		for _, turn := range page.Turns {
			turns[turn.Key] = turn.NativeTurnID
		}
		if !page.HasMoreBefore {
			break
		}
		if page.StartSeq <= 0 || (before != 0 && page.StartSeq >= before) {
			t.Fatal("fixture pagination did not advance")
		}
		before = page.StartSeq
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ProjectedSeq < items[j].ProjectedSeq })
	view := []fixtureDifferentialView{}
	for _, item := range items {
		text := item.Text
		if item.ContentRef != "" {
			body, err := store.LoadTranscriptBody(ctx, applied.ThreadID, item.ID)
			if err != nil {
				t.Fatal(err)
			}
			text = body.Text
		}
		view = append(view, fixtureDifferentialView{string(item.Type), string(item.Role), string(item.Status), fixtureDigest([]byte(text)), item.NativeItemID, turns[item.TurnKey], item.ToolName, item.CallID, item.Phase, len(text), fixtureTime(item.CreatedAt), text})
	}
	return view
}

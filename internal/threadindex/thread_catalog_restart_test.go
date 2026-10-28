package threadindex

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// A scan commits a file's source row before its transcript rows. Nano stopping
// between the two, as a deploy restart did just after Codex saved a reply,
// left the row describing the file and the transcript behind it; every later
// scan, the restart's included, then saw an unchanged file. Whether the
// stopped scan was appending, indexing the file for the first time, reading
// a truncated file or one rewritten in place at the same size, the restart
// must serve the file as it is now, also when the file changed again between
// the scan's discovery and its read. Boundary: the attached file's catch-up
// after a restart.
func TestCatalogRestartServesWhatAScanStoppedBeforeSaving(t *testing.T) {
	ctx := context.Background()
	native := "40b7284b-c7dd-56b1-a8d1-f5f536dbe04c"
	data, err := os.ReadFile(filepath.Join("..", "app", "testdata", "duplicate_gate", "codex-agent-message-local-id.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	raw := string(data)
	const shortPrompt, shortAnswer = "is the build finished?", "Yes—it finished."
	type row struct{ role, itemType, text string }
	readRows := func(service *Service, id string) ([]row, error) {
		page, err := service.LoadTranscript(ctx, id, TranscriptQuery{View: DevThreadTranscriptView, HiddenSpecified: true, Limit: MaxThreadTranscriptLimit})
		if err != nil {
			return nil, err
		}
		rows := []row{}
		for _, item := range page.Items {
			if item.Category == "messages" || item.Category == "reasoning" {
				rows = append(rows, row{item.Role, item.Type, item.Text})
			}
		}
		return rows, nil
	}
	writeFile := func(path string, raw []byte) {
		t.Helper()
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.SplitAfter(raw, "\n")
	prompt := raw[:len(strings.Join(lines[:5], ""))] // the prompt is saved; the reply's lines follow
	both := []row{{"user", "user_message", shortPrompt}, {"assistant", "agent_message", shortAnswer}}
	revised := strings.ReplaceAll(raw, shortAnswer, "Nah—it finished.") // the same byte length
	revisedRows := []row{{"user", "user_message", shortPrompt}, {"assistant", "agent_message", "Nah—it finished."}}
	partial := `{"timestamp":` // an unfinished line holds no row
	for _, tc := range []struct {
		name string
		// What the last finished scan saved, what the stopped scan discovered
		// (when the file changed before it was read) and the file it read.
		indexed, discovered, file string
		want                      []row
	}{
		{"append", prompt, "", raw, both},
		{"first index", "", "", raw, both},
		{"truncation", raw, "", prompt, both[:1]},
		{"same-size rewrite", raw, "", revised, revisedRows},
		{"growth discovered, then a rewrite read", raw, raw + partial, revised, revisedRows},
		{"rewrite discovered, then growth read", raw, revised, revised + partial, revisedRows},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}
			store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			service, err := newTestCatalogService(ctx, store, Options{Roots: []history.SourceRoot{spec}, DisableWatcher: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(service.Close)
			path := filepath.Join(spec.Path, "rollout-"+native+".jsonl")
			if tc.indexed != "" {
				writeFile(path, []byte(tc.indexed))
				service.FileChanged(path)
				waitForCatalogWithin(t, time.Minute, func() bool {
					id, err := service.ResolveThreadReference(ctx, native)
					if err != nil {
						return false
					}
					rows, err := readRows(service, id)
					return err == nil && len(rows) >= 1
				})
			}
			service.Close()
			// The stopped scan: it read the file and committed the source row only.
			// A rewrite of equal size differs only by its time.
			write := func(content string, offset time.Duration) {
				writeFile(path, []byte(content))
				at := time.Now().Add(offset)
				if err := os.Chtimes(path, at, at); err != nil {
					t.Fatal(err)
				}
			}
			discover := func() history.SourceCandidate {
				discovery, err := history.DiscoverRoot(ctx, spec)
				if err != nil || len(discovery.Candidates) != 1 {
					t.Fatal("discover the rollout", err)
				}
				return discovery.Candidates[0]
			}
			var candidate history.SourceCandidate
			if tc.discovered != "" {
				write(tc.discovered, 2*time.Second)
				candidate = discover()
			}
			write(tc.file, 4*time.Second)
			if tc.discovered == "" {
				candidate = discover()
			}
			root, err := store.EnsureRoot(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			stored, exists, err := store.LoadSource(ctx, root.ID, path)
			if err != nil {
				t.Fatal(err)
			}
			var header history.TranscriptCheckpoint
			var hasHeader bool
			if exists {
				if header, hasHeader, err = store.LoadTranscriptCheckpointHeader(ctx, stored.ThreadID); err != nil {
					t.Fatal(err)
				}
			}
			input, prepared, err := service.projectSource(ctx, candidate, stored, exists, header, hasHeader, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			prepared.File.Close()
			if _, err := store.ReconcileRoot(ctx, history.RootReconciliation{RootID: root.ID, ObservedPaths: []string{path}, Sources: []history.SourceProjection{input}, State: history.IndexReady, Partial: true, CompletedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}

			// The restart walks nothing; the attachment's catch-up of its own
			// file must serve the file as it is now.
			restarted, err := newTestCatalogService(ctx, store, Options{Roots: []history.SourceRoot{spec}, DisableWatcher: true})
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			restarted.holdAttachedSource("restarted", []string{path})
			catchUp := restarted.attachedCatchUpFor("restarted")
			select {
			case <-catchUp.done:
			case <-time.After(time.Minute):
				t.Fatal("restarted catalog did not catch up the attached file")
			}
			if result := restarted.catchUpResultOf(catchUp); result != catchUpReady {
				t.Fatalf("catch-up result = %v, want ready", result)
			}
			id, err := restarted.ResolveThreadReference(ctx, native)
			if err != nil {
				t.Fatal(err)
			}
			served, err := readRows(restarted, id)
			if err != nil || !slices.Equal(served, tc.want) {
				t.Fatalf("transcript after restart=%+v, %v; want %+v", served, err, tc.want)
			}
		})
	}
}

package threadindex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// gateRow is what a reader sees for one served message or reasoning row.
type gateRow struct {
	Role string
	Type string
	Text string
}

func (r gateRow) String() string {
	text := r.Text
	if len(text) > 60 {
		text = text[:60] + "…"
	}
	return fmt.Sprintf("%s/%s %q", r.Role, r.Type, text)
}

// gateProblems compares served rows with the expected rows, in order. It
// reports each missing row, each extra copy, and any order change, so that
// deleting both copies of a message fails as loudly as serving it twice.
func gateProblems(expected, served []gateRow) []string {
	want, got := map[gateRow]int{}, map[gateRow]int{}
	for _, row := range expected {
		want[row]++
	}
	for _, row := range served {
		got[row]++
	}
	var problems []string
	for _, row := range expected {
		if want[row] > got[row] {
			problems = append(problems, fmt.Sprintf("missing: %s (want %d, served %d)", row, want[row], got[row]))
			got[row] = want[row] // report each row once
		}
	}
	for _, row := range served {
		if got[row] > want[row] {
			problems = append(problems, fmt.Sprintf("repeated: %s (want %d, served %d)", row, want[row], got[row]))
			want[row] = got[row]
		}
	}
	if len(problems) == 0 && len(expected) == len(served) {
		for i := range expected {
			if expected[i] != served[i] {
				problems = append(problems, fmt.Sprintf("order: position %d served %s, want %s", i, served[i], expected[i]))
				break
			}
		}
	}
	return problems
}

type gateCatalog struct {
	service *Service
	handler *Handlers
	roots   map[history.Provider]string
}

// newGateCatalog starts the real thread catalog over the named fixtures.
// Codex fixtures are rollout files; Claude fixtures are project session files.
// extraCodex holds generated rollouts, written before the catalog starts.
func newGateCatalog(t *testing.T, codexFixtures, claudeFixtures []string, extraCodex ...string) *gateCatalog {
	t.Helper()
	codexRoot, claudeRoot := t.TempDir(), t.TempDir()
	codexRaw := make([][]byte, 0, len(codexFixtures)+len(extraCodex))
	for _, name := range codexFixtures {
		codexRaw = append(codexRaw, readGateFixture(t, name))
	}
	for _, raw := range extraCodex {
		codexRaw = append(codexRaw, []byte(raw))
	}
	for _, raw := range codexRaw {
		writeGateFile(t, filepath.Join(codexRoot, "rollout-"+gateFixtureNativeID(t, raw)+".jsonl"), raw)
	}
	project := filepath.Join(claudeRoot, "-fixture-project")
	for _, name := range claudeFixtures {
		raw := readGateFixture(t, name)
		writeGateFile(t, filepath.Join(project, gateFixtureNativeID(t, raw)+".jsonl"), raw)
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	service, err := newTestCatalogService(context.Background(), store, Options{
		Roots: []history.SourceRoot{
			{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: codexRoot},
			{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: claudeRoot},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	select {
	case <-service.catalogReady:
	case <-time.After(time.Minute):
		t.Fatal("catalog did not start")
	}
	service.RequestRefresh() // the explicit discovery a new store needs
	return &gateCatalog{service: service, handler: NewHandlers(service),
		roots: map[history.Provider]string{history.ProviderCodex: codexRoot, history.ProviderClaude: project}}
}

func readGateFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "app", "testdata", "duplicate_gate", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeGateFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// gateFixtureNativeID reads the provider thread id from the first row that
// names one: Codex session_meta, or a Claude row's sessionId.
func gateFixtureNativeID(t *testing.T, raw []byte) string {
	t.Helper()
	for _, line := range strings.Split(string(raw), "\n") {
		var row struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionId"`
			Payload   struct {
				ID string `json:"id"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		if row.Type == "session_meta" && row.Payload.ID != "" {
			return row.Payload.ID
		}
		if row.SessionID != "" {
			return row.SessionID
		}
	}
	t.Fatal("fixture names no native thread id")
	return ""
}

// threadID waits until the catalog serves the thread with at least minRows
// message or reasoning rows.
func (c *gateCatalog) threadID(t *testing.T, nativeID string, minRows int) string {
	t.Helper()
	var id string
	waitForCatalogWithin(t, time.Minute, func() bool {
		resolved, err := c.service.ResolveThreadReference(context.Background(), nativeID)
		if err != nil {
			return false
		}
		id = resolved
		return len(c.servedTranscript(t, id)) >= minRows
	})
	return id
}

// servedTranscript reads GET /api/v1/threads/{id}/transcript with nothing
// hidden, following every older page, and keeps message and reasoning rows.
func (c *gateCatalog) servedTranscript(t *testing.T, threadID string) []gateRow {
	t.Helper()
	var rows []gateRow
	before := ""
	for page := 0; page < 20; page++ {
		target := "/api/v1/threads/" + url.PathEscape(threadID) + "/transcript?view=dev&hide=none&limit=200"
		if before != "" {
			target += "&before=" + url.QueryEscape(before)
		}
		recorder := httptest.NewRecorder()
		c.handler.HandleThreadTranscript(recorder, httptest.NewRequest(http.MethodGet, target, nil), threadID)
		if recorder.Code != http.StatusOK {
			return nil
		}
		var response TranscriptResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		rows = append(gateTranscriptRows(response.Items), rows...)
		if !response.Page.HasMoreBefore || response.Page.NextBeforeCursor == nil {
			return rows
		}
		before = *response.Page.NextBeforeCursor
	}
	t.Fatal("transcript paging did not end")
	return nil
}

func gateTranscriptRows(items []TranscriptItem) []gateRow {
	rows := make([]gateRow, 0, len(items))
	for _, item := range items {
		if item.Category == "messages" || item.Category == "reasoning" {
			rows = append(rows, gateRow{Role: item.Role, Type: item.Type, Text: item.Text})
		}
	}
	return rows
}

func assertGateRows(t *testing.T, surface string, expected, served []gateRow) {
	t.Helper()
	if problems := gateProblems(expected, served); len(problems) != 0 {
		t.Errorf("%s: %s\nserved:\n  %s", surface, strings.Join(problems, "; "), gateRowList(served))
	}
}

func gateRowList(rows []gateRow) string {
	lines := make([]string, len(rows))
	for i, row := range rows {
		lines[i] = row.String()
	}
	return strings.Join(lines, "\n  ")
}

func gateUser(text string) gateRow { return gateRow{"user", "user_message", text} }

func gateAgent(text string) gateRow { return gateRow{"assistant", "agent_message", text} }

const gateShortPrompt = "is the build finished?"
const gateShortAnswer = "Yes—it finished."

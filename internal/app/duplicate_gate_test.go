package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

// Duplicate-messages gate.
//
// Every check here reads served output: the thread transcript and messages
// HTTP responses for saved history. A check fails when one real message is
// served twice, when a row is missing, or when order changes.
// Rows with equal text but different provider identity must stay separate.
//
// The Codex fixtures in testdata/duplicate_gate are cut from real rollouts;
// testdata/duplicate_gate/README.md records each source line and its hash.

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

// TestDuplicateGateCheckerReportsMissingAndRepeatedRows proves the checker
// itself. Boundary: the comparison every served-output gate below uses. It
// catches a gate that passes when both copies vanish or when equal text is
// collapsed.
func TestDuplicateGateCheckerReportsMissingAndRepeatedRows(t *testing.T) {
	user := gateRow{"user", "user_message", "ship it?"}
	done := gateRow{"assistant", "agent_message", "Done."}
	expected := []gateRow{user, done, done}
	cases := []struct {
		name   string
		served []gateRow
		want   string
	}{
		{"both copies deleted", []gateRow{user}, "missing"},
		{"one equal row collapsed", []gateRow{user, done}, "missing"},
		{"served three times", []gateRow{user, done, done, done}, "repeated"},
		{"reordered", []gateRow{done, user, done}, "order"},
	}
	for _, tc := range cases {
		problems := gateProblems(expected, tc.served)
		if len(problems) == 0 || !strings.HasPrefix(problems[0], tc.want) {
			t.Fatalf("%s: problems = %v, want a %q report", tc.name, problems, tc.want)
		}
	}
	if problems := gateProblems(expected, []gateRow{user, done, done}); len(problems) != 0 {
		t.Fatalf("equal messages with separate identity were rejected: %v", problems)
	}
}

type gateCatalog struct {
	store   *history.Store
	service *sidebar.CatalogSource
	server  *server
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
	service, err := newThreadCatalogService(context.Background(), store, threadindex.Options{
		Roots: []history.SourceRoot{
			{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: codexRoot},
			{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: claudeRoot},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })

	service.RequestRefresh() // the explicit discovery a new store needs
	return &gateCatalog{store: store, service: service, server: &server{catalog: service},
		roots: map[history.Provider]string{history.ProviderCodex: codexRoot, history.ProviderClaude: project}}
}

func readGateFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "duplicate_gate", name))
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
		recorder := messagesRequest(t, c.server, target)
		if recorder.Code != http.StatusOK {
			return nil
		}
		var response threadindex.TranscriptResponse
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

func gateTranscriptRows(items []threadindex.TranscriptItem) []gateRow {
	rows := make([]gateRow, 0, len(items))
	for _, item := range items {
		if item.Category == "messages" || item.Category == "reasoning" {
			rows = append(rows, gateRow{Role: item.Role, Type: item.Type, Text: item.Text})
		}
	}
	return rows
}

// servedMessages reads GET /api/v1/threads/{id}/messages?format=json.
func (c *gateCatalog) servedMessages(t *testing.T, threadID string) []gateRow {
	t.Helper()
	recorder := messagesRequest(t, c.server, "/api/v1/threads/"+url.PathEscape(threadID)+"/messages?format=json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("messages status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response threadMessagesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	rows := make([]gateRow, 0, len(response.Messages))
	for _, message := range response.Messages {
		rows = append(rows, gateRow{Role: message.Role, Text: message.Text})
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

func gateUser(text string) gateRow      { return gateRow{"user", "user_message", text} }
func gateAgent(text string) gateRow     { return gateRow{"assistant", "agent_message", text} }
func gateReasoning(text string) gateRow { return gateRow{"assistant", "reasoning", text} }

func gateMessageRows(rows []gateRow) []gateRow {
	var messages []gateRow
	for _, row := range rows {
		switch row.Type {
		case "user_message":
			messages = append(messages, gateRow{Role: threadindex.MessageRoleUser, Text: row.Text})
		case "agent_message":
			messages = append(messages, gateRow{Role: threadindex.MessageRoleAgent, Text: row.Text})
		}
	}
	return messages
}

// gateFixtureText returns the full text of the first fixture row of the given
// payload type whose text starts with prefix, so expected rows can name long
// real messages by their opening words.
func gateFixtureText(t *testing.T, name, payloadType, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(string(readGateFixture(t, name)), "\n") {
		var row struct {
			Payload struct {
				Type    string `json:"type"`
				Message string `json:"message"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &row) != nil || row.Payload.Type != payloadType {
			continue
		}
		text := row.Payload.Message
		if len(row.Payload.Content) > 0 {
			text = row.Payload.Content[0].Text
		}
		if strings.HasPrefix(text, prefix) {
			return text
		}
	}
	t.Fatalf("%s has no %s starting %q", name, payloadType, prefix)
	return ""
}

// TestDuplicateGateEqualMessagesStaySeparate is the positive control for
// every duplicate fix. Boundary: saved history as the transcript and messages
// endpoints serve it. It catches a fix that merges messages because their text
// is equal: Codex said "T–Z has no failure output after one minute." twice in
// one turn, 22 minutes apart, and Claude said "Running the tick." on two loop
// ticks. Each must stay two rows. Green on main 0cf5b1b.
func TestDuplicateGateEqualMessagesStaySeparate(t *testing.T) {
	catalog := newGateCatalog(t, []string{"codex-equal-replies-distinct.jsonl"}, []string{"claude-repeated-tick.jsonl"})
	t.Run("codex", func(t *testing.T) {
		id := catalog.threadID(t, "5bbbb199-a4bf-5277-a168-8a1dab42d235", 3)
		prompt := gateFixtureText(t, "codex-equal-replies-distinct.jsonl", "message", "Check the sample build")
		repeated := "T–Z has no failure output after one minute."
		expected := []gateRow{gateUser(prompt), gateReasoning(""), gateAgent(repeated), gateReasoning(""), gateAgent(repeated)}
		assertGateRows(t, "transcript", expected, catalog.servedTranscript(t, id))
		assertGateRows(t, "messages", gateMessageRows(expected), catalog.servedMessages(t, id))
	})
	t.Run("claude", func(t *testing.T) {
		id := catalog.threadID(t, "dcee0889-2586-5d7b-a5dd-62b2866262ea", 2)
		var ticks int
		for _, row := range catalog.servedMessages(t, id) {
			if row.Role == threadindex.MessageRoleAgent && row.Text == "Running the tick." {
				ticks++
			}
		}
		if ticks != 2 {
			t.Fatalf("Claude served %d \"Running the tick.\" messages, want 2:\n  %s", ticks, gateRowList(catalog.servedMessages(t, id)))
		}
	})
}

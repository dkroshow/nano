package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// measuredRows is a capture from a served Nano page: forty real Enhanced rows
// of one thread, each with the lines it rendered at (its box height over the
// message line height) and the characters its body had room for. Paths and
// ids are replaced by same-length placeholders, which keeps the wrapping.
type measuredRows struct {
	Rows []struct {
		Kind  string  `json:"kind"`
		Cols  int     `json:"cols"`
		Lines float64 `json:"lines"`
		Text  string  `json:"text"`
	} `json:"rows"`
}

func loadMeasuredRows(t *testing.T) measuredRows {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "warm_fill", "measured_rows.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows measuredRows
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) < 30 {
		t.Fatalf("fixture holds %d rows", len(rows.Rows))
	}
	return rows
}

func (m measuredRows) item(i int) transcriptItem {
	role := "assistant"
	if m.Rows[i].Kind == "user" {
		role = "user"
	}
	return transcriptItem{ID: m.Rows[i].Text[:8], Role: role, Text: m.Rows[i].Text}
}

// An estimate above the rendered height would send too few rows and leave a
// gap under them; one far below would send many rows for nothing. Every real
// row estimates between three quarters of its height and its height.
func TestWarmFillEstimateStaysUnderMeasuredHeight(t *testing.T) {
	rows := loadMeasuredRows(t)
	for i, row := range rows.Rows {
		estimate := estimateRowLines(rows.item(i), row.Cols)
		if estimate > row.Lines+0.01 {
			t.Errorf("row %d (%s, %d chars): estimated %.2f lines, rendered %.2f", i, row.Kind, len(row.Text), estimate, row.Lines)
		}
		if estimate < 0.75*row.Lines {
			t.Errorf("row %d (%s, %d chars): estimated %.2f lines, rendered %.2f: far too low", i, row.Kind, len(row.Text), estimate, row.Lines)
		}
	}
}

// The count is the newest rows whose rendered heights reach the window: never
// fewer (a gap), at most one more than needed. One row taller than the window
// fills it alone; a window taller than every row takes them all.
func TestWarmFillCountFillsTheWindow(t *testing.T) {
	rows := loadMeasuredRows(t)
	items := make([]transcriptItem, len(rows.Rows))
	for i := range rows.Rows {
		items[i] = rows.item(i)
	}
	for _, lines := range []int{10, 30, 60, 98} {
		fill := warmFill{lines: lines, cols: rows.Rows[0].Cols}
		got := fill.count(items)
		rendered := 0.0
		need := len(items)
		for i := len(rows.Rows) - 1; i >= 0; i-- {
			rendered += rows.Rows[i].Lines
			if rendered >= float64(lines) {
				need = len(rows.Rows) - i
				break
			}
		}
		if got < need || got > need+1 {
			t.Errorf("window of %d lines: sent %d rows, the rendered heights need %d", lines, got, need)
		}
	}
	tall := []transcriptItem{{Role: "user", Text: rows.Rows[0].Text}} // 27 rendered lines
	if got := (warmFill{lines: 20, cols: 118}).count(append([]transcriptItem{items[1], items[2]}, tall...)); got != 1 {
		t.Fatalf("one row taller than the window: sent %d rows, want 1", got)
	}
	if got := (warmFill{lines: 5000, cols: 118}).count(items); got != len(items) {
		t.Fatalf("a window taller than every row: sent %d rows, want all %d", got, len(items))
	}
	if got := (warmFill{lines: 30, cols: 118}).count(nil); got != 0 {
		t.Fatalf("no rows: count %d", got)
	}
}

func TestParseWarmFill(t *testing.T) {
	if fill, err := parseWarmFill("", ""); err != nil || fill != (warmFill{lines: warmFillLinesDefault, cols: warmFillColsDefault}) {
		t.Fatalf("absent = %+v %v, want the default window", fill, err)
	}
	if fill, err := parseWarmFill("45", "120"); err != nil || fill != (warmFill{lines: 45, cols: 120}) {
		t.Fatalf("45x120 = %+v %v", fill, err)
	}
	for _, bad := range [][2]string{{"45", ""}, {"", "120"}, {"x", "120"}, {"4", "120"}, {"45", "5000"}, {"-1", "120"}} {
		if _, err := parseWarmFill(bad[0], bad[1]); err == nil {
			t.Errorf("lines=%q cols=%q accepted", bad[0], bad[1])
		}
	}
}

// A markdown renderer folds newlines inside a paragraph into spaces. Counting
// each source line as a rendered line made a row of a hundred one-character
// lines look a hundred lines tall, so a tall window got that one small row
// and a gap under it. Block starts still take their own lines.
func TestWarmFillEstimateFoldsSoftNewlines(t *testing.T) {
	prose := transcriptItem{Role: "assistant", Text: strings.Repeat("x\n", 99) + "x"}
	if got := estimateRowLines(prose, 118); got > warmFillAgentChrome+2 {
		t.Fatalf("a hundred one-character lines estimated %.2f lines, want one two-line paragraph plus chrome", got)
	}
	list := transcriptItem{Role: "assistant", Text: "- one\n- two\n- three\n\n| a | b |\n|---|---|\n| 1 | 2 |"}
	if got := estimateRowLines(list, 118); got < warmFillAgentChrome+6 {
		t.Fatalf("three list items, a gap and three table rows estimated %.2f lines, want each on its own line", got)
	}
	typed := transcriptItem{Role: "user", Text: strings.Repeat("x\n", 9) + "x"}
	if got := estimateRowLines(typed, 118); got != warmFillUserChrome+10 {
		t.Fatalf("a user message shows its ten source lines as typed: estimated %.2f", got)
	}
}

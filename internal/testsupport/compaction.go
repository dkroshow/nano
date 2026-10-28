package testsupport

import (
	"embed"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

//go:embed testdata/compaction/*.jsonl
var compactionFiles embed.FS

// TimedLine is one captured provider frame and its receive time.
type TimedLine struct {
	At   time.Time
	Line []byte
}

// CompactionFixture reads scrubbed frames captured from a real compaction.
func CompactionFixture(t *testing.T, provider string) []TimedLine {
	t.Helper()
	raw, err := compactionFiles.ReadFile("testdata/compaction/" + provider + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var lines []TimedLine
	for _, text := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var entry struct {
			TS   time.Time       `json:"ts"`
			Line json.RawMessage `json:"line"`
		}
		if err := json.Unmarshal([]byte(text), &entry); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, TimedLine{At: entry.TS, Line: entry.Line})
	}
	return lines
}

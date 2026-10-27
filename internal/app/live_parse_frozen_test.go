package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func TestFrozenClaudeSecondBlockThroughConvertedFile(t *testing.T) {
	path := os.Getenv("NANO_CLAUDE_BLOCK_REPLAY_FIXTURE")
	if path == "" {
		t.Skip("set NANO_CLAUDE_BLOCK_REPLAY_FIXTURE to frozen receipt 00093-5cdfcac18395")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != "5cdfcac183952c19f54d7b5102ea0191c5d4a5e31324c6d4ba1de2b4db5445a9" {
		t.Fatal("frozen second-block source changed")
	}
	lines := bytes.Split(raw, []byte{'\n'})
	var calls [2]string
	var message string
	for i, number := range []int{25, 26} {
		var row struct {
			Message struct {
				ID      string
				Content []struct{ ID string }
			}
		}
		if json.Unmarshal(lines[number-1], &row) != nil || len(row.Message.Content) != 1 {
			t.Fatal("target native block missing")
		}
		calls[i], message = row.Message.Content[0].ID, row.Message.ID
	}
	view := fixtureConvertedFileView(t, "claude", "file", raw)
	counts := [2]int{}
	for _, item := range view {
		if item.Type != "command" {
			continue
		}
		for i, call := range calls {
			if item.Call == call {
				counts[i]++
				if item.NativeID != fmt.Sprintf("%s:%d", message, i) {
					t.Fatal("replay replaced the stored native block identity")
				}
			}
		}
	}
	if counts != [2]int{1, 1} {
		t.Fatalf("native command blocks duplicated or lost: %v", counts)
	}
	t.Logf("converted_rows=%d target_blocks=2 one_row_per_block=true second_block_identity_preserved=true", len(view))
}

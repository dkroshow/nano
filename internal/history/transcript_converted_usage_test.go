package history

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConvertedCodexLateExecutionKeepsTurnContext(t *testing.T) {
	// A late execution result after another turn starts recreates the first
	// turn in bounded reader state. Its missing context is not a retraction.
	raw := `{"type":"session_meta","payload":{"id":"thread"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"first"}}
{"type":"turn_context","payload":{"turn_id":"first","model":"example","effort":"high","service_tier":"fast"}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call","arguments":"{\"cmd\":\"echo ok\"}"}}
{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"output_tokens":2}}}}
{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"first"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"second"}}
{"type":"turn_context","payload":{"turn_id":"second","model":"other","effort":"low"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Next"}]}}
{"type":"event_msg","payload":{"type":"exec_command_end","turn_id":"first","call_id":"call","command":["echo","ok"],"aggregated_output":"ok","exit_code":0}}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "input.jsonl")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	entry := differentialSource{Source: SourceCandidate{Root: SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: dir}, Path: path, RelativePath: "input.jsonl", Kind: ThreadMain, Size: int64(len(raw)), ModTime: time.Unix(200, 0)}}
	result, _, _, err := convertedDifferentialSnapshot(context.Background(), file, entry, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 2 {
		t.Fatalf("turn count=%d", len(result.Turns))
	}
	first, second := result.Turns[0], result.Turns[1]
	if first.NativeID != "first" || first.Model != "example" || first.Effort != "high" || first.Tier != "fast" || first.Status != "interrupted" || first.Usage == nil || first.Usage.Input == nil || *first.Usage.Input != 10 {
		t.Fatalf("late execution lost first-turn fields: %+v", first)
	}
	if second.NativeID != "second" || second.Model != "other" || second.Effort != "low" || second.Tier != "" || second.Usage != nil {
		t.Fatalf("first-turn context leaked into second turn: %+v", second)
	}
	var commands []differentialItem
	for _, item := range result.Items {
		if item.Type == "command" && item.Call == "call" {
			commands = append(commands, item)
		}
	}
	if len(commands) != 2 || commands[0].Tool != "exec_command" || commands[0].Status != "unknown" || commands[1].Tool != "" || commands[1].Status != "completed" || commands[0].Turn != commands[1].Turn {
		t.Fatalf("late execution replaced its generation facet: %+v", commands)
	}
}

func TestConvertedFilePreservesTurnUsageDisplayFields(t *testing.T) {
	for _, tc := range []struct {
		provider Provider
		root     RootKind
		raw      string
		duration int64
	}{
		{ProviderClaude, RootClaudeProjects, `{"type":"user","sessionId":"thread","timestamp":"2026-01-01T00:00:00Z","message":{"content":"Prompt"}}
{"type":"assistant","sessionId":"thread","timestamp":"2026-01-01T00:00:02Z","effort":"high","message":{"id":"message","model":"example","content":[{"type":"text","text":"Answer"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}}
{"type":"result","sessionId":"thread","timestamp":"2026-01-01T00:00:03Z","subtype":"success","is_error":false}
{"type":"system","subtype":"turn_duration","sessionId":"thread","timestamp":"2026-01-01T00:00:04Z","durationMs":2500}
`, 2500},
		{ProviderCodex, RootCodexSessions, `{"type":"session_meta","payload":{"id":"thread"}}
{"type":"event_msg","timestamp":"2026-01-01T00:00:00Z","payload":{"type":"task_started","turn_id":"turn"}}
{"type":"turn_context","payload":{"turn_id":"turn","model":"example","effort":"high"}}
{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Prompt"}]}}
{"type":"response_item","timestamp":"2026-01-01T00:00:02Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Answer"}]}}
{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"output_tokens":2}}}}
{"type":"event_msg","timestamp":"2026-01-01T00:00:03Z","payload":{"type":"task_complete","turn_id":"turn","duration_ms":2750}}
`, 2750},
	} {
		t.Run(string(tc.provider), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "input.jsonl")
			if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			entry := differentialSource{Source: SourceCandidate{Root: SourceRoot{Provider: tc.provider, Kind: tc.root, Path: dir}, Path: path, RelativePath: "input.jsonl", Kind: ThreadMain, Size: int64(len(tc.raw)), ModTime: time.Unix(200, 0)}}
			result, _, _, err := convertedDifferentialSnapshot(context.Background(), file, entry, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Turns) != 1 {
				t.Fatalf("turn count=%d", len(result.Turns))
			}
			turn := result.Turns[0]
			if turn.StartedAt == nil || turn.StartedAt.Format(time.RFC3339) != "2026-01-01T00:00:00Z" || turn.CompletedAt == nil || turn.CompletedAt.Format(time.RFC3339) != "2026-01-01T00:00:03Z" || turn.Duration != tc.duration {
				t.Errorf("turn display clocks/duration lost: start=%v end=%v duration=%d", turn.StartedAt, turn.CompletedAt, turn.Duration)
			}
			if turn.Model != "example" || turn.Effort != "high" || turn.Usage == nil || turn.Usage.Input == nil || *turn.Usage.Input != 10 || turn.Usage.Output == nil || *turn.Usage.Output != 2 {
				t.Errorf("stored usage fields lost: model=%q effort=%q usage=%+v", turn.Model, turn.Effort, turn.Usage)
			}
		})
	}
}

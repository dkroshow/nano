package sessions

import "testing"

func TestClaudeProjectionTerminalEvidence(t *testing.T) {
	status, detail := ClaudeProjectionTerminal([]byte(`{"type":"result","is_error":true,"subtype":"error_during_execution","result":"tool failed","terminal_reason":"error"}`))
	if status != agentTurnFailed || len(detail) == 0 {
		t.Fatalf("Claude error = %q, %s", status, detail)
	}
	status, _ = ClaudeProjectionTerminal([]byte(`{"type":"result","is_error":true,"terminal_reason":"aborted_by_user"}`))
	if status != agentTurnInterrupted {
		t.Fatalf("Claude abort = %q", status)
	}
	status, detail = ClaudeProjectionTerminal([]byte(`{"type":"result","is_error":false,"result":"done"}`))
	if status != agentTurnCompleted || len(detail) != 0 {
		t.Fatalf("Claude success = %q, %s", status, detail)
	}
}

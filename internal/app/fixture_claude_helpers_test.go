package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	fixture "github.com/dkroshow/nano/internal/testsupport/claudeshim"
)

var dataLines = fixture.DataLines
var waitForClaude = fixture.WaitFor
var waitForClaudeWithin = fixture.WaitForWithin

func claudeDataWireEvents(sess *session) []wireEvent {
	return fixture.DataEvents(claudeWireEvents(sess))
}

// emitClaudeInit waits for a following wire frame so the init's name publication has finished.
func emitClaudeInit(t *testing.T, sess *session, conn *shimConn, seq int64, native, model string) {
	t.Helper()
	line, _ := json.Marshal(map[string]any{"type": "system", "subtype": "init", "session_id": native, "model": model})
	conn.Line(seq, "recv", string(line))
	marker := fmt.Sprintf("after-init-%d", seq)
	barrier, _ := json.Marshal(map[string]any{"type": "system", "subtype": "status", "status": "idle", "uuid": marker})
	conn.Line(seq+1, "recv", string(barrier))
	waitForClaude(t, "init publication before the next wire frame", func() bool {
		for _, event := range claudeWireEvents(sess) {
			if bytes.Contains(event.Msg, []byte(marker)) {
				return true
			}
		}
		return false
	})
}

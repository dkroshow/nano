package sessions

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// Only the stamped block differs from the existing ACP request. The real
// SendTurn path still accepts the provider's native prompt id normally.
func TestGrokSubmissionStampsOnlyTextBlock(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "peer.py")
	if err := os.WriteFile(script, []byte(`import sys,json
for line in sys.stdin:
 m=json.loads(line)
 print(json.dumps({"jsonrpc":"2.0","id":m["id"],"result":{"stopReason":"end_turn","_meta":{"promptId":"native-prompt"}}}),flush=True)
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := protocol.NewClientCmd(exec.Command("python3", script))
	sent := make(chan []byte, 1)
	c.WireLog = func(d protocol.Direction, line []byte) {
		if d == protocol.Send {
			sent <- append([]byte(nil), line...)
		}
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sess := &Session{hostedThreadID: "hosted", threadID: grokFixtureThread}
	g := &grokSession{sess: sess, client: c}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := g.SendTurn(ctx, "Neutral prompt.", "action")
	if err != nil || !strings.Contains(string(result), `"native-prompt"`) {
		t.Fatalf("SendTurn=%s, %v", result, err)
	}
	var request struct {
		Method string                     `json:"method"`
		Params map[string]json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(<-sent, &request); err != nil {
		t.Fatal(err)
	}
	// This value is frozen from Claude's pre-switch UUID tuple, not calculated
	// with the implementation under test.
	const wantKey = "118da848-8c4f-500f-8d8d-d273dcaa64de"
	var prompt []map[string]any
	json.Unmarshal(request.Params["prompt"], &prompt)
	want := []map[string]any{{"type": "text", "text": "Neutral prompt.", "_meta": map[string]any{"x.nano/submissionId": wantKey}}}
	if request.Method != "session/prompt" || len(request.Params) != 2 || string(request.Params["sessionId"]) != `"`+grokFixtureThread+`"` || !reflect.DeepEqual(prompt, want) {
		t.Fatalf("ACP request=%+v prompt=%+v, want only the exact block key %s", request, prompt, wantKey)
	}
	if got := SubmissionEventID("hosted", "action"); got != wantKey {
		t.Fatalf("Claude submission UUID changed: %s", got)
	}
}

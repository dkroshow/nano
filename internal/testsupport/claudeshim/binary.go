package claudeshim

import (
	"os"
	"path/filepath"
	"testing"
)

// FakeBinary is a stand-in child: it emits one system.init line and then
// waits for stdin to close, so the direct transport's read loop, init capture
// and graceful close all run without the real CLI.
func FakeBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\n" +
		`echo '{"type":"system","subtype":"init","session_id":"native-direct","model":"sonnet"}'` + "\n" +
		"cat >/dev/null\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

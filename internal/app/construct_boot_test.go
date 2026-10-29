package app

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/config"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

// The real construction boundary must retain every owner opened before HTTP.
// An early lazy coordinator once captured a nil restart tracker, so even an
// empty boot served forever with complete=false and no boot-loop timestamp.
func TestConstructedServerReportsCompletedBoot(t *testing.T) {
	const child = "NANO_TEST_CONSTRUCTED_BOOT"
	if os.Getenv(child) != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestConstructedServerReportsCompletedBoot$", "-test.timeout=20s")
		command.Env = append(os.Environ(), child+"=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("constructed server boot: %v\n%s", err, output)
		}
		return
	}
	// A child test process bounds the background workers to this one boot.
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("TMPDIR", root)
	cfg, err := config.Parse([]string{"-data-dir", filepath.Join(root, "data"), "-cwd", root, "-port", "0", "-no-watch", "-usage=false", "-heartbeat=false", "-claude-bin", "/usr/bin/false", "-grok-bin", "/usr/bin/false", "-claude-shim-ws=", "-codex-ws", "ws://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	app := construct(cfg)
	// A sidebar captured before the stores opened silently loses star overlays.
	if err := fixtureSetStarred(t, app.server.threadStars, "codex", "boot-star", true); err != nil {
		t.Fatal(err)
	}
	row := sidebar.Thread{Thread: threadindex.Thread{Provider: "codex", NativeID: "boot-star"}}
	app.server.sidebarOwner().StarThread(&row)
	if !row.Starred {
		t.Fatal("constructed sidebar did not retain the opened star store")
	}
	local, remote := app.mount()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- app.serve(listener, local, remote) }()
	t.Cleanup(func() { listener.Close(); <-served })
	client := &http.Client{Timeout: time.Second}
	endpoint := "http://" + listener.Addr().String() + "/api/v1/restarts"
	var last string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(endpoint)
		if err != nil {
			last = err.Error()
			time.Sleep(10 * time.Millisecond)
			continue
		}
		var body struct {
			Restarts []struct {
				Complete   bool   `json:"complete"`
				BootLoopMS *int64 `json:"boot_loop_ms"`
			} `json:"restarts"`
		}
		err = json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if err == nil && response.StatusCode == http.StatusOK && len(body.Restarts) > 0 && body.Restarts[0].Complete && body.Restarts[0].BootLoopMS != nil {
			return
		}
		last = fmt.Sprintf("status=%d body=%+v decode=%v", response.StatusCode, body, err)
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("constructed boot never completed with a boot_loop_ms: %s", last)
}

package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The session owns retries and the status/error sent to the hosted-state owner.
// The app tests retain persistence coverage; this boundary can shorten retries.
type bootStatusRecorder struct {
	resultRecorder
	statusMu        sync.Mutex
	status, message string
}

func (r *bootStatusRecorder) UpdateSession(_, _, _, status, message string) error {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	r.status, r.message = status, message
	return nil
}
func (r *bootStatusRecorder) snapshotStatus() (string, string) {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	return r.status, r.message
}
func (*bootStatusRecorder) SessionNeedsAttention(string) bool { return false }

func TestGrokBootReattachRetriesSessionLoadTimeout(t *testing.T) {
	for _, tc := range []struct {
		name         string
		timeouts     int
		neverAnswers bool
		wantStatus   string
		wantAttempts int
	}{
		{name: "once", timeouts: 1, wantStatus: "ready", wantAttempts: 2},
		{name: "twice", timeouts: 2, wantStatus: "ready", wantAttempts: 3},
		{name: "three_times", timeouts: 3, wantStatus: "exited", wantAttempts: 3},
		{name: "never_answers", neverAnswers: true, wantStatus: "exited", wantAttempts: 3},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stateDir := t.TempDir()
			bin := writeRetryingGrokACP(t, stateDir, tc.timeouts, tc.neverAnswers)
			sess, _ := readyTransactionalSendTestSession(t, nil)
			sess.id, sess.provider = "grok-boot-"+tc.name, "grok"
			sess.hostedThreadID, sess.threadID = "nht_grok_boot_"+tc.name, "native-"+tc.name
			sess.state = map[string]json.RawMessage{}
			sess.projection = newDeliveryTestProjection("grok")
			sess.lifecycleState = testLifecycleSnapshot("starting", false)
			executions := &bootStatusRecorder{}
			sess.agentExecutions = executions
			t.Cleanup(sess.Close)

			if err := sess.BeginProviderSpawn(providerRequest{Provider: "grok", Mode: "resume", ThreadID: sess.ThreadID(), Cwd: stateDir, GrokBin: bin, BootReattach: &BootControl{loadTimeout: 50 * time.Millisecond, retryBackoffBase: time.Millisecond}}, nil, func() {}); err != nil {
				t.Fatal(err)
			}

			waitForGrokAttempts(t, stateDir, 1)
			waitForGrokStatus(t, sess, "reconnecting")
			waitForGrokStatus(t, sess, tc.wantStatus)
			if got := readGrokAttempts(t, stateDir); got != tc.wantAttempts {
				t.Fatalf("session/load attempts = %d, want %d", got, tc.wantAttempts)
			}
			// The hosted-status notification follows the in-memory transition,
			// so wait for that consumer to receive the same status.
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				if status, _ := executions.snapshotStatus(); status == tc.wantStatus {
					break
				}
			}
			status, message := executions.snapshotStatus()
			if status != tc.wantStatus {
				t.Fatalf("published hosted status = %q; want %s", status, tc.wantStatus)
			}
			if tc.wantStatus == "exited" {
				summary := sess.Summary()
				if !strings.Contains(summary.Error, "session/load") || !strings.Contains(summary.Error, context.DeadlineExceeded.Error()) {
					t.Fatalf("terminal reason = %q, want session/load deadline", summary.Error)
				}
				if message != summary.Error {
					t.Fatalf("published terminal reason = %q, want %q", message, summary.Error)
				}
			}
		})
	}
}

func writeRetryingGrokACP(t *testing.T, stateDir string, timeouts int, never bool) string {
	t.Helper()
	bin := filepath.Join(stateDir, "fake-grok-acp")
	neverValue := 0
	if never {
		neverValue = 1
	}
	script := fmt.Sprintf(`#!/bin/sh
state_dir=%s
lock="$state_dir/attempt.lock"
while ! mkdir "$lock" 2>/dev/null; do sleep 0.01; done
attempt=0
if [ -f "$state_dir/attempts" ]; then read -r attempt < "$state_dir/attempts"; fi
attempt=$((attempt + 1))
printf '%%s\n' "$attempt" > "$state_dir/attempts.$$"
mv "$state_dir/attempts.$$" "$state_dir/attempts"
rmdir "$lock"
IFS= read -r request || exit 1
printf '%%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
IFS= read -r request || exit 1
if [ %d -eq 1 ] || [ "$attempt" -le %d ]; then
  while :; do sleep 1; done
fi
printf '%%s\n' '{"jsonrpc":"2.0","id":2,"result":{"_meta":{"x.ai/sessionConfig":{"options":[{"category":"mode","id":"high","selected":true}]}}}}'
while IFS= read -r request; do :; done
`, strconv.Quote(stateDir), neverValue, timeouts)
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func readGrokAttempts(t *testing.T, stateDir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stateDir, "attempts"))
	if err != nil {
		return 0
	}
	attempts, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("decode Grok attempt count: %v", err)
	}
	return attempts
}

func waitForGrokAttempts(t *testing.T, stateDir string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if readGrokAttempts(t, stateDir) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Grok ACP attempts never reached %d", want)
}

func waitForGrokStatus(t *testing.T, sess *session, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sess.Summary().Status == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Grok session status = %q, want %q", sess.Summary().Status, want)
}

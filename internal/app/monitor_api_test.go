package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/observability"
	"github.com/dkroshow/nano/internal/sessions"
)

// Catches: the host monitor losing the active turns its finished-turn row
// checks, agents not seeing recorded alerts, the newest deploys or the newest
// snapshot's delivery lags, or the response growing with the alert or deploy
// history, which is read only from its end. Boundary: GET /api/v1/monitor,
// the monitor's only Nano read.
func TestMonitorServesActiveTurnsAndTheNewestAlerts(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "health-2026-10-01.jsonl")
	// Two snapshot lines in the recorded shape (trimmed); only the newest
	// one's delivery block is served.
	lines := `{"v":1,"ts":"2026-10-01T05:58:00Z","delivery":{"notices_overdue":3,"heartbeats_overdue":0,"query_ms":90}}` + "\n" +
		`{"v":1,"ts":"2026-10-01T05:59:00Z","delivery":{"prompt_settle":{"agent":{"count":63,"median_s":39.867,"max_s":685.422}},"notice_dispatch_latest":{"count":13,"median_s":0.1,"max_s":164.76},"heartbeat_wake":{"count":11,"median_s":0.101,"max_s":0.2},"notices_overdue":0,"heartbeats_overdue":1,"query_ms":120}}` + "\n"
	if err := os.WriteFile(snapshot, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(snapshot, now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	var alerts strings.Builder
	for i := range 1000 { // about 100 KB, past the tail the endpoint reads
		fmt.Fprintf(&alerts, `{"at":"2026-10-01T05:00:00Z","row":"swap","firing":true,"text":"alert %d","source":"host-monitor"}`+"\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "alerts.jsonl"), []byte(alerts.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	// Synthetic deployment records in the format accepted by the endpoint.
	var deploys strings.Builder
	for i := range 7 {
		fmt.Fprintf(&deploys, `{"v":1,"started_at":"2026-10-01T0%d:00:00Z","stamp":"s%d","restarted_at":"2026-10-01T0%d:01:00Z","offline_s":12,"result":"verified"}`+"\n", i, i, i)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploys.jsonl"), []byte(deploys.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newProviderRuntimeFixture(t, sessions.Config{ID: "working", Provider: "codex", HostedThreadID: "nht_working", DisplayName: "busy agent", CWD: "/w", CreatedAt: now}, "native-working", "model", func(f *providerRuntimeFixture) {
		f.resumeResult = map[string]any{"model": "model", "thread": map[string]any{"id": "native-working", "turns": []any{map[string]any{"id": "busy-turn", "status": "inProgress"}}}}
	})
	working := f.session
	idle := memorySession(t, "idle", "claude", "native-idle", "ready", "/w", "model", now)
	s := testServerWithSessions(&server{Registry: newSessionRegistry(), hostMonitor: observability.NewHostMonitor(dir, filepath.Join(dir, "deploys.jsonl"))}, map[string]*sessions.Session{working.ID(): working, idle.ID(): idle})

	rec := httptest.NewRecorder()
	s.monitorHandlers().HandleMonitor(rec, httptest.NewRequest(http.MethodGet, "/api/v1/monitor", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		ThreadsHeld int                        `json:"threadsHeld"`
		TurnsActive []observability.ActiveTurn `json:"turnsActive"`
		Monitor     map[string]any             `json:"monitor"`
		Alerts      []struct {
			Text string `json:"text"`
		} `json:"alerts"`
		Deploys []struct {
			Stamp string `json:"stamp"`
		} `json:"deploys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ThreadsHeld != 2 || len(body.TurnsActive) != 1 || body.TurnsActive[0] != (observability.ActiveTurn{Provider: "codex", ThreadID: "native-working", HostedThreadID: "nht_working", Name: "busy agent"}) {
		t.Fatalf("threads held %d, turns %+v", body.ThreadsHeld, body.TurnsActive)
	}
	delivery, _ := body.Monitor["delivery"].(map[string]any)
	if delivery["heartbeats_overdue"] != float64(1) || delivery["notices_overdue"] != float64(0) || delivery["prompt_settle"] == nil {
		t.Fatalf("monitor.delivery = %v; want the newest snapshot's delivery block", body.Monitor["delivery"])
	}
	if body.Monitor["stale"] != false {
		t.Fatalf("monitor = %v; a one-minute-old snapshot is fresh", body.Monitor)
	}
	if len(body.Alerts) != 50 || body.Alerts[0].Text != "alert 950" || body.Alerts[len(body.Alerts)-1].Text != "alert 999" {
		t.Fatalf("alerts = %d, first %q; want the newest %d, oldest first", len(body.Alerts), body.Alerts[0].Text, 50)
	}
	if len(body.Deploys) != 5 || body.Deploys[0].Stamp != "s6" || body.Deploys[5-1].Stamp != "s2" {
		t.Fatalf("deploys = %+v; want the newest %d, newest first", body.Deploys, 5)
	}
}

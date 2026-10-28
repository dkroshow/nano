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

	"github.com/dkroshow/nano/internal/lifecycle"
)

// Catches: the host monitor losing the active turns its finished-turn row
// checks, agents not seeing recorded alerts, the newest deploys or the newest
// snapshot's delivery lags, or the response growing with the alert or deploy
// history, which is read only from its end. Boundary: GET /api/v1/monitor,
// the monitor's only Nano read.
func TestMonitorServesActiveTurnsAndTheNewestAlerts(t *testing.T) {
	now := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
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
	if err := os.WriteFile(filepath.Join(dir, monitorAlertsName), []byte(alerts.String()), 0o600); err != nil {
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

	working := memorySession("working", "codex", "native-working", "ready", "/w", "model", now)
	working.hostedThreadID, working.displayName = "nht_working", "busy agent"
	working.lifecycleState = lifecycle.Snapshot{State: lifecycle.State{Connection: lifecycle.Ready, Work: lifecycle.Working}, Epoch: 1}
	idle := memorySession("idle", "claude", "native-idle", "ready", "/w", "model", now)
	s := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{working.id: working, idle.id: idle}}, hostMonitor: newHostMonitor(dir, filepath.Join(dir, "deploys.jsonl"))}
	s.hostMonitor.now = func() time.Time { return now }

	rec := httptest.NewRecorder()
	s.handleMonitor(rec, httptest.NewRequest(http.MethodGet, "/api/v1/monitor", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		ThreadsHeld int            `json:"threadsHeld"`
		TurnsActive []monitorTurn  `json:"turnsActive"`
		Monitor     map[string]any `json:"monitor"`
		Alerts      []monitorAlert `json:"alerts"`
		Deploys     []struct {
			Stamp string `json:"stamp"`
		} `json:"deploys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ThreadsHeld != 2 || len(body.TurnsActive) != 1 || body.TurnsActive[0] != (monitorTurn{Provider: "codex", ThreadID: "native-working", HostedThreadID: "nht_working", Name: "busy agent"}) {
		t.Fatalf("threads held %d, turns %+v", body.ThreadsHeld, body.TurnsActive)
	}
	delivery, _ := body.Monitor["delivery"].(map[string]any)
	if delivery["heartbeats_overdue"] != float64(1) || delivery["notices_overdue"] != float64(0) || delivery["prompt_settle"] == nil {
		t.Fatalf("monitor.delivery = %v; want the newest snapshot's delivery block", body.Monitor["delivery"])
	}
	if body.Monitor["stale"] != false {
		t.Fatalf("monitor = %v; a one-minute-old snapshot is fresh", body.Monitor)
	}
	if len(body.Alerts) != monitorAlertsShown || body.Alerts[0].Text != "alert 950" || body.Alerts[len(body.Alerts)-1].Text != "alert 999" {
		t.Fatalf("alerts = %d, first %q; want the newest %d, oldest first", len(body.Alerts), body.Alerts[0].Text, monitorAlertsShown)
	}
	if len(body.Deploys) != monitorDeploysShown || body.Deploys[0].Stamp != "s6" || body.Deploys[monitorDeploysShown-1].Stamp != "s2" {
		t.Fatalf("deploys = %+v; want the newest %d, newest first", body.Deploys, monitorDeploysShown)
	}
}

// Catches: a stopped monitor job going unrecorded, its alert repeated every
// minute, one raised where no monitor is installed, or a stale alert never
// cleared because Nano restarted before snapshots resumed. Boundary: Nano's
// freshness check over the monitor's directory.
func TestHostMonitorRecordsStoppedSnapshotsOnceAndTheirReturn(t *testing.T) {
	now := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	m := newHostMonitor(dir, filepath.Join(dir, "deploys.jsonl"))
	m.now = func() time.Time { return now }
	alertLines := func() []string {
		data, err := os.ReadFile(filepath.Join(dir, monitorAlertsName))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}

	if err := m.check(); err != nil || alertLines() != nil {
		t.Fatalf("no monitor installed: err %v, alerts %v", err, alertLines())
	}
	snapshot := filepath.Join(dir, "health-2026-10-01.jsonl")
	if err := os.WriteFile(snapshot, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(snapshot, now.Add(-5*time.Minute), now.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := m.check(); err != nil {
			t.Fatal(err)
		}
	}
	if lines := alertLines(); len(lines) != 1 || !strings.Contains(lines[0], `"row":"monitor-stale","firing":true`) {
		t.Fatalf("after the job stopped: %v; want one firing line", lines)
	}
	// Other rows record more changes than the API shows, then Nano restarts
	// and snapshots resume before its first check.
	f, err := os.OpenFile(filepath.Join(dir, monitorAlertsName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for i := range monitorAlertsShown + 10 {
		fmt.Fprintf(f, `{"at":"2026-10-01T06:00:00Z","row":"swap","firing":%t,"text":"x","source":"host-monitor"}`+"\n", i%2 == 0)
	}
	f.Close()
	if err := os.Chtimes(snapshot, now, now); err != nil {
		t.Fatal(err)
	}
	restarted := newHostMonitor(dir, filepath.Join(dir, "deploys.jsonl"))
	restarted.now = m.now
	if err := restarted.check(); err != nil {
		t.Fatal(err)
	}
	if lines := alertLines(); len(lines) != monitorAlertsShown+12 || !strings.Contains(lines[len(lines)-1], `"row":"monitor-stale","firing":false`) {
		t.Fatalf("after a restart and snapshots resuming: %d lines, last %q; want a monitor-stale clear line", len(lines), lines[len(lines)-1])
	}
}

// Catches: an older delivery block served after the newest snapshot's probe
// failed, so a stale lag looks current, or a partial last line (a snapshot
// being written) breaking the read. Boundary: GET /api/v1/monitor.
func TestMonitorDeliveryComesFromTheNewestCompleteSnapshot(t *testing.T) {
	for _, c := range []struct {
		name, lines string
		want        any
	}{
		{"partial last line", `{"ts":"a","delivery":{"notices_overdue":1}}` + "\n" + `{"ts":"b","delivery":{"notices_overdue":2}}` + "\n" + `{"ts":"c","deli`, float64(2)},
		{"newest probe failed", `{"ts":"a","delivery":{"notices_overdue":1}}` + "\n" + `{"ts":"b","probe_errors":{"delivery":"database is locked"}}` + "\n", nil},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "health-2026-10-01.jsonl"), []byte(c.lines), 0o600); err != nil {
			t.Fatal(err)
		}
		s := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{}}, hostMonitor: newHostMonitor(dir, filepath.Join(dir, "deploys.jsonl"))}
		rec := httptest.NewRecorder()
		s.handleMonitor(rec, httptest.NewRequest(http.MethodGet, "/api/v1/monitor", nil))
		var body struct {
			Monitor struct {
				Delivery map[string]any `json:"delivery"`
			} `json:"monitor"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("%s: status %d: %s", c.name, rec.Code, rec.Body)
		}
		var got any
		if body.Monitor.Delivery != nil {
			got = body.Monitor.Delivery["notices_overdue"]
		}
		if got != c.want {
			t.Errorf("%s: delivery %v; want notices_overdue %v", c.name, body.Monitor.Delivery, c.want)
		}
	}
}

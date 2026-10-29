package observability

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
)

// Catches: a stopped monitor job going unrecorded, its alert repeated every
// minute, one raised where no monitor is installed, or a stale alert never
// cleared because Nano restarted before snapshots resumed. Boundary: Nano's
// freshness check over the monitor's directory.
func TestHostMonitorRecordsStoppedSnapshotsOnceAndTheirReturn(t *testing.T) {
	now := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	m := NewHostMonitor(dir, filepath.Join(dir, "deploys.jsonl"))
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
	restarted := NewHostMonitor(dir, filepath.Join(dir, "deploys.jsonl"))
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
		s := &MonitorHandlers{Monitor: NewHostMonitor(dir, filepath.Join(dir, "deploys.jsonl")), Requests: &RequestTimes{}, Live: func() (int, []ActiveTurn) { return 0, []ActiveTurn{} }, WarmStreams: func() any { return nil }, Restarts: func() (any, error) { return nil, nil }}
		rec := httptest.NewRecorder()
		s.HandleMonitor(rec, httptest.NewRequest(http.MethodGet, "/api/v1/monitor", nil))
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

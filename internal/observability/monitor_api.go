package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/threadindex"
)

// The monitor endpoint exposes Nano process and active-turn state to operator
// tools. If a tool writes host snapshots and alerts in the health directory,
// Nano serves the latest records and flags snapshots that have stopped updating.

const (
	// Snapshots land every minute and the check runs every minute, so a
	// four-minute threshold records a stopped monitor within five minutes.
	monitorStaleAfter  = 4 * time.Minute
	monitorAlertsShown = 50
	monitorAlertsName  = "alerts.jsonl"
	// The newest 50 alert lines are a few KB; the file is never read whole.
	monitorAlertsTail = 64 << 10
	// Optional deploy records are under 1 KB each, served newest
	// first beside the restart records they join by stamp.
	monitorDeploysShown = 5
	monitorDeploysTail  = 16 << 10
	// A snapshot line is a few KB, more when a run catches up on request
	// minutes; the newest complete line within this tail is served.
	monitorSnapshotTail = 256 << 10
)

// HostMonitor reads host snapshots and records changes in snapshot freshness.
type HostMonitor struct {
	healthDir   string
	deploysPath string
	now         func() time.Time

	mu     sync.Mutex
	stale  bool
	loaded bool // stale has been restored from the alert record
}

type monitorAlert struct {
	At     time.Time `json:"at"`
	Row    string    `json:"row"`
	Firing bool      `json:"firing"`
	Text   string    `json:"text"`
	Source string    `json:"source"`
}

// NewHostMonitor watches the supplied snapshot directory using the supplied clock.
// A nil clock uses time.Now.
func NewHostMonitor(healthDir, deploysPath string, now func() time.Time) *HostMonitor {
	if now == nil {
		now = time.Now
	}
	return &HostMonitor{healthDir: healthDir, deploysPath: deploysPath, now: now}
}

// recentDeploys returns the newest deploy records, newest first.
func (m *HostMonitor) recentDeploys() ([]json.RawMessage, error) {
	lines, err := TailJSONLines(m.deploysPath, monitorDeploysTail)
	if err != nil {
		return nil, err
	}
	deploys := []json.RawMessage{}
	for i := len(lines) - 1; i >= 0 && len(deploys) < monitorDeploysShown; i-- {
		deploys = append(deploys, lines[i])
	}
	return deploys, nil
}

// newestSnapshot returns the snapshot file the monitor last wrote and when;
// zero when it never has, which means no monitor is installed.
func (m *HostMonitor) newestSnapshot() (string, time.Time, error) {
	paths, err := filepath.Glob(filepath.Join(m.healthDir, "health-*.jsonl"))
	if err != nil {
		return "", time.Time{}, err
	}
	var file string
	var newest time.Time
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return "", time.Time{}, err
		}
		if info.ModTime().After(newest) {
			file, newest = path, info.ModTime()
		}
	}
	return file, newest, nil
}

// latestDelivery returns the newest snapshot's delivery block, as the monitor
// recorded it, so agents read delivery lags and overdue counts here as well;
// null when that run's delivery probe failed.
func (m *HostMonitor) latestDelivery(file string) (json.RawMessage, error) {
	lines, err := TailJSONLines(file, monitorSnapshotTail)
	if err != nil || len(lines) == 0 {
		return nil, err
	}
	var snapshot struct {
		Delivery json.RawMessage `json:"delivery"`
	}
	if err := json.Unmarshal(lines[len(lines)-1], &snapshot); err != nil {
		return nil, err
	}
	return snapshot.Delivery, nil
}

// check records a monitor-stale alert when snapshots stop, and a clear line
// when they resume.
func (m *HostMonitor) check() error {
	_, newest, err := m.newestSnapshot()
	if err != nil || newest.IsZero() {
		return err
	}
	now := m.now()
	stale := now.Sub(newest) > monitorStaleAfter
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		// A Nano restart must still clear a stale alert it recorded before.
		// Search the whole tail, not just the lines the API shows.
		alerts, err := m.tailAlerts()
		if err != nil {
			return err
		}
		for _, line := range alerts {
			var alert monitorAlert
			if json.Unmarshal(line, &alert) == nil && alert.Row == "monitor-stale" {
				m.stale = alert.Firing
			}
		}
		m.loaded = true
	}
	if stale == m.stale {
		return nil
	}
	text := "host snapshots resumed"
	if stale {
		text = fmt.Sprintf("no host snapshot for %s: the monitor job may have stopped", now.Sub(newest).Round(time.Minute))
		log.Printf("WARNING: %s (newest in %s)", text, m.healthDir)
	}
	if err := m.record(monitorAlert{At: now.UTC(), Row: "monitor-stale", Firing: stale, Text: text, Source: "nano"}); err != nil {
		return err
	}
	m.stale = stale
	return nil
}

func (m *HostMonitor) record(alert monitorAlert) error {
	line, err := json.Marshal(alert)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(m.healthDir, monitorAlertsName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	return errors.Join(werr, f.Close())
}

// Run checks snapshot freshness until the context ends.
func (m *HostMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := m.check(); err != nil {
			log.Printf("host monitor freshness: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tailAlerts returns the alert lines in the last monitorAlertsTail bytes of
// the file, oldest first.
func (m *HostMonitor) tailAlerts() ([]json.RawMessage, error) {
	return TailJSONLines(filepath.Join(m.healthDir, monitorAlertsName), monitorAlertsTail)
}

// MonitorHandlers joins host diagnostics with snapshots from their live owners.
// The callbacks only read response values; those owners retain their state.
type MonitorHandlers struct {
	Monitor     *HostMonitor
	Requests    *RequestTimes
	Catalog     *threadindex.Service
	Live        func() (int, []ActiveTurn)
	WarmStreams func() any
	Restarts    func() (any, error)
}

// HandleMonitor serves process, host and active-turn diagnostics.
func (s *MonitorHandlers) HandleMonitor(w http.ResponseWriter, _ *http.Request) {
	held, turns := s.Live()

	file, newest, err := s.Monitor.newestSnapshot()
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "monitor_unreadable", err.Error())
		return
	}
	var delivery json.RawMessage
	if file != "" {
		if delivery, err = s.Monitor.latestDelivery(file); err != nil {
			httpapi.WriteError(w, http.StatusInternalServerError, "monitor_unreadable", err.Error())
			return
		}
	}
	alerts, err := s.Monitor.tailAlerts()
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "monitor_unreadable", err.Error())
		return
	}
	alerts = alerts[max(0, len(alerts)-monitorAlertsShown):]
	restarts, err := s.Restarts()
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "monitor_unreadable", err.Error())
		return
	}
	deploys, err := s.Monitor.recentDeploys()
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "monitor_unreadable", err.Error())
		return
	}
	snapshot := map[string]any{"newestSnapshotAt": nil, "stale": false, "delivery": nil}
	if !newest.IsZero() {
		snapshot = map[string]any{"newestSnapshotAt": newest.UTC(), "stale": s.Monitor.now().Sub(newest) > monitorStaleAfter, "delivery": delivery}
	}
	requests, streams := s.Requests.Report()
	var ingest *threadindex.IngestStatus
	if s.Catalog != nil {
		status := s.Catalog.IngestStatus()
		ingest = &status
	}

	w.Header().Set("Cache-Control", "no-cache")
	httpapi.WriteJSON(w, map[string]any{
		"ingest":      ingest,
		"threadsHeld": held,
		"turnsActive": turns,
		"monitor":     snapshot,
		"alerts":      alerts,
		"requests":    requests,
		"streamsOpen": streams,
		"warmStreams": s.WarmStreams(),
		"restarts":    restarts,
		"deploys":     deploys,
	})
}

// HandleIngestReceipts serves the latest ingestion receipt of every held
// native file, newest first, for probes that join Nano's stage times to their
// own file and browser observations.
func (s *MonitorHandlers) HandleIngestReceipts(w http.ResponseWriter, _ *http.Request) {
	if s.Catalog == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	httpapi.WriteJSON(w, map[string]any{"receipts": s.Catalog.IngestReceipts()})
}

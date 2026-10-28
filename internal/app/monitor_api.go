package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/observability"
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

type hostMonitor struct {
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

func newHostMonitor(healthDir, deploysPath string) *hostMonitor {
	return &hostMonitor{healthDir: healthDir, deploysPath: deploysPath, now: time.Now}
}

// recentDeploys returns the newest deploy records, newest first.
func (m *hostMonitor) recentDeploys() ([]json.RawMessage, error) {
	lines, err := tailJSONLines(m.deploysPath, monitorDeploysTail)
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
func (m *hostMonitor) newestSnapshot() (string, time.Time, error) {
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
func (m *hostMonitor) latestDelivery(file string) (json.RawMessage, error) {
	lines, err := tailJSONLines(file, monitorSnapshotTail)
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
func (m *hostMonitor) check() error {
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

func (m *hostMonitor) record(alert monitorAlert) error {
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

func (m *hostMonitor) run(ctx context.Context) {
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
func (m *hostMonitor) tailAlerts() ([]json.RawMessage, error) {
	return tailJSONLines(filepath.Join(m.healthDir, monitorAlertsName), monitorAlertsTail)
}

// tailJSONLines returns the valid JSON lines in the last tail bytes of an
// append-only ledger, oldest first; a missing file has none. The alert and
// restart ledgers share it.
func tailJSONLines(path string, tail int64) ([]json.RawMessage, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := max(0, info.Size()-tail)
	data := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(data, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if start > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:] // drop the partial first line
		}
	}
	lines := []json.RawMessage{}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		if json.Valid(line) {
			lines = append(lines, json.RawMessage(line))
		}
	}
	return lines, nil
}

func (s *server) handleMonitor(w http.ResponseWriter, _ *http.Request) {
	sessions := s.SnapshotSessions()
	turns := []monitorTurn{}
	for _, sess := range sessions {
		if turn, active := sess.ActiveTurnSnapshot(); active {
			turns = append(turns, turn)
		}
	}
	file, newest, err := s.hostMonitor.newestSnapshot()
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "monitor_unreadable", err.Error())
		return
	}
	var delivery json.RawMessage
	if file != "" {
		if delivery, err = s.hostMonitor.latestDelivery(file); err != nil {
			httpapi.WriteError(w, http.StatusInternalServerError, "monitor_unreadable", err.Error())
			return
		}
	}
	alerts, err := s.hostMonitor.tailAlerts()
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "monitor_unreadable", err.Error())
		return
	}
	alerts = alerts[max(0, len(alerts)-monitorAlertsShown):]
	restarts, err := s.restart.recent(restartsDefaultLimit, false)
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "monitor_unreadable", err.Error())
		return
	}
	deploys, err := s.hostMonitor.recentDeploys()
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "monitor_unreadable", err.Error())
		return
	}
	snapshot := map[string]any{"newestSnapshotAt": nil, "stale": false, "delivery": nil}
	if !newest.IsZero() {
		snapshot = map[string]any{"newestSnapshotAt": newest.UTC(), "stale": s.hostMonitor.now().Sub(newest) > monitorStaleAfter, "delivery": delivery}
	}
	requests, streams := s.requests.Report()
	var ingest *threadindex.IngestStatus
	if catalog, ok := s.catalog.(*threadCatalogService); ok {
		status := catalog.IngestStatus()
		ingest = &status
	}
	w.Header().Set("Cache-Control", "no-cache")
	httpapi.WriteJSON(w, map[string]any{
		"ingest":      ingest,
		"threadsHeld": len(sessions),
		"turnsActive": turns,
		"monitor":     snapshot,
		"alerts":      alerts,
		"requests":    requests,
		"streamsOpen": streams,
		"warmStreams": s.warm.report(),
		"restarts":    restarts,
		"deploys":     deploys,
	})
}

// handleIngestReceipts serves the latest ingestion receipt of every held
// native file, newest first, for probes that join Nano's stage times to their
// own file and browser observations.
func (s *server) handleIngestReceipts(w http.ResponseWriter, _ *http.Request) {
	catalog, ok := s.catalog.(*threadCatalogService)
	if !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	httpapi.WriteJSON(w, map[string]any{"receipts": catalog.IngestReceipts()})
}

type monitorTurn = observability.ActiveTurn

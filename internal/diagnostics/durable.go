package diagnostics

import (
	"database/sql"
	"errors"
	"strings"
)

type BrowserDuplicateIncident struct {
	IncidentFingerprint        string `json:"incidentFingerprint"`
	Provider                   string `json:"provider,omitempty"`
	ThreadID                   string `json:"threadId,omitempty"`
	HostedThreadID             string `json:"hostedThreadId,omitempty"`
	RawRenderKeyFingerprint    string `json:"rawRenderKeyFingerprint,omitempty"`
	SemanticIDFingerprint      string `json:"semanticIdFingerprint,omitempty"`
	RawRenderKeyDuplicateCount int    `json:"rawRenderKeyDuplicateCount"`
	SemanticIDDuplicateCount   int    `json:"semanticIdDuplicateCount"`
	FirstBuild                 string `json:"firstBuild,omitempty"`
	LastBuild                  string `json:"lastBuild,omitempty"`
	FirstPaneID                string `json:"firstPaneId"`
	LastPaneID                 string `json:"lastPaneId"`
	FirstObservedAt            string `json:"firstObservedAt"`
	LastObservedAt             string `json:"lastObservedAt"`
	ObservationCount           int64  `json:"observationCount"`
}

type BrowserDuplicateIncidentFilter struct {
	ThreadID, HostedThreadID, PaneID string
	Limit                            int
}

type browserDuplicateScanner interface{ Scan(...any) error }

func scanBrowserDuplicateIncident(scanner browserDuplicateScanner) (BrowserDuplicateIncident, error) {
	var value BrowserDuplicateIncident
	err := scanner.Scan(&value.IncidentFingerprint, &value.Provider, &value.ThreadID, &value.HostedThreadID,
		&value.RawRenderKeyFingerprint, &value.SemanticIDFingerprint,
		&value.RawRenderKeyDuplicateCount, &value.SemanticIDDuplicateCount,
		&value.FirstBuild, &value.LastBuild, &value.FirstPaneID, &value.LastPaneID,
		&value.FirstObservedAt, &value.LastObservedAt, &value.ObservationCount)
	return value, err
}

const browserDuplicateIncidentSelect = `SELECT incident_fingerprint,provider,thread_id,hosted_thread_id,
	raw_render_key_fingerprint,semantic_id_fingerprint,raw_render_key_duplicate_count,semantic_id_duplicate_count,
	first_build,last_build,first_pane_id,last_pane_id,first_observed_at,last_observed_at,observation_count FROM browser_duplicate_incidents`

func ObserveBrowserDuplicateIncident(db *sql.DB, observation BrowserDuplicateIncident) (BrowserDuplicateIncident, bool, error) {
	if db == nil {
		return BrowserDuplicateIncident{}, false, errors.New("durable browser duplicate store unavailable")
	}
	result, err := db.Exec(`INSERT INTO browser_duplicate_incidents(
		incident_fingerprint,provider,thread_id,hosted_thread_id,raw_render_key_fingerprint,semantic_id_fingerprint,
		raw_render_key_duplicate_count,semantic_id_duplicate_count,first_build,last_build,first_pane_id,last_pane_id,
		first_observed_at,last_observed_at,observation_count,alert_state)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'pending')
		ON CONFLICT(incident_fingerprint) DO UPDATE SET
			provider=excluded.provider,thread_id=excluded.thread_id,hosted_thread_id=excluded.hosted_thread_id,
			last_build=excluded.last_build,last_pane_id=excluded.last_pane_id,last_observed_at=excluded.last_observed_at,
			observation_count=browser_duplicate_incidents.observation_count+1`,
		observation.IncidentFingerprint, observation.Provider, observation.ThreadID, observation.HostedThreadID,
		observation.RawRenderKeyFingerprint, observation.SemanticIDFingerprint,
		observation.RawRenderKeyDuplicateCount, observation.SemanticIDDuplicateCount,
		observation.FirstBuild, observation.LastBuild, observation.FirstPaneID, observation.LastPaneID,
		observation.FirstObservedAt, observation.LastObservedAt, observation.ObservationCount)
	if err != nil {
		return BrowserDuplicateIncident{}, false, err
	}
	changed, _ := result.RowsAffected()
	stored, err := scanBrowserDuplicateIncident(db.QueryRow(browserDuplicateIncidentSelect+` WHERE incident_fingerprint=?`, observation.IncidentFingerprint))
	return stored, changed == 1 && stored.ObservationCount == 1, err
}

func ListBrowserDuplicateIncidents(db *sql.DB, filter BrowserDuplicateIncidentFilter) ([]BrowserDuplicateIncident, error) {
	if db == nil {
		return nil, errors.New("durable browser duplicate store unavailable")
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	where, args := []string{"1=1"}, []any{}
	if filter.ThreadID != "" {
		where, args = append(where, "thread_id=?"), append(args, filter.ThreadID)
	}
	if filter.HostedThreadID != "" {
		where, args = append(where, "hosted_thread_id=?"), append(args, filter.HostedThreadID)
	}
	if filter.PaneID != "" {
		where, args = append(where, "(first_pane_id=? OR last_pane_id=?)"), append(args, filter.PaneID, filter.PaneID)
	}
	args = append(args, limit)
	rows, err := db.Query(browserDuplicateIncidentSelect+` WHERE `+strings.Join(where, " AND ")+` ORDER BY last_observed_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []BrowserDuplicateIncident{}
	for rows.Next() {
		incident, err := scanBrowserDuplicateIncident(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, incident)
	}
	return result, rows.Err()
}

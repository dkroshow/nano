package app

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
)

const (
	defaultTranscriptValidationCapacity = 500
	maxTranscriptValidationBodyBytes    = 256 << 10
	maxTranscriptValidationRows         = 500
	maxTranscriptValidationHistory      = 20
	maxTranscriptValidationStoreBytes   = 4 << 20
)

// transcriptValidationSnapshot is a browser-observed comparison between the
// ordered rows the transcript component intended to render and those actually
// mounted in its DOM. It is deliberately content-free.
type transcriptValidationSnapshot struct {
	SnapshotID               string                    `json:"snapshotId"`
	CheckedAt                string                    `json:"checkedAt,omitempty"`
	ReceivedAt               string                    `json:"receivedAt,omitempty"`
	TabID                    string                    `json:"tabId"`
	PageID                   string                    `json:"pageId,omitempty"`
	Build                    string                    `json:"build,omitempty"`
	PaneID                   string                    `json:"paneId"`
	Surface                  string                    `json:"surface"`
	ViewMode                 string                    `json:"viewMode,omitempty"`
	Provider                 string                    `json:"provider,omitempty"`
	ThreadID                 string                    `json:"threadId,omitempty"`
	SessionID                string                    `json:"sessionId,omitempty"`
	HostedThreadID           string                    `json:"hostedThreadId,omitempty"`
	Epoch                    string                    `json:"epoch,omitempty"`
	Revision                 int64                     `json:"revision,omitempty"`
	ActiveRenderSource       string                    `json:"activeRenderSource,omitempty"`
	PageVisible              *bool                     `json:"pageVisible,omitempty"`
	SurfaceVisible           *bool                     `json:"surfaceVisible,omitempty"`
	Fresh                    bool                      `json:"fresh"`
	AgeMS                    int64                     `json:"ageMs"`
	Status                   string                    `json:"status"`
	DuplicateRenderKeyCount  int                       `json:"duplicateRenderKeyCount"`
	DuplicateSemanticIDCount int                       `json:"duplicateSemanticIdCount"`
	ExpectedRows             []transcriptValidationRow `json:"expectedRows"`
	MountedRows              []transcriptValidationRow `json:"mountedRows"`
}

type transcriptValidationRow struct {
	Key            string `json:"key"`
	Source         string `json:"source"`
	ID             string `json:"id"`
	SemanticID     string `json:"semanticId,omitempty"`
	Type           string `json:"type"`
	ExpectsContent *bool  `json:"expectsContent,omitempty"`
	HasContent     *bool  `json:"hasContent,omitempty"`
	Active         *bool  `json:"active,omitempty"`
	Status         string `json:"status,omitempty"`
	Group          string `json:"group,omitempty"`
	Depth          *int   `json:"depth,omitempty"`
}

type storedTranscriptValidation struct {
	snapshot  transcriptValidationSnapshot
	signature string
	bytes     int
}

type transcriptValidationStore struct {
	mu       sync.Mutex
	capacity int
	order    []string
	latest   map[string]storedTranscriptValidation
	history  map[string][]storedTranscriptValidation
	bytes    int
}

func newTranscriptValidationStore(capacity int) *transcriptValidationStore {
	if capacity <= 0 {
		capacity = defaultTranscriptValidationCapacity
	}
	return &transcriptValidationStore{
		capacity: capacity,
		latest:   make(map[string]storedTranscriptValidation),
		history:  make(map[string][]storedTranscriptValidation),
	}
}

func transcriptValidationPaneKey(snapshot transcriptValidationSnapshot) string {
	return snapshot.TabID + "\x00" + snapshot.PaneID + "\x00" + snapshot.Surface
}

func transcriptValidationSignature(snapshot transcriptValidationSnapshot) string {
	value, _ := json.Marshal(struct {
		Surface      string                    `json:"surface"`
		ViewMode     string                    `json:"viewMode"`
		Build        string                    `json:"build"`
		ThreadID     string                    `json:"threadId"`
		HostedID     string                    `json:"hostedThreadId"`
		Status       string                    `json:"status"`
		ExpectedRows []transcriptValidationRow `json:"expectedRows"`
		MountedRows  []transcriptValidationRow `json:"mountedRows"`
	}{snapshot.Surface, snapshot.ViewMode, snapshot.Build, snapshot.ThreadID, snapshot.HostedThreadID, snapshot.Status, snapshot.ExpectedRows, snapshot.MountedRows})
	return string(value)
}

func storedValidation(snapshot transcriptValidationSnapshot) storedTranscriptValidation {
	value, _ := json.Marshal(snapshot)
	return storedTranscriptValidation{snapshot: snapshot, signature: transcriptValidationSignature(snapshot), bytes: len(value)}
}

func (s *transcriptValidationStore) put(snapshot transcriptValidationSnapshot) bool {
	if s == nil || !normalizeTranscriptValidation(&snapshot) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := transcriptValidationPaneKey(snapshot)
	previous, exists := s.latest[key]
	if exists {
		for index, candidate := range s.order {
			if candidate == key {
				s.order = append(s.order[:index], s.order[index+1:]...)
				break
			}
		}
	}
	s.order = append(s.order, key)
	snapshot.ReceivedAt = time.Now().UTC().Format(time.RFC3339Nano)
	stored := storedValidation(snapshot)
	if exists {
		s.bytes -= previous.bytes
	}
	s.latest[key] = stored
	s.bytes += stored.bytes
	if !exists || previous.signature != stored.signature {
		s.history[key] = append(s.history[key], stored)
		s.bytes += stored.bytes
		s.trimSurfaceHistory(key)
	}
	for len(s.order) > s.capacity {
		s.deleteSurface(s.order[0])
		s.order = s.order[1:]
	}
	s.trimToByteLimit()
	return true
}

func (s *transcriptValidationStore) trimSurfaceHistory(key string) {
	for len(s.history[key]) > maxTranscriptValidationHistory {
		remove := 0
		for index, entry := range s.history[key] {
			if entry.snapshot.Status == "match" {
				remove = index
				break
			}
		}
		s.bytes -= s.history[key][remove].bytes
		s.history[key] = append(s.history[key][:remove], s.history[key][remove+1:]...)
	}
}

func (s *transcriptValidationStore) deleteSurface(key string) {
	if latest, ok := s.latest[key]; ok {
		s.bytes -= latest.bytes
		delete(s.latest, key)
	}
	for _, entry := range s.history[key] {
		s.bytes -= entry.bytes
	}
	delete(s.history, key)
}

func (s *transcriptValidationStore) trimToByteLimit() {
	for s.bytes > maxTranscriptValidationStoreBytes {
		removed := false
		for _, status := range []string{"match", ""} {
			for _, key := range s.order {
				for index, entry := range s.history[key] {
					if status != "" && entry.snapshot.Status != status {
						continue
					}
					s.bytes -= entry.bytes
					s.history[key] = append(s.history[key][:index], s.history[key][index+1:]...)
					removed = true
					break
				}
				if removed {
					break
				}
			}
			if removed {
				break
			}
		}
		if removed {
			continue
		}
		if len(s.order) == 0 {
			break
		}
		s.deleteSurface(s.order[0])
		s.order = s.order[1:]
	}
}

func (s *transcriptValidationStore) list(limit int, threadID, hostedThreadID, paneID, surface, status string) ([]transcriptValidationSnapshot, []transcriptValidationSnapshot) {
	if s == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > s.capacity {
		limit = s.capacity
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]transcriptValidationSnapshot, 0, limit)
	history := make([]transcriptValidationSnapshot, 0, limit)
	now := time.Now().UTC()
	for index := len(s.order) - 1; index >= 0 && len(result) < limit; index-- {
		key := s.order[index]
		snapshot := s.latest[key].snapshot
		if !transcriptValidationMatches(snapshot, threadID, hostedThreadID, paneID, surface, status) {
			continue
		}
		snapshot = currentTranscriptValidation(snapshot, now)
		result = append(result, snapshot)
	}
	for _, entries := range s.history {
		for _, entry := range entries {
			if transcriptValidationMatches(entry.snapshot, threadID, hostedThreadID, paneID, surface, status) {
				history = append(history, currentTranscriptValidation(entry.snapshot, now))
			}
		}
	}
	sort.Slice(history, func(i, j int) bool {
		// RFC3339Nano drops trailing zeroes, so text order is not time order.
		left, _ := time.Parse(time.RFC3339Nano, history[i].ReceivedAt)
		right, _ := time.Parse(time.RFC3339Nano, history[j].ReceivedAt)
		return left.After(right)
	})
	if len(history) > limit {
		history = history[:limit]
	}
	return result, history
}

// freshVisibleThread reuses the browser's existing mounted-transcript proof as
// repair priority evidence. It is not a second presence system: the same
// 45-second freshness rule powers the public validation response.
func (s *transcriptValidationStore) freshVisibleThread(threadID string, now time.Time) bool {
	if s == nil || threadID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.latest {
		snapshot := entry.snapshot
		if snapshot.Surface != "transcript" || snapshot.ThreadID != threadID {
			continue
		}
		if currentTranscriptValidation(snapshot, now).Fresh {
			return true
		}
	}
	return false
}

func transcriptValidationMatches(snapshot transcriptValidationSnapshot, threadID, hostedThreadID, paneID, surface, status string) bool {
	return (threadID == "" || snapshot.ThreadID == threadID) &&
		(hostedThreadID == "" || snapshot.HostedThreadID == hostedThreadID) &&
		(paneID == "" || snapshot.PaneID == paneID) &&
		(surface == "" || snapshot.Surface == surface) &&
		(status == "" || snapshot.Status == status)
}

func currentTranscriptValidation(snapshot transcriptValidationSnapshot, now time.Time) transcriptValidationSnapshot {
	receivedAt, _ := time.Parse(time.RFC3339Nano, snapshot.ReceivedAt)
	snapshot.AgeMS = max(0, now.Sub(receivedAt).Milliseconds())
	snapshot.Fresh = snapshot.PageVisible != nil && *snapshot.PageVisible &&
		snapshot.SurfaceVisible != nil && *snapshot.SurfaceVisible && snapshot.AgeMS <= 45_000
	return snapshot
}

func normalizeTranscriptValidation(snapshot *transcriptValidationSnapshot) bool {
	if snapshot == nil {
		return false
	}
	snapshot.SnapshotID = diagnosticToken(snapshot.SnapshotID, 160)
	snapshot.CheckedAt = diagnosticTime(snapshot.CheckedAt)
	snapshot.TabID = diagnosticToken(snapshot.TabID, 100)
	snapshot.PageID = diagnosticToken(snapshot.PageID, 100)
	snapshot.Build = diagnosticToken(snapshot.Build, 100)
	snapshot.PaneID = diagnosticToken(snapshot.PaneID, 100)
	snapshot.Surface = diagnosticToken(snapshot.Surface, 24)
	snapshot.ViewMode = diagnosticToken(snapshot.ViewMode, 32)
	snapshot.Provider = diagnosticToken(snapshot.Provider, 32)
	snapshot.ThreadID = diagnosticToken(snapshot.ThreadID, 160)
	snapshot.SessionID = diagnosticToken(snapshot.SessionID, 100)
	snapshot.HostedThreadID = diagnosticToken(snapshot.HostedThreadID, 160)
	snapshot.Epoch = diagnosticToken(snapshot.Epoch, 160)
	snapshot.ActiveRenderSource = diagnosticToken(snapshot.ActiveRenderSource, 32)
	snapshot.Status = diagnosticToken(snapshot.Status, 16)
	snapshot.Fresh = false
	snapshot.AgeMS = 0
	if snapshot.Surface == "" {
		snapshot.Surface = "transcript"
	}
	if snapshot.SnapshotID == "" || snapshot.TabID == "" || snapshot.PaneID == "" ||
		(snapshot.Surface != "transcript" && snapshot.Surface != "sidebar") ||
		(snapshot.Status != "match" && snapshot.Status != "mismatch") ||
		len(snapshot.ExpectedRows) > maxTranscriptValidationRows || len(snapshot.MountedRows) > maxTranscriptValidationRows {
		return false
	}
	for index := range snapshot.ExpectedRows {
		if !normalizeTranscriptValidationRow(&snapshot.ExpectedRows[index], true) {
			return false
		}
	}
	for index := range snapshot.MountedRows {
		if !normalizeTranscriptValidationRow(&snapshot.MountedRows[index], false) {
			return false
		}
	}
	rawDuplicates, semanticDuplicates := duplicateIdentitySets(snapshot.MountedRows)
	snapshot.DuplicateRenderKeyCount = len(rawDuplicates)
	snapshot.DuplicateSemanticIDCount = len(semanticDuplicates)
	return true
}

func normalizeTranscriptValidationRow(row *transcriptValidationRow, expected bool) bool {
	row.Key = diagnosticToken(row.Key, 320)
	row.Source = diagnosticToken(row.Source, 16)
	row.ID = diagnosticToken(row.ID, 280)
	row.SemanticID = diagnosticToken(row.SemanticID, 280)
	row.Type = diagnosticToken(row.Type, 80)
	row.Status = diagnosticToken(row.Status, 80)
	row.Group = diagnosticToken(row.Group, 160)
	if row.Depth != nil && *row.Depth < 0 {
		*row.Depth = 0
	}
	if row.Key == "" || row.Source == "" || row.ID == "" || row.Type == "" || row.Key != row.Source+":"+row.ID {
		return false
	}
	if expected {
		row.HasContent = nil
	} else {
		row.ExpectsContent = nil
	}
	return true
}

func (s *server) handleTranscriptValidationRecord(w http.ResponseWriter, r *http.Request) {
	if s.transcriptValidations == nil {
		httpapi.WriteHTTPError(w, "transcript validations unavailable", http.StatusServiceUnavailable)
		return
	}
	defer r.Body.Close()
	var snapshot transcriptValidationSnapshot
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxTranscriptValidationBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		httpapi.WriteHTTPError(w, "invalid transcript validation", http.StatusBadRequest)
		return
	}
	if !normalizeTranscriptValidation(&snapshot) || !s.transcriptValidations.put(snapshot) {
		httpapi.WriteHTTPError(w, "invalid transcript validation", http.StatusBadRequest)
		return
	}
	response := map[string]any{"accepted": true}
	if observation, duplicated := deriveBrowserDuplicateIncident(snapshot, time.Now()); duplicated {
		if s.controlMetadata == nil || s.controlMetadataErr != "" {
			httpapi.WriteHTTPError(w, "durable browser duplicate store unavailable", http.StatusServiceUnavailable)
			return
		}
		incident, _, err := s.controlMetadata.ObserveBrowserDuplicateIncident(observation)
		if err != nil {
			httpapi.WriteHTTPError(w, "persist browser duplicate incident: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		// Inbox delivery is retried from the durable pending row. A relay
		// failure must not make the browser create a second observation.
		response["duplicateIncident"] = incident
	}
	httpapi.WriteJSON(w, response)
}

func (s *server) handleTranscriptValidationsList(w http.ResponseWriter, r *http.Request) {
	if s.transcriptValidations == nil {
		httpapi.WriteHTTPError(w, "transcript validations unavailable", http.StatusServiceUnavailable)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && status != "match" && status != "mismatch" {
		httpapi.WriteHTTPError(w, "invalid status", http.StatusBadRequest)
		return
	}
	latest, history := s.transcriptValidations.list(
		limit,
		r.URL.Query().Get("threadId"),
		r.URL.Query().Get("hostedThreadId"),
		r.URL.Query().Get("paneId"),
		r.URL.Query().Get("surface"),
		status,
	)
	response := map[string]any{"validations": latest, "history": history}
	if s.controlMetadata != nil && s.controlMetadataErr == "" {
		incidents, err := s.controlMetadata.ListBrowserDuplicateIncidents(browserDuplicateIncidentFilter{
			ThreadID: r.URL.Query().Get("threadId"), HostedThreadID: r.URL.Query().Get("hostedThreadId"),
			PaneID: r.URL.Query().Get("paneId"), Limit: limit,
		})
		if err == nil {
			response["duplicateIncidents"] = incidents
		}
	}
	httpapi.WriteJSON(w, response)
}

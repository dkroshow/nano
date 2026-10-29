package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/diagnostics"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/threadindex"
)

const (
	maxTranscriptAuditRequestBytes = 16 << 10
	maxTranscriptAuditResultBytes  = 96 << 10
	maxTranscriptAuditRows         = 200

	maxTranscriptAuditDifferences = 20
	maxTranscriptAuditPending     = 8
	maxTranscriptAuditHistory     = 100
	transcriptAuditTTL            = 30 * time.Second
)

var (
	errTranscriptAuditIdempotencyConflict = errors.New("transcript audit client action conflicts with an existing request")
	errTranscriptAuditPendingLimit        = errors.New("too many transcript audits are pending")
)

type transcriptAuditRequest struct {
	ID                     string                   `json:"auditId"`
	ClientActionID         string                   `json:"clientActionId"`
	ThreadID               string                   `json:"threadId"`
	TabID                  string                   `json:"tabId"`
	PageID                 string                   `json:"pageId"`
	PaneID                 string                   `json:"paneId"`
	Scopes                 []threadindex.AuditScope `json:"scopes"`
	Status                 string                   `json:"status"`
	RequestedAt            string                   `json:"requestedAt"`
	Deadline               string                   `json:"deadline"`
	ExpectedBuild          string                   `json:"expectedBuild,omitempty"`
	CatalogThreadID        string                   `json:"catalogThreadId"`
	ReferenceProjectionKey string                   `json:"referenceProjectionKey,omitempty"`
	ReferenceSnapshotHead  int64                    `json:"referenceSnapshotHeadSeq,omitempty"`
	ReferenceState         string                   `json:"referenceState,omitempty"`
	Result                 *transcriptAuditResult   `json:"result,omitempty"`
	requestedAt            time.Time
	deadline               time.Time
}

// AuditEvent identifies the browser pane requested to capture an audit.
type AuditEvent struct {
	AuditID         string                   `json:"auditId"`
	ThreadID        string                   `json:"threadId"`
	CatalogThreadID string                   `json:"catalogThreadId"`
	TabID           string                   `json:"tabId"`
	PageID          string                   `json:"pageId"`
	PaneID          string                   `json:"paneId"`
	Scopes          []threadindex.AuditScope `json:"scopes"`
	Deadline        string                   `json:"deadline"`
}

type transcriptAuditDifference struct {
	Kind           string `json:"kind"`
	ID             string `json:"id,omitempty"`
	Field          string `json:"field,omitempty"`
	ExpectedIndex  int    `json:"expectedIndex,omitempty"`
	BrowserIndex   int    `json:"browserIndex,omitempty"`
	BrowserCount   int    `json:"browserCount,omitempty"`
	ExpectedHash   string `json:"expectedHash,omitempty"`
	BrowserHash    string `json:"browserHash,omitempty"`
	ExpectedLength int    `json:"expectedLength,omitempty"`
	BrowserLength  int    `json:"browserLength,omitempty"`
}

type transcriptAuditExclusion struct {
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

type transcriptAuditCounts struct {
	Reference      int `json:"reference"`
	Browser        int `json:"browser"`
	ContentChecked int `json:"contentChecked"`
	Missing        int `json:"missing"`
	Extra          int `json:"extra"`
	Duplicate      int `json:"duplicate"`
	Reordered      int `json:"reordered"`
	Changed        int `json:"changed"`
	NotChecked     int `json:"notChecked"`
}

type transcriptAuditRange struct {
	StartSeq        int64 `json:"startSeq"`
	EndSeq          int64 `json:"endSeq"`
	SnapshotHeadSeq int64 `json:"snapshotHeadSeq"`
	Partial         bool  `json:"partial"`
}

type transcriptAuditResult struct {
	Status               string                      `json:"status"`
	Complete             bool                        `json:"complete"`
	Scopes               []threadindex.AuditScope    `json:"scopes"`
	Counts               transcriptAuditCounts       `json:"counts"`
	Differences          []transcriptAuditDifference `json:"differences"`
	DifferencesTruncated bool                        `json:"differencesTruncated"`
	Exclusions           []transcriptAuditExclusion  `json:"exclusions"`
	Reasons              []string                    `json:"reasons"`
	Range                transcriptAuditRange        `json:"range"`
	CapturedAt           string                      `json:"capturedAt,omitempty"`
	PageID               string                      `json:"pageId,omitempty"`
	ViewMode             string                      `json:"viewMode,omitempty"`
	Build                string                      `json:"build,omitempty"`
}

// AuditStore retains bounded pending and completed browser audits.
type AuditStore struct {
	mu       sync.Mutex
	order    []string
	requests map[string]*transcriptAuditRequest
	now      func() time.Time
}

// NewAuditStore creates the bounded audit record owner.
func NewAuditStore() *AuditStore {
	return &AuditStore{requests: make(map[string]*transcriptAuditRequest), now: time.Now}
}

func (s *AuditStore) create(request transcriptAuditRequest) (transcriptAuditRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	for _, id := range s.order {
		candidate := s.requests[id]
		if candidate == nil || candidate.ClientActionID != request.ClientActionID {
			continue
		}
		if candidate.ThreadID != request.ThreadID || candidate.CatalogThreadID != request.CatalogThreadID || candidate.TabID != request.TabID || candidate.PageID != request.PageID || candidate.PaneID != request.PaneID || !slices.Equal(candidate.Scopes, request.Scopes) {
			return transcriptAuditRequest{}, errTranscriptAuditIdempotencyConflict
		}
		return *candidate, nil
	}
	pending := 0
	for _, request := range s.requests {
		if request.Status == "pending" {
			pending++
		}
	}
	if pending >= maxTranscriptAuditPending {
		return transcriptAuditRequest{}, errTranscriptAuditPendingLimit
	}
	copy := request
	s.requests[copy.ID] = &copy
	s.order = append(s.order, copy.ID)
	for len(s.order) > maxTranscriptAuditHistory {
		delete(s.requests, s.order[0])
		s.order = s.order[1:]
	}
	return copy, nil
}

func (s *AuditStore) get(id string) (transcriptAuditRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	request, ok := s.requests[id]
	if !ok {
		return transcriptAuditRequest{}, false
	}
	return *request, true
}

func (s *AuditStore) finish(id string, result transcriptAuditResult) (transcriptAuditRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	request, ok := s.requests[id]
	if !ok {
		return transcriptAuditRequest{}, false
	}
	if request.Status == "pending" {
		request.Status = result.Status
		request.Result = &result
	}
	return *request, true
}

func (s *AuditStore) expireLocked() {
	now := s.now().UTC()
	for _, request := range s.requests {
		if request.Status != "pending" || now.Before(request.deadline) {
			continue
		}
		result := inconclusiveTranscriptAudit(request.Scopes, "browser_capture_missing")
		request.Status = result.Status
		request.Result = &result
	}
}

// AuditHub publishes audit requests for the browser event stream.
type AuditHub struct {
	mu   sync.Mutex
	subs map[chan AuditEvent]struct{}
}

// NewAuditHub creates an audit notification hub.
func NewAuditHub() *AuditHub {
	return &AuditHub{subs: make(map[chan AuditEvent]struct{})}
}

// Subscribe registers a browser stream for audit requests.
func (h *AuditHub) Subscribe() chan AuditEvent {
	ch := make(chan AuditEvent, maxTranscriptAuditPending)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// Unsubscribe releases a browser audit subscription.
func (h *AuditHub) Unsubscribe(ch chan AuditEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

func (h *AuditHub) notify(event AuditEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- event:
		default:
		}
	}
}

type transcriptAuditCatalog interface {
	LoadTranscript(context.Context, string, threadindex.TranscriptQuery) (threadindex.TranscriptResponse, error)
	LoadTranscriptAuditReference(context.Context, string, string, int64, int64, int64, []threadindex.AuditScope) (threadindex.AuditReference, error)
}

func normalizeTranscriptAuditScopes(scopes []threadindex.AuditScope) ([]threadindex.AuditScope, bool) {
	for _, scope := range scopes {
		if scope != threadindex.AuditMessages {
			return nil, false
		}
	}
	return []threadindex.AuditScope{threadindex.AuditMessages}, true
}

func (s *AuditHandlers) HandleTranscriptAuditCreate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	if s.transcriptAudits == nil || s.AuditEvents == nil {
		httpapi.WriteHTTPError(w, "transcript audits unavailable", http.StatusServiceUnavailable)
		return
	}
	catalog, ok := s.catalog.(transcriptAuditCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteHTTPError(w, "transcript catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	defer r.Body.Close()
	var body struct {
		ClientActionID string                   `json:"clientActionId"`
		ThreadRef      string                   `json:"threadRef"`
		TabID          string                   `json:"tabId"`
		PageID         string                   `json:"pageId"`
		PaneID         string                   `json:"paneId"`
		Scopes         []threadindex.AuditScope `json:"scopes"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxTranscriptAuditRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		httpapi.WriteHTTPError(w, "invalid transcript audit request", http.StatusBadRequest)
		return
	}
	body.ClientActionID = diagnostics.Token(body.ClientActionID, 160)
	body.ThreadRef = diagnostics.Token(body.ThreadRef, 200)
	body.TabID = diagnostics.Token(body.TabID, 100)
	body.PageID = diagnostics.Token(body.PageID, 100)
	body.PaneID = diagnostics.Token(body.PaneID, 100)
	scopes, validScopes := normalizeTranscriptAuditScopes(body.Scopes)
	if body.ClientActionID == "" || body.ThreadRef == "" || body.TabID == "" || body.PageID == "" || body.PaneID == "" || !validScopes {
		httpapi.WriteHTTPError(w, "invalid transcript audit request", http.StatusBadRequest)
		return
	}
	id, err := s.resolveThreadReference(r.Context(), body.ThreadRef)
	if err != nil {
		threadindex.WriteThreadReferenceError(w, err)
		return
	}
	pin, err := catalog.LoadTranscript(r.Context(), id, threadindex.TranscriptQuery{Limit: 1, View: threadindex.DevThreadTranscriptView, HiddenSpecified: true})
	if err != nil {
		threadindex.WriteTranscriptBackendError(w, err)
		return
	}
	now := time.Now().UTC()
	request := transcriptAuditRequest{
		ID: "tau_" + auditID(), ClientActionID: body.ClientActionID, ThreadID: body.ThreadRef,
		TabID: body.TabID, PageID: body.PageID, PaneID: body.PaneID, Scopes: scopes, Status: "pending",
		RequestedAt: now.Format(time.RFC3339Nano), Deadline: now.Add(transcriptAuditTTL).Format(time.RFC3339Nano),
		ExpectedBuild: s.buildName, ReferenceProjectionKey: pin.Projection.Key,
		ReferenceSnapshotHead: pin.Page.SnapshotHeadSeq, ReferenceState: pin.Projection.State,
		CatalogThreadID: id, requestedAt: now, deadline: now.Add(transcriptAuditTTL),
	}
	if request.ReferenceProjectionKey == "" || (request.ReferenceState != "ready" && request.ReferenceState != "retained") {
		result := inconclusiveTranscriptAudit(scopes, "saved_reference_unavailable")
		request.Status, request.Result = result.Status, &result
	}
	stored, err := s.transcriptAudits.create(request)
	if err != nil {
		status := http.StatusTooManyRequests
		if errors.Is(err, errTranscriptAuditIdempotencyConflict) {
			status = http.StatusConflict
		}
		httpapi.WriteHTTPError(w, err.Error(), status)
		return
	}
	if stored.Status == "pending" {
		s.AuditEvents.notify(AuditEvent{AuditID: stored.ID, ThreadID: stored.ThreadID, CatalogThreadID: stored.CatalogThreadID, TabID: stored.TabID, PageID: stored.PageID, PaneID: stored.PaneID, Scopes: stored.Scopes, Deadline: stored.Deadline})
	}
	httpapi.WriteJSON(w, stored)
}

func (s *AuditHandlers) HandleTranscriptAuditRead(w http.ResponseWriter, _ *http.Request, id string) {
	if s.transcriptAudits == nil {
		httpapi.WriteHTTPError(w, "transcript audits unavailable", http.StatusServiceUnavailable)
		return
	}
	request, ok := s.transcriptAudits.get(diagnostics.Token(id, 200))
	if !ok {
		httpapi.WriteHTTPError(w, "transcript audit not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpapi.WriteJSON(w, request)
}

func (s *AuditHandlers) HandleTranscriptAuditReference(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Cache-Control", "private, no-store")
	if s.transcriptAudits == nil {
		httpapi.WriteHTTPError(w, "transcript audits unavailable", http.StatusServiceUnavailable)
		return
	}
	catalog, ok := s.catalog.(transcriptAuditCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteHTTPError(w, "transcript catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	request, found := s.transcriptAudits.get(diagnostics.Token(id, 200))
	if !found {
		httpapi.WriteHTTPError(w, "transcript audit not found", http.StatusNotFound)
		return
	}
	if request.Status != "pending" {
		httpapi.WriteHTTPError(w, "transcript audit is not pending", http.StatusConflict)
		return
	}
	minSeq, minErr := parseOptionalTranscriptAuditSeq(r.URL.Query().Get("minProjectedSeq"))
	maxSeq, maxErr := parseOptionalTranscriptAuditSeq(r.URL.Query().Get("maxProjectedSeq"))
	if minErr != nil || maxErr != nil || (minSeq > 0 && maxSeq > 0 && minSeq > maxSeq) {
		httpapi.WriteHTTPError(w, "invalid transcript audit range", http.StatusBadRequest)
		return
	}
	reference, err := catalog.LoadTranscriptAuditReference(r.Context(), request.CatalogThreadID, request.ReferenceProjectionKey, request.ReferenceSnapshotHead, minSeq, maxSeq, request.Scopes)
	if err != nil {
		if errors.Is(err, history.ErrTranscriptChanged) || errors.Is(err, threadindex.ErrAuditReferenceChanged) {
			result := inconclusiveTranscriptAudit(request.Scopes, "saved_reference_changed")
			s.transcriptAudits.finish(request.ID, result)
			httpapi.WriteHTTPError(w, "transcript audit reference changed", http.StatusConflict)
			return
		}
		threadindex.WriteTranscriptBackendError(w, err)
		return
	}
	httpapi.WriteJSON(w, reference)
}

func (s *AuditHandlers) HandleTranscriptAuditResult(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Cache-Control", "private, no-store")
	if s.transcriptAudits == nil {
		httpapi.WriteHTTPError(w, "transcript audits unavailable", http.StatusServiceUnavailable)
		return
	}
	request, found := s.transcriptAudits.get(diagnostics.Token(id, 200))
	if !found {
		httpapi.WriteHTTPError(w, "transcript audit not found", http.StatusNotFound)
		return
	}
	if request.Status != "pending" {
		httpapi.WriteJSON(w, request)
		return
	}
	defer r.Body.Close()
	var body struct {
		CapturedAt      string                `json:"capturedAt"`
		TabID           string                `json:"tabId"`
		PageID          string                `json:"pageId"`
		Build           string                `json:"build"`
		PaneID          string                `json:"paneId"`
		LegacyThreadID  string                `json:"threadId"`        // accepted from an older bundle; never trusted
		CatalogThreadID string                `json:"catalogThreadId"` // accepted from an older bundle; never trusted
		PaneThreadRef   string                `json:"paneThreadRef"`
		ViewMode        string                `json:"viewMode"`
		PageVisible     bool                  `json:"pageVisible"`
		SurfaceVisible  bool                  `json:"surfaceVisible"`
		ProjectionKey   string                `json:"projectionKey"`
		SnapshotHeadSeq int64                 `json:"snapshotHeadSeq"`
		Result          transcriptAuditResult `json:"result"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxTranscriptAuditResultBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		httpapi.WriteHTTPError(w, "invalid transcript audit result", http.StatusBadRequest)
		return
	}
	result, ok := normalizeTranscriptAuditResult(body.Result, request.Scopes)
	if !ok {
		httpapi.WriteHTTPError(w, "invalid transcript audit result", http.StatusBadRequest)
		return
	}
	result.CapturedAt = diagnostics.Time(body.CapturedAt)
	result.PageID = diagnostics.Token(body.PageID, 100)
	result.ViewMode = diagnostics.Token(body.ViewMode, 32)
	result.Build = diagnostics.Token(body.Build, 100)
	capturedAt, capturedErr := time.Parse(time.RFC3339Nano, result.CapturedAt)
	paneThreadID, paneThreadErr := s.resolveThreadReference(r.Context(), diagnostics.Token(body.PaneThreadRef, 200))
	switch {
	case diagnostics.Token(body.TabID, 100) != request.TabID || diagnostics.Token(body.PageID, 100) != request.PageID || diagnostics.Token(body.PaneID, 100) != request.PaneID:
		result = inconclusiveTranscriptAudit(request.Scopes, "wrong_browser_pane")
	case body.PaneThreadRef == "" || paneThreadErr != nil || paneThreadID != request.CatalogThreadID:
		result = inconclusiveTranscriptAudit(request.Scopes, "wrong_thread")
	case request.ExpectedBuild == "" || result.Build != request.ExpectedBuild:
		result = inconclusiveTranscriptAudit(request.Scopes, "wrong_or_stale_build")
	case !body.PageVisible || !body.SurfaceVisible:
		result = inconclusiveTranscriptAudit(request.Scopes, "browser_pane_not_visible")
	case capturedErr != nil || capturedAt.Before(request.requestedAt) || capturedAt.After(request.deadline):
		result = inconclusiveTranscriptAudit(request.Scopes, "stale_browser_capture")
	case diagnostics.Token(body.ProjectionKey, 200) != request.ReferenceProjectionKey || body.SnapshotHeadSeq != request.ReferenceSnapshotHead:
		result = inconclusiveTranscriptAudit(request.Scopes, "saved_reference_changed")
	}
	result.CapturedAt = diagnostics.Time(body.CapturedAt)
	result.PageID = diagnostics.Token(body.PageID, 100)
	result.ViewMode = diagnostics.Token(body.ViewMode, 32)
	result.Build = diagnostics.Token(body.Build, 100)
	finished, _ := s.transcriptAudits.finish(request.ID, result)
	httpapi.WriteJSON(w, finished)
}

func normalizeTranscriptAuditResult(result transcriptAuditResult, expectedScopes []threadindex.AuditScope) (transcriptAuditResult, bool) {
	if result.Status != "match" && result.Status != "mismatch" && result.Status != "inconclusive" {
		return result, false
	}
	scopes, ok := normalizeTranscriptAuditScopes(result.Scopes)
	if !ok || !slices.Equal(scopes, expectedScopes) || len(result.Differences) > maxTranscriptAuditDifferences || len(result.Exclusions) > maxTranscriptAuditRows || len(result.Reasons) > 20 {
		return result, false
	}
	counts := result.Counts
	if counts.Reference < 0 || counts.Browser < 0 || counts.ContentChecked < 0 || counts.Missing < 0 || counts.Extra < 0 || counts.Duplicate < 0 || counts.Reordered < 0 || counts.Changed < 0 || counts.NotChecked < 0 || result.Range.StartSeq < 0 || result.Range.EndSeq < 0 || result.Range.SnapshotHeadSeq < 0 {
		return result, false
	}
	result.Scopes = scopes
	allowedDifferences := map[string]bool{"missing": true, "extra": true, "duplicate": true, "reordered": true, "changed": true, "structured_changed": true}
	for index := range result.Differences {
		diff := &result.Differences[index]
		diff.Kind = diagnostics.Token(diff.Kind, 32)
		diff.ID = diagnostics.Token(diff.ID, 280)
		diff.Field = diagnostics.Token(diff.Field, 32)
		diff.ExpectedHash = diagnostics.Token(diff.ExpectedHash, 80)
		diff.BrowserHash = diagnostics.Token(diff.BrowserHash, 80)
		if !allowedDifferences[diff.Kind] || diff.ExpectedLength < 0 || diff.BrowserLength < 0 || diff.BrowserCount < 0 {
			return result, false
		}
	}
	for index := range result.Exclusions {
		result.Exclusions[index].Scope = diagnostics.Token(result.Exclusions[index].Scope, 32)
		result.Exclusions[index].Reason = diagnostics.Token(result.Exclusions[index].Reason, 80)
		if result.Exclusions[index].Reason == "" || result.Exclusions[index].Count < 0 {
			return result, false
		}
	}
	for index := range result.Reasons {
		result.Reasons[index] = diagnostics.Token(result.Reasons[index], 80)
		if result.Reasons[index] == "" {
			return result, false
		}
	}
	incomplete := len(result.Exclusions) > 0 || len(result.Reasons) > 0
	if result.Complete == incomplete {
		return result, false
	}
	if len(result.Differences) > 0 || result.DifferencesTruncated {
		if result.Status != "mismatch" {
			return result, false
		}
	} else if incomplete {
		if result.Status != "inconclusive" {
			return result, false
		}
	} else if result.Status != "match" {
		return result, false
	}
	return result, true
}

func parseOptionalTranscriptAuditSeq(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seq < 0 {
		return 0, errors.New("invalid transcript audit sequence")
	}
	return seq, nil
}

func inconclusiveTranscriptAudit(scopes []threadindex.AuditScope, reason string) transcriptAuditResult {
	return transcriptAuditResult{Status: "inconclusive", Scopes: append([]threadindex.AuditScope(nil), scopes...), Differences: []transcriptAuditDifference{}, Exclusions: []transcriptAuditExclusion{}, Reasons: []string{reason}}
}

// WriteTranscriptAuditEvent writes one content-free browser SSE request.
func WriteTranscriptAuditEvent(w io.Writer, event AuditEvent) {
	payload, err := json.Marshal(event)
	if err == nil {
		fmt.Fprintf(w, "event: transcript-audit\ndata: %s\n\n", payload)
	}
}

// AuditHandlers coordinates bounded browser audits against saved transcript references.
type AuditHandlers struct {
	catalog          transcriptAuditCatalog
	transcriptAudits *AuditStore
	AuditEvents      *AuditHub
	buildName        string
}

// NewAuditHandlers binds browser audits to the concrete saved transcript owner.
func NewAuditHandlers(catalog *threadindex.Service, store *AuditStore, events *AuditHub, buildName string) *AuditHandlers {
	h := &AuditHandlers{transcriptAudits: store, AuditEvents: events, buildName: buildName}
	if catalog != nil {
		h.catalog = catalog
	}
	return h
}
func (s *AuditHandlers) resolveThreadReference(ctx context.Context, ref string) (string, error) {
	resolver, _ := s.catalog.(threadindex.ReferenceResolver)
	return threadindex.ResolveReference(ctx, resolver, ref)
}
func auditID() string { b := make([]byte, 4); rand.Read(b); return hex.EncodeToString(b) }

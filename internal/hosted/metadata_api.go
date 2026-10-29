package hosted

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

func (s *ViewService) canonicalMetadataAvailable(w http.ResponseWriter) bool {
	if s.Metadata == nil || s.MetadataError != "" {
		message := "canonical metadata unavailable"
		if s.MetadataError != "" {
			message += ": " + s.MetadataError
		}
		httpapi.WriteHTTPError(w, message, http.StatusServiceUnavailable)
		return false
	}
	return true
}

// HandleMetadataRead serves canonical metadata with the current attachment overlay.
func (s *ViewService) HandleMetadataRead(w http.ResponseWriter, r *http.Request, record ownedthreads.SessionRecord) {
	if !s.canonicalMetadataAvailable(w) {
		return
	}
	view, err := s.Metadata.View(record.ID)
	if err == sql.ErrNoRows {
		httpapi.WriteNotFound(w)
		return
	}
	if err != nil {
		httpapi.WriteHTTPError(w, "canonical metadata unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	httpapi.WriteJSON(w, s.DecorateMetadata(view, s.AttachmentState(record.ID)))
}

// HandleMetadataChanges serves revision hints and identifies a required full refetch.
func (s *ViewService) HandleMetadataChanges(w http.ResponseWriter, r *http.Request, record ownedthreads.SessionRecord) {
	if !s.canonicalMetadataAvailable(w) {
		return
	}
	rawAfter := r.URL.Query().Get("afterRevision")
	after := int64(0)
	var parseErr error
	if rawAfter != "" {
		after, parseErr = strconv.ParseInt(rawAfter, 10, 64)
	}
	if parseErr != nil || after < 0 {
		httpapi.WriteHTTPError(w, "afterRevision must be a non-negative integer", http.StatusBadRequest)
		return
	}
	view, err := s.Metadata.View(record.ID)
	if err == sql.ErrNoRows {
		httpapi.WriteNotFound(w)
		return
	}
	if err != nil {
		httpapi.WriteHTTPError(w, "canonical metadata unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	changes := s.Metadata.ChangesAfter(record.ID, after)
	oldest := view.Revision
	if len(changes) > 0 {
		oldest = changes[0].Revision
	}
	refetch := after > view.Revision || (after < view.Revision && (len(changes) == 0 || after < oldest-1))
	httpapi.WriteJSON(w, map[string]any{"changes": changes, "currentRevision": view.Revision, "oldestRetainedRevision": oldest, "refetchRequired": refetch})
	return
}

// HandleMetadataEvents serves bounded canonical evidence after an event cursor.
func (s *ViewService) HandleMetadataEvents(w http.ResponseWriter, r *http.Request, record ownedthreads.SessionRecord) {
	if !s.canonicalMetadataAvailable(w) {
		return
	}
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if r.URL.Query().Get("after") == "" {
		after = 0
		err = nil
	}
	limit, limitErr := strconv.Atoi(r.URL.Query().Get("limit"))
	if r.URL.Query().Get("limit") == "" {
		limit = 100
		limitErr = nil
	}
	filter := hostedstore.MetadataEventFilter{
		Field:       strings.TrimSpace(r.URL.Query().Get("field")),
		Source:      strings.TrimSpace(r.URL.Query().Get("source")),
		Disposition: strings.TrimSpace(r.URL.Query().Get("disposition")),
		Freshness:   strings.TrimSpace(r.URL.Query().Get("freshness")),
	}
	if err != nil || after < 0 || limitErr != nil || limit < 1 || limit > 200 || !validMetadataEventFilter(filter) {
		httpapi.WriteHTTPError(w, "after/limit or metadata event filters are invalid", http.StatusBadRequest)
		return
	}
	if _, err := s.Metadata.View(record.ID); err == sql.ErrNoRows {
		httpapi.WriteNotFound(w)
		return
	} else if err != nil {
		httpapi.WriteHTTPError(w, "canonical metadata unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	page, err := s.Metadata.EventsAfter(record.ID, after, limit, filter)
	if err != nil {
		httpapi.WriteHTTPError(w, "canonical metadata unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	httpapi.WriteJSON(w, page)
	return
}

// HandlePermissionEvents serves bounded provider permission evidence.
func (s *ViewService) HandlePermissionEvents(w http.ResponseWriter, r *http.Request, record ownedthreads.SessionRecord) {
	if s.Metadata == nil || s.MetadataError != "" {
		httpapi.WriteHTTPError(w, "canonical metadata unavailable", http.StatusServiceUnavailable)
		return
	}
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if r.URL.Query().Get("after") == "" {
		after = 0
		err = nil
	}
	limit, limitErr := strconv.Atoi(r.URL.Query().Get("limit"))
	if r.URL.Query().Get("limit") == "" {
		limit = 100
		limitErr = nil
	}
	if err != nil || after < 0 || limitErr != nil || limit < 1 || limit > 200 {
		httpapi.WriteHTTPError(w, "after and limit must be valid non-negative pagination values", http.StatusBadRequest)
		return
	}
	page, err := s.Metadata.PermissionEventsAfter(record.ID, after, limit)
	if err != nil {
		httpapi.WriteHTTPError(w, "permission events unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	httpapi.WriteJSON(w, page)
}

// HandleFamily serves durable invocation lineage and its retained operations.
func (s *ViewService) HandleFamily(w http.ResponseWriter, _ *http.Request, record ownedthreads.SessionRecord) {
	ancestors, descendants := s.Executions.LineageForHosted(record.ID)
	memberIDs := append([]string{record.ID}, descendants...)
	members := make([]executionFamilyMemberView, 0, len(memberIDs))
	for _, memberID := range memberIDs {
		member, found, err := s.Executions.SessionFromCanonical(memberID)
		if err != nil {
			httpapi.WriteHTTPError(w, "durable family lineage unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if !found {
			httpapi.WriteHTTPError(w, "durable family lineage is incomplete", http.StatusServiceUnavailable)
			return
		}
		members = append(members, executionFamilyMemberView{HostedThreadID: member.ID, Provider: member.Provider,
			NativeThreadID: member.NativeThreadID, InvokerHostedThreadID: member.InvokerHostedThreadID,
			Status: member.Status, OperationIDs: s.Executions.OperationIDsForHosted(member.ID)})
	}
	httpapi.WriteJSON(w, map[string]any{"rootHostedThreadId": record.ID, "ancestorHostedThreadIds": ancestors, "members": members})
}

func validMetadataEventFilter(filter hostedstore.MetadataEventFilter) bool {
	if filter.Field != "" && filter.Field != hostedstore.PostureFieldModel && filter.Field != hostedstore.PostureFieldEffort && filter.Field != hostedstore.PostureFieldPermissionMode && filter.Field != hostedstore.PostureFieldServiceTier && filter.Field != "cwd" && filter.Field != "subscriptions" && filter.Field != hostedstore.AssignmentField {
		return false
	}
	if filter.Source != "" && filter.Source != hostedstore.PostureSourceCreation && filter.Source != hostedstore.PostureSourceOperator && filter.Source != hostedstore.PostureSourceProvider && filter.Source != hostedstore.SourceMigration {
		return false
	}
	if filter.Disposition != "" && filter.Disposition != hostedstore.DispositionRequested && filter.Disposition != hostedstore.DispositionConfirmed && filter.Disposition != hostedstore.DispositionRejected && filter.Disposition != hostedstore.DispositionCleared && filter.Disposition != hostedstore.DispositionObserved {
		return false
	}
	return filter.Freshness == "" || filter.Freshness == hostedstore.FreshnessLive || filter.Freshness == hostedstore.FreshnessHistorical || filter.Freshness == hostedstore.FreshnessReplayed
}

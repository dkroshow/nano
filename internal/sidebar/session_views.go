package sidebar

import (
	"database/sql"
	"encoding/json"
	"sort"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	sessionruntime "github.com/dkroshow/nano/internal/sessions"
	threadvalues "github.com/dkroshow/nano/internal/threads"
)

// ListedSession is a live attachment with its compact displayed metadata.
type ListedSession struct {
	sessionruntime.Summary
	Metadata *listedMetadata `json:"metadata,omitempty"`
}

type listedMetadata struct {
	Revision int64                          `json:"revision"`
	Fields   map[string]listedMetadataField `json:"fields"`
}

type listedMetadataField struct {
	DisplayValue json.RawMessage `json:"displayValue,omitempty"`
}

// listMetadata projects only the metadata displayed by a session list.
func listMetadata(view *hostedstore.MetadataView) *listedMetadata {
	if view == nil {
		return nil
	}
	fields := make(map[string]listedMetadataField, len(view.Fields))
	for name, field := range view.Fields {
		fields[name] = listedMetadataField{DisplayValue: field.DisplayValue}
	}
	return &listedMetadata{Revision: view.Revision, Fields: fields}
}

// DetachedSessionView keeps the legacy attachment shape required to locate a
// resumable provider transport, while exposing hosted facts only through the
// listed metadata live sessions carry; the full view is a thread's own read.
// The embedded record is deliberately scrubbed at response construction; old
// JSON cwd/model/tier values are migration input, not API truth.
type DetachedSessionView struct {
	hostedstore.Attachment
	// Status is "starting" while the boot loop has not registered the thread.
	Status              string          `json:"status,omitempty"`
	Metadata            *listedMetadata `json:"metadata,omitempty"`
	MetadataUnavailable bool            `json:"metadataUnavailable,omitempty"`
}

// ListedSessions is the session list's rows for sessions, oldest first. The
// list read and the runtime event that names joined sessions share it, so a
// browser holds the same row either way. Every per-session fact is read in
// bounded batches, never once per session: views, attention, badges and names.
func (s *Controller) ListedSessions(sessions []*sessionruntime.Session) []ListedSession {
	var summaryDB *sql.DB
	if s.controlMetadata != nil {
		summaryDB = s.controlMetadata.ReadPool()
	}
	hostedIDs := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		if sess.HostedThreadID() != "" {
			hostedIDs = append(hostedIDs, sess.HostedThreadID())
		}
	}
	views, viewsErr := s.controlMetadata.ViewsFrom(summaryDB, hostedIDs)
	needsAttention := s.agentExecutions.SessionsNeedingAttention()
	summaries := make([]sessionruntime.Summary, 0, len(sessions))
	for _, sess := range sessions {
		view, found := views[sess.HostedThreadID()]
		viewErr := viewsErr
		if viewErr == nil && !found {
			viewErr = sql.ErrNoRows
		}
		sum := sess.View().SummaryWith(view, viewErr, sess.HostedThreadID() != "" && needsAttention[sess.HostedThreadID()])
		summaries = append(summaries, sum)
	}
	s.attachStarredOnSessions(summaries)
	sort.Slice(summaries, func(i, j int) bool {
		return threadvalues.TimestampBefore(summaries[i].CreatedAt, summaries[j].CreatedAt)
	})
	listed := make([]ListedSession, len(summaries))
	for i, sum := range summaries {
		listed[i] = ListedSession{Summary: sum, Metadata: listMetadata(sum.Metadata)}
	}
	return listed
}

// DetachedSessionViews lists the attachments whose thread has no live
// session. Right after a restart these include threads the boot loop has not
// reached yet (boot_reattach.go), marked "starting"; the rest are records
// whose resume failed (daemon down, thread gone), offered as one-click
// re-attachable. Absent the store, the list is empty. Kept out of s.sessions
// by design, so they carry no runtime overlay in the Explorer.
func (s *Controller) DetachedSessionViews(live map[hostedstore.AttachmentKey]bool) []DetachedSessionView {
	var records []hostedstore.Attachment
	if s.attachments != nil {
		records = s.attachments.DetachedExcept(live)
	}
	return s.detachedViewsOf(records)
}

// DetachedViewsOf is the list rows of these detached records.
func (s *Controller) detachedViewsOf(records []hostedstore.Attachment) []DetachedSessionView {
	var summaryDB *sql.DB
	if s.controlMetadata != nil {
		summaryDB = s.controlMetadata.ReadPool()
	}
	detachedIDs := make([]string, 0, len(records))
	for _, record := range records {
		if record.HostedThreadID != "" {
			detachedIDs = append(detachedIDs, record.HostedThreadID)
		}
	}
	detachedViews, detachedErr := s.controlMetadata.ViewsFrom(summaryDB, detachedIDs)
	detached := []DetachedSessionView{}
	for _, record := range records {
		view := DetachedSessionView{Attachment: record}
		if state := s.boot.State(record.Key()); state != "" {
			view.Status = state
		}
		// These retired JSON fields must never leak back into a hosted
		// response as a plausible current setting.
		view.Cwd, view.Model, view.TurnServiceTier = "", "", ""
		if record.HostedThreadID == "" || s.controlMetadata == nil || s.controlMetadataErr != "" {
			view.MetadataUnavailable = record.HostedThreadID != ""
		} else if metadata, ok := detachedViews[record.HostedThreadID]; ok && detachedErr == nil {
			view.Metadata = listMetadata(&metadata)
		} else {
			view.MetadataUnavailable = true
		}
		detached = append(detached, view)
	}
	return detached
}

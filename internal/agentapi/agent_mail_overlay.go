// agent_mail_overlay.go — the operator-facing surface over the durable agent
// inbox. The store (agent_messages.go) remains the authority; this
// file adds what the browser UI needs on top of it:
//
//   - a `mail` read-time overlay on catalog rows, stamped at response time like
//     Assignment/Starred, never stored in the catalog and never part
//     of catalog_version;
//   - a `mail` named event on the thread-index SSE stream so sidebar badges and
//     open inbox panels refresh through the same push path everything else
//     uses;
//   - the operator's cross-thread listing (GET /api/v1/agent-messages), which
//     is read-only and never marks mail seen.
//
// The operator on the trusted local listener can also send mail as an
// "operator" sender and edit an operator message while it is still unread —
// both wired through agent_messages_api.go, which calls notifyMailChanged here.
package agentapi

import (
	"log"
	"net/http"
	"strconv"
	"strings"

	featuremail "github.com/dkroshow/nano/internal/features/mail"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/interact"
)

const (
	operatorSenderKey  = "operator"
	operatorSenderKind = "operator"
	operatorSenderName = "Operator"
)

// notifyMailChanged recomputes counts for the affected recipients plus the
// global summary and fans them to SSE subscribers. Best-effort by design: a
// failed read logs and drops the event — the durable store already committed,
// and the client's next full fetch repairs any missed badge.
// NotifyMailChanged refreshes unread notices and publishes committed inbox counts.
func (s *Controller) notifyMailChanged(recipients ...string) {
	if s == nil || s.mail == nil {
		return
	}
	ids := make([]string, 0, len(recipients))
	seen := map[string]bool{}
	for _, id := range recipients {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	for _, id := range ids {
		if err := refreshMailNotice(s.mail, s.notices, id); err != nil {
			log.Printf("mail notice refresh for %s: %v", id, err)
		}
	}
	if s.mailEvents == nil {
		return
	}
	all, err := s.mail.CountsFor(nil)
	if err != nil {
		log.Printf("mail change notify: %v", err)
		return
	}
	delta := featuremail.Delta{Counts: map[string]featuremail.Counts{}}
	for _, id := range ids {
		delta.Counts[id] = all[id] // zero-value counts if the id has no mail
	}
	for _, counts := range all {
		delta.Summary.Total += counts.Total
		delta.Summary.Unread += counts.Unread
		delta.Summary.Seen += counts.Seen
	}
	if s.sidebar != nil {
		s.sidebar.ApplyMailDelta(delta)
	}
	s.mailEvents.Notify(delta)
}

func refreshMailNotice(mail *featuremail.Store, notices *interact.Notices, recipient string) error {
	count, _, err := mail.UnreadCount(recipient)
	if err != nil {
		return err
	}
	return featuremail.SyncUnreadNotice(notices, recipient, count)
}

// SyncMailNotices reconstructs unread notices from the durable inbox before HTTP starts.
func SyncMailNotices(mail *featuremail.Store, notices *interact.Notices) error {
	if mail == nil || notices == nil {
		return nil
	}
	counts, err := mail.CountsFor(nil)
	if err != nil {
		return err
	}
	for recipient := range counts {
		if err := refreshMailNotice(mail, notices, recipient); err != nil {
			return err
		}
	}
	return nil
}

// handleAgentMessagesGlobal serves the operator's cross-thread mail view:
// newest first across every recipient, filterable by status and ref, paged by
// the durable global message order. Read-only — never marks mail seen.
// HandleAgentMessagesGlobal serves the operator cross-thread inbox view without marking mail read.
func (s *Controller) HandleAgentMessagesGlobal(w http.ResponseWriter, r *http.Request) {
	if !s.agentInboxAvailable(w) {
		return
	}
	opts, err := parseAgentMessageListOptions(r)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_mail_filter", err.Error())
		return
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("before")); raw != "" {
		value, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || value < 0 {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_mail_filter", "before must be a non-negative integer")
			return
		}
		opts.Since = value
	}
	items, err := s.mail.ListAll(opts)
	if err != nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "mail_read_failed", err.Error())
		return
	}
	all, err := s.mail.CountsFor(nil)
	if err != nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "mail_read_failed", err.Error())
		return
	}
	var summary featuremail.Counts
	for _, counts := range all {
		summary.Total += counts.Total
		summary.Unread += counts.Unread
		summary.Seen += counts.Seen
	}
	var nextBefore int64
	if len(items) > 0 {
		nextBefore = items[len(items)-1].MessageOrder
	}
	httpapi.WriteJSON(w, map[string]any{"items": items, "summary": summary, "nextBefore": nextBefore})
}

// PromptWithUnreadFooter appends the current unread notice to a submitted prompt.
func PromptWithUnreadFooter(notices *interact.Notices, hostedThreadID, prompt string) (string, error) {
	if hostedThreadID == "" {
		return prompt, nil
	}
	return notices.Append(hostedThreadID, prompt), nil
}

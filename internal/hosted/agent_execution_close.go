package hosted

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/sessions"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

// ResolveHostedRecord accepts the durable hosted-thread id, an optional
// client creation alias, the current Nano session id, or the live provider
// thread id. All references converge on the same durable record before an
// operation is created.
func (s *Controller) ResolveHostedRecord(ref string) (ownedthreads.SessionRecord, bool) {
	if record, ok := s.Executions.ResolveSession(ref); ok {
		return record, true
	}
	return s.Sessions.ResolveLiveHostedThreadRecord(ref, s.Executions.Session)
}

// closeHostedThreadForRecord ends a hosted thread's attachment whether or not
// a live session exists, finishing its attachment row in actor's name.
// Callers hold relationshipMu. The detached branch settles non-terminal
// operations explicitly — previously they dangled forever, because only a
// live session's close path ran settlement.
func (s *Controller) closeHostedThreadForRecord(record ownedthreads.SessionRecord, actor string) error {
	return s.CloseLifecycle(record.ID, func() error { return s.closeHostedThreadForRecordCore(record, actor) })
}

// closeHostedThreadForRecordCore closes only the provider attachment while
// callers hold any feature-specific lifecycle locks.
func (s *Controller) closeHostedThreadForRecordCore(record ownedthreads.SessionRecord, actor string) error {
	if sess := s.sessionForHosted(record.ID); sess != nil {
		return s.closeAgentSession(sess, actor)
	}
	if s.Attachments != nil && record.NativeThreadID != "" {
		if attachment, ok := s.Attachments.Get(record.Provider, record.NativeThreadID); ok {
			if attachment.HostedThreadID != "" && attachment.HostedThreadID != record.ID {
				return errors.New("attachment belongs to another hosted thread; close refused")
			}
			if err := s.closeDetachedAttachment(attachment, actor); err != nil {
				return err
			}
		}
	}
	if err := s.Executions.MarkSessionTurns(record.ID, ownedthreads.AgentTurnInterrupted, &ownedthreads.ExecutionError{
		Source: "client", Code: "session_closed",
		Message: "the hosted thread was closed while detached",
	}); err != nil {
		return err
	}
	// Persist the terminal state inside the heartbeat lifecycle lock. The
	// public close and bulk-close handlers repeat this update with their own
	// reason, but doing it here prevents a concurrent attachment from slipping
	// into the detached-close gap.
	return s.Executions.UpdateSession(record.ID, "", "", hostedstore.StatusExited, "closed")
}

// dropQueuedTurns removes undelivered queued prompts for the thread: an
// operation settled as interrupted must not redeliver its prompt on a later
// reattachment.
func (s *Controller) dropQueuedTurns(record ownedthreads.SessionRecord) error {
	if s.Queue == nil {
		return nil
	}
	for {
		rec, ok := s.Queue.First(record.Provider, record.NativeThreadID, record.ID)
		if !ok {
			return nil
		}
		if rec.ClientActionID == "" {
			return nil
		}
		if _, err := sessions.CancelQueuedTurnStores(s.Queue, s.Executions, rec.ClientActionID, sessions.DiscardedByCaller); err != nil {
			return err
		}
	}
}

func (s *Controller) closeAgentSession(sess *sessions.Session, actor string) error {
	if sess == nil {
		return nil
	}
	var attachment hostedstore.Attachment
	var remembered bool
	if s.Attachments != nil {
		attachment, remembered = s.Attachments.Get(sess.Provider(), sess.State().ThreadID())
	}
	if remembered && sess.Provider() == "claude" {
		if err := sess.Teardown().VerifyClaudeAttachment(attachment.ShimSessionID); err != nil {
			return err
		}
	}
	err := sess.Teardown().CloseWithResult()
	if !sess.State().IsClosed() {
		return err
	}
	s.Sessions.RemoveCurrentSession(sess)
	defer s.NotifyRuntime()
	if err != nil {
		return fmt.Errorf("Claude stop unconfirmed; attachment retained for retry: %w", err)
	}
	if remembered {
		if err := s.Attachments.Finish(attachment, actor); err != nil {
			return err
		}
	}
	sess.State().ConfirmClaudeClose()
	return nil
}

// Who finished an attachment row when the caller does not name a hosted
// thread: the browser's session close and dismiss controls record the
// operator; any /api/v1 close without actorThreadRef, the browser's family
// close included, records the anonymous API caller.
const (
	finishedByOperator = "operator"
	finishedByAPI      = "api"
)

// closeActor reads the optional {"actorThreadRef": ...} body of a close. A
// named actor must resolve to a hosted thread and is recorded by its hosted
// id; an empty body records the anonymous API caller.
func (s *Controller) closeActor(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		ActorThreadRef string `json:"actorThreadRef"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		httpapi.WriteHTTPError(w, `body must be empty or {"actorThreadRef":"..."}`, http.StatusBadRequest)
		return "", false
	}
	ref := strings.TrimSpace(body.ActorThreadRef)
	if ref == "" {
		return finishedByAPI, true
	}
	actor, ok := s.ResolveHostedRecord(ref)
	if !ok {
		httpapi.WriteHTTPError(w, "actorThreadRef does not resolve to a hosted thread", http.StatusBadRequest)
		return "", false
	}
	return actor.ID, true
}

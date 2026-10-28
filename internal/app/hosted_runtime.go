package app

import (
	"errors"

	"github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/sessions"
)

type hostedRuntime struct{ server *server }

func (a hostedRuntime) Admission(id string) hosted.AdmissionSession {
	sess := a.server.SessionForHostedThread(id)
	if sess == nil {
		return nil
	}
	return hostedAdmissionSession{server: a.server, session: sess}
}
func (a hostedRuntime) CloseSession(id string) hosted.CloseSession {
	sess := a.server.SessionForHostedThread(id)
	if sess == nil {
		return nil
	}
	return hostedCloseSession{server: a.server, session: sess}
}
func (a hostedRuntime) AttachmentSession(provider, native string) hosted.CloseSession {
	sess := a.server.AttachmentSession(provider, native)
	if sess == nil {
		return nil
	}
	return hostedCloseSession{server: a.server, session: sess}
}
func (a hostedRuntime) ResolveLiveRecord(ref string, lookup func(string) (agentSessionRecord, bool)) (agentSessionRecord, bool) {
	return a.server.ResolveLiveHostedThreadRecord(ref, lookup)
}
func (a hostedRuntime) Waiting(id string) string {
	_, state := a.server.boot.waitingHosted(id)
	return state
}
func (a hostedRuntime) Reattach(id string) { a.server.reattachWaitingHosted(id) }
func (a hostedRuntime) StopDetached(record hosted.Attachment) error {
	if record.Provider == "claude" {
		return sessions.CloseDetachedClaude(a.server.defaults.claudeShimWS, record.ShimSessionID)
	}
	return nil
}
func (a hostedRuntime) ForgetDetached(native string, forget func() error) error {
	return a.server.ForgetDetachedClaude(native, forget)
}
func (a hostedRuntime) CancelPrompt(id, action string) (bool, error) {
	var err error
	if sess := a.server.SessionForHostedThread(id); sess != nil {
		_, err = sess.CancelQueuedTurnAction(action, sessions.DiscardedByCaller)
	} else {
		_, err = sessions.CancelQueuedTurnStores(a.server.queuedTurns, a.server.agentExecutions, action, sessions.DiscardedByCaller)
	}
	var refused *sessions.PromptActionRefused
	conflict := errors.Is(err, sessions.ErrQueuedTurnNotFound) || errors.Is(err, sessions.ErrQueuedTurnDelivering) || errors.Is(err, sessions.ErrPromptEnded) || errors.As(err, &refused)
	return conflict, err
}
func (a hostedRuntime) CancelStoredPrompt(action string) error {
	_, err := sessions.CancelQueuedTurnStores(a.server.queuedTurns, a.server.agentExecutions, action, sessions.DiscardedByCaller)
	return err
}
func (a hostedRuntime) CheckRetiredReferences(retired map[string]bool, check func() error) error {
	return a.server.CheckRetiredReferences(retired, check)
}

type hostedAdmissionSession struct {
	server  *server
	session *sessions.Session
}

func (a hostedAdmissionSession) WithIdleDelivery(rested bool, deliver func() error) (string, error) {
	return a.session.WithIdleDelivery(rested, deliver)
}
func (a hostedAdmissionSession) StartPrepared(action, text, presentation, hash, origin string, next bool) (bool, int, string, error) {
	op, status, reason, err := a.session.QueueOrStartTurnSendPreparedPresentation(action, text, presentation, hash, origin, next)
	return op != nil, status, reason, err
}
func (a hostedAdmissionSession) StartLockedPrepared(action, text, presentation, hash, origin string, next bool) (bool, int, string, error) {
	op, status, reason, err := a.session.QueueOrStartTurnSendLockedPreparedPresentation(action, text, presentation, hash, origin, next)
	return op != nil, status, reason, err
}
func (a hostedAdmissionSession) QueuedText(action string) (string, bool) {
	row, found := a.session.QueuedPrompt(action)
	return row.Text, found
}
func (a hostedAdmissionSession) NotifyRuntime() { a.session.NotifyRuntime() }

type hostedCloseSession struct {
	server  *server
	session *sessions.Session
}

func (a hostedCloseSession) ID() string             { return a.session.ID() }
func (a hostedCloseSession) Provider() string       { return a.session.Provider() }
func (a hostedCloseSession) ThreadID() string       { return a.session.ThreadID() }
func (a hostedCloseSession) HostedThreadID() string { return a.session.HostedThreadID() }
func (a hostedCloseSession) Current() bool {
	return a.server.LookupSession(a.session.ID()) == a.session
}
func (a hostedCloseSession) VerifyClaudeAttachment(id string) error {
	return a.session.VerifyClaudeAttachment(id)
}
func (a hostedCloseSession) CloseWithResult() error { return a.session.CloseWithResult() }
func (a hostedCloseSession) IsClosed() bool         { return a.session.IsClosed() }
func (a hostedCloseSession) RemoveCurrent()         { a.server.RemoveCurrentSession(a.session) }
func (a hostedCloseSession) ConfirmClaudeClose()    { a.session.ConfirmClaudeClose() }
func (a hostedCloseSession) Detach()                { a.session.Close(); a.server.RemoveSession(a.session.ID()) }

type hostedCloseStars struct{ store *threadStarsStore }

func (a hostedCloseStars) IsStarred(provider, native string) bool {
	return a.store.isStarred(provider, native)
}
func (a hostedCloseStars) UnstarMany(keys []hosted.AttachmentKey) error {
	return a.store.unstarMany(keys)
}

func (a hostedRuntime) HasDeliveryEvidence(id, action string) bool {
	return a.server.SessionForHostedThread(id).HasDeliveryEvidence(action, a.server.queuedTurns, a.server.sendAttempts)
}
func (a hostedRuntime) DeliveryWasAttempted(id, action string) bool {
	return a.server.SessionForHostedThread(id).DeliveryWasAttempted(action, a.server.sendAttempts)
}

func (a hostedRuntime) SetPresentation(id, mode string) {
	if sess := a.server.SessionForHostedThread(id); sess != nil {
		sess.SetPresentation(mode)
	}
}
func (a hostedRuntime) SetSidebarPlacement(id, mode string) {
	if sess := a.server.SessionForHostedThread(id); sess != nil {
		sess.SetSidebarPlacement(mode)
	}
}

var _ hosted.ControlRuntime = hostedRuntime{}

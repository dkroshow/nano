package app

import (
	"net/http"
	"time"

	"github.com/dkroshow/nano/internal/hosted"
)

type agentSessionView = hosted.SessionView
type agentTurnView = hosted.TurnView
type availabilityView = hosted.AvailabilityView

func (s *server) hostedViews() *hosted.ViewService {
	return &hosted.ViewService{Executions: s.agentExecutions, Metadata: s.controlMetadata,
		ExecutionError: s.executionControlErr, MetadataError: s.controlMetadataErr,
		Sessions: hostedViewSessions{server: s}, Names: hostedViewNames{server: s},
		Waiting:       func(id string) string { _, state := s.boot.waitingHosted(id); return state },
		ResolveRecord: s.resolveHostedThreadRecord}
}
func (s *server) agentSessionView(id string) agentSessionView { return s.hostedViews().SessionView(id) }
func (s *server) agentTurnView(record agentTurnRecord) agentTurnView {
	return s.hostedViews().TurnView(record)
}
func (s *server) availabilityFor(id string, now time.Time) availabilityView {
	return s.hostedViews().AvailabilityFor(id, now)
}
func (s *server) hostedAttachmentState(id string) hostedAttachmentView {
	return s.hostedViews().AttachmentState(id)
}
func (s *server) decorateHostedMetadata(metadata hostedMetadataView, attachment hostedAttachmentView) *hostedMetadataView {
	return s.hostedViews().DecorateMetadata(metadata, attachment)
}
func (s *server) handleAgentSessionsList(w http.ResponseWriter, r *http.Request) {
	s.hostedViews().HandleSessionsList(w, r)
}
func (s *server) handleHostedThreadAvailability(w http.ResponseWriter, r *http.Request, id string) {
	s.hostedViews().HandleAvailability(w, r, id)
}
func (s *server) handleHostedThreadMetadataRead(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedViews().HandleMetadataRead(w, r, record)
}
func (s *server) handleHostedThreadMetadataChanges(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedViews().HandleMetadataChanges(w, r, record)
}
func (s *server) handleHostedThreadMetadataEvents(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedViews().HandleMetadataEvents(w, r, record)
}
func (s *server) handleHostedThreadPermissionEvents(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedViews().HandlePermissionEvents(w, r, record)
}
func (s *server) handleHostedThreadFamily(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedViews().HandleFamily(w, r, record)
}
func (s *server) handleTurnOperationRead(w http.ResponseWriter, r *http.Request) {
	s.hostedViews().HandleOperationRead(w, r)
}
func (s *server) handleTurnOperationChanges(w http.ResponseWriter, r *http.Request) {
	s.hostedViews().HandleOperationChanges(w, r)
}
func (s *server) handleTurnOperationsCollection(w http.ResponseWriter, r *http.Request) {
	s.hostedViews().HandleOperationsCollection(w, r)
}

type hostedViewSessions struct{ server *server }

func (a hostedViewSessions) Session(id string) (hosted.SessionSnapshot, bool) {
	sess := a.server.SessionForHostedThread(id)
	if sess == nil {
		return hosted.SessionSnapshot{}, false
	}
	sum := sess.Summary()
	ready, working := sess.ReadyAndWorking()
	return hosted.SessionSnapshot{ID: sum.ID, ThreadID: sum.ThreadID, Status: sum.Status, Error: sum.Error,
		TurnActive: sum.TurnActive, Waiting: sum.Waiting, MetadataDegraded: sum.MetadataDegraded,
		ThreadDiagnostics: sum.ThreadDiagnostics, Ready: ready, Working: working}, true
}
func (a hostedViewSessions) Turn(id string, interactions bool) (hosted.TurnSnapshot, bool) {
	sess := a.server.SessionForHostedThread(id)
	if sess == nil {
		return hosted.TurnSnapshot{}, false
	}
	snapshot := hosted.TurnSnapshot{ID: sess.ID(), ThreadID: sess.ThreadID()}
	if interactions {
		snapshot.Interactions = sess.PendingInteractionList()
	}
	return snapshot, true
}
func (a hostedViewSessions) Attachment(id string) (hosted.AttachmentView, bool) {
	sess := a.server.SessionForHostedThread(id)
	if sess == nil {
		return hosted.AttachmentView{}, false
	}
	sum := sess.Summary()
	return hosted.AttachmentView{State: sum.Status, Provenance: "live_session", Error: sum.Error}, true
}
func (a hostedViewSessions) Availability(id string) (hosted.AvailabilitySnapshot, bool) {
	sess := a.server.SessionForHostedThread(id)
	if sess == nil {
		return hosted.AvailabilitySnapshot{}, false
	}
	state := sess.AvailabilityState()
	pending := sess.CorePromptInFlight()
	queued := sess.QueuedPromptCount()
	return hosted.AvailabilitySnapshot{Status: state.Status, Active: state.Active, Waiting: state.Waiting, Permission: state.Permission,
		LastHumanPromptAt: state.LastHumanPromptAt, PromptInFlight: pending, QueuedPrompts: queued}, true
}
func (a hostedViewSessions) HostedIDs() []string {
	sessions := a.server.SnapshotSessions()
	ids := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		ids = append(ids, sess.HostedThreadID())
	}
	return ids
}
func (a hostedViewSessions) QueuePosition(id, action string) int {
	sess := a.server.SessionForHostedThread(id)
	return sess.QueuePositionOr(action, a.server.queuedTurns)
}

type hostedViewNames struct{ server *server }

func (a hostedViewNames) DurableName(provider, native string) (string, bool) {
	return a.server.durableThreadName(provider, native)
}
func (a hostedViewNames) DurableNameRef(provider, native string) (hosted.MetadataName, bool) {
	return a.server.durableThreadNameRef(provider, native)
}

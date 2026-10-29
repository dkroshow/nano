package app

import (
	"net/http"
	"time"

	"github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

type agentSessionView = hosted.SessionView
type agentTurnView = hosted.TurnView
type availabilityView = hosted.AvailabilityView

func (s *server) hostedViews() *hosted.ViewService {
	return &hosted.ViewService{Executions: s.agentExecutions, Metadata: s.controlMetadata,
		ExecutionError: s.executionControlErr, MetadataError: s.controlMetadataErr,
		Sessions: &s.Registry, Queue: s.queuedTurns, Names: hostedViewNames{server: s},
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

type hostedViewNames struct{ server *server }

func (a hostedViewNames) DurableName(provider, native string) (string, bool) {
	return a.server.durableThreadName(provider, native)
}
func (a hostedViewNames) DurableNameRef(provider, native string) (hostedstore.MetadataName, bool) {
	return a.server.durableThreadNameRef(provider, native)
}

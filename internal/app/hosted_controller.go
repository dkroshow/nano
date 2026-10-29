package app

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/agentapi"
	featureheartbeat "github.com/dkroshow/nano/internal/features/heartbeat"
	featuremail "github.com/dkroshow/nano/internal/features/mail"
	"github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/sidebar"
)

func (s *server) hostedController() *hosted.Controller {
	s.hostedControlOnce.Do(func() { s.hostedControl = newHostedController(s) })
	return s.hostedControl
}

func newHostedController(s *server) *hosted.Controller {
	return &hosted.Controller{
		Executions: s.agentExecutions, Metadata: s.controlMetadata, MetadataError: s.controlMetadataErr, ExecutionError: s.executionControlErr,
		Attachments: s.attachments, Queue: s.queuedTurns, Sessions: &s.Registry, SendAttempts: s.sendAttempts, ClaudeShimWS: s.defaults.ClaudeShimWS,
		Boot: &s.bootAttachments, AttachmentsControl: s.attachmentCoordinator(), Stars: s.threadStars,
		Views: s.hostedViews, ResolveIdentity: s.resolveIdentity, PrepareCreation: s.prepareHostedCreation,
		PromptWithUnreadFooter: func(id, prompt string) (string, error) { return agentapi.PromptWithUnreadFooter(s.notices, id, prompt) }, UnreadFooterUnavailable: featuremail.ErrUnreadNoticeUnavailable,
		FeatureFooter: func(action string) (string, bool) {
			if strings.HasPrefix(action, featureheartbeat.ActionPrefix) {
				return featureheartbeat.Footer, true
			}
			return "", false
		},
		CloseLifecycle: func(id string, close func() error) error {
			if s.heartbeat == nil {
				return close()
			}
			s.heartbeat.AttachmentMu.Lock()
			defer s.heartbeat.AttachmentMu.Unlock()
			return errors.Join(s.heartbeat.PauseAttachmentForClosedTarget(id, time.Now()), close())
		},
		NotifyRuntime: s.attachmentCoordinator().NotifyRuntime, RefreshRuntime: s.attachmentCoordinator().NotifyRuntimeRefresh,
		PublishRelationshipChange: func() {
			if s.runtimeEvents != nil {
				s.runtimeEvents.Notify(sidebar.RuntimeDelta{Refresh: true})
			}
		},
		LegacyCreate: s.handleSessionCreateAction,
	}
}

func (s *server) hostedThreadOperation(handler func(http.ResponseWriter, *http.Request, agentSessionRecord)) http.HandlerFunc {
	return s.hostedController().HostedOperation(handler)
}
func (s *server) handleHostedThreadRead(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedController().HandleRead(w, r, record)
}
func (s *server) handleHostedThreadDelete(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedController().HandleDelete(w, r, record)
}
func (s *server) handleHostedThreadClose(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedController().HandleClose(w, r, record)
}
func validAgentClientID(value string) bool { return hosted.ValidClientID(value) }
func (s *server) handleAgentTurnSubmit(w http.ResponseWriter, r *http.Request, hostedThreadID string) {
	s.hostedController().HandleTurnSubmit(w, r, hostedThreadID)
}
func (s *server) handleAgentSessionCreate(w http.ResponseWriter, r *http.Request) {
	s.hostedController().HandleCreate(w, r)
}
func (s *server) turnAttributionFor(sender agentSessionRecord) (turnAttribution, error) {
	return s.hostedController().TurnAttributionFor(sender)
}
func (s *server) handleHostedThreadPresentation(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedController().HandlePresentation(w, r, record)
}
func (s *server) handleHostedThreadSubscriptions(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedController().HandleSubscriptions(w, r, record)
}
func (s *server) handleHostedThreadSidebarPlacement(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedController().HandleSidebarPlacement(w, r, record)
}
func (s *server) handleHostedThreadAssignment(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedController().HandleAssignment(w, r, record)
}
func (s *server) handleHostedThreadDetach(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedController().HandleDetach(w, r, record)
}
func (s *server) handleTurnOperationCancel(w http.ResponseWriter, r *http.Request) {
	s.hostedController().HandleOperationCancel(w, r)
}
func (s *server) resolveHostedThreadRecord(ref string) (agentSessionRecord, bool) {
	return s.hostedController().ResolveHostedRecord(ref)
}
func (s *server) handleBulkCloseTreePreview(w http.ResponseWriter, root agentSessionRecord) {
	s.hostedController().HandleBulkClosePreview(w, root)
}
func (s *server) handleBulkCloseTree(w http.ResponseWriter, r *http.Request, root agentSessionRecord) {
	s.hostedController().HandleBulkClose(w, r, root)
}
func (s *server) handleRetiredHostedRecords(w http.ResponseWriter, r *http.Request) {
	s.hostedController().HandleRetirement(w, r)
}

package app

import (
	"errors"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/agentapi"
	featureheartbeat "github.com/dkroshow/nano/internal/features/heartbeat"
	featuremail "github.com/dkroshow/nano/internal/features/mail"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/sidebar"
)

func (s *server) hostedController() *hostedmodel.Controller {
	if s.hostedControl == nil {
		panic("hosted controller used before construction")
	}
	return s.hostedControl
}

func newHostedController(s *server) *hostedmodel.Controller {
	return &hostedmodel.Controller{
		Executions: s.agentExecutions, Metadata: s.controlMetadata, MetadataError: s.controlMetadataErr, ExecutionError: s.executionControlErr,
		Attachments: s.attachments, Queue: s.queuedTurns, Sessions: &s.Registry, SendAttempts: s.sendAttempts, ClaudeShimWS: s.defaults.ClaudeShimWS,
		Boot: &s.bootAttachments, AttachmentsControl: s.attachmentCoordinator(),
		Views: s.hostedViews, ResolveIdentity: s.hostedIdentity().ResolveIdentity,
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
	}
}

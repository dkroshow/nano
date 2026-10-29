package agentapi

import (
	"sync"

	"github.com/dkroshow/nano/internal/diagnostics"
	"github.com/dkroshow/nano/internal/features/feedback"
	featureheartbeat "github.com/dkroshow/nano/internal/features/heartbeat"
	featuremail "github.com/dkroshow/nano/internal/features/mail"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/sidebar"
)

// Sources supplies the concrete owners consumed by the agent HTTP adapter.
type Sources struct {
	Hosted                 *hosted.Controller
	Identity               *hosted.IdentityService
	Mail                   *featuremail.Store
	MailError              string
	MailEvents             *featuremail.Hub
	Notices                *interact.Notices
	Sidebar                *sidebar.Controller
	Heartbeat              *featureheartbeat.Service
	Feedback               *feedback.Store
	Diagnostics            *diagnostics.Store
	BuildStamp, BundleName string
}

// Controller owns agent-facing HTTP adaptation and completion-hook publication.
// Feature stores and schedulers retain their state and persistence.
type Controller struct {
	hosted                 *hosted.Controller
	identity               *hosted.IdentityService
	mail                   *featuremail.Store
	mailErr                string
	mailEvents             *featuremail.Hub
	notices                *interact.Notices
	sidebar                *sidebar.Controller
	heartbeat              *featureheartbeat.Service
	feedback               *feedback.Store
	browserDiagnostics     *diagnostics.Store
	buildStamp, bundleName string
	subscriptionMu         sync.Mutex
	subscriptions          *subscriptions.Dispatcher
}

// New composes the adapter without starting any feature or background work.
func New(s Sources) *Controller {
	return &Controller{
		hosted: s.Hosted, identity: s.Identity,
		mail: s.Mail, mailErr: s.MailError, mailEvents: s.MailEvents, notices: s.Notices,
		sidebar: s.Sidebar, heartbeat: s.Heartbeat, feedback: s.Feedback,
		browserDiagnostics: s.Diagnostics, buildStamp: s.BuildStamp, bundleName: s.BundleName,
	}
}

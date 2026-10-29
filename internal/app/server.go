package app

import (
	"io"
	"sync/atomic"
	"time"

	"github.com/dkroshow/nano/internal/agentapi"
	"github.com/dkroshow/nano/internal/config"
	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/diagnostics"
	"github.com/dkroshow/nano/internal/features/feedback"
	featureheartbeat "github.com/dkroshow/nano/internal/features/heartbeat"
	featuremail "github.com/dkroshow/nano/internal/features/mail"
	"github.com/dkroshow/nano/internal/historical"
	"github.com/dkroshow/nano/internal/history"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi/remoteauth"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/observability"
	sessionruntime "github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/setup"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/usage"
)

// deliveryExpiryWindow keeps a provider marked slow for five minutes after a delivery expires.
const deliveryExpiryWindow = 5 * time.Minute

// processStarted is the earliest moment this process can name.
var processStarted = time.Now()

type server struct {
	dropLiveTextDeltas bool
	defaults           config.SessionDefaults
	codex              *sessionruntime.CodexDaemon
	spoolDir           string
	catalog            *sidebar.CatalogSource

	// catalogRefresh is the thread catalog's nonblocking hint for one known
	// native file, handed to each spawned session so hosted-session wire
	// activity can name its file (catalog_nudge.go). Nil when the catalog is
	// unavailable.
	catalogRefresh func(string)
	// catalogSources is the catalog's attached-file watch set; each
	// registered session holds its native files there. Nil when the catalog
	// is unavailable.
	catalogSources sessionruntime.AttachedCatalog
	// runtimeEvents fans compact `runtime` SSE deltas to thread-index event
	// subscribers on session bind/unbind and turnActive flips.
	runtimeEvents *sidebar.RuntimeHub
	// mailEvents fans per-thread inbox count changes (`mail` SSE events) to
	// thread-index event subscribers so sidebar badges and open inbox panels
	// refresh without polling (agent_mail_overlay.go).
	mailEvents *featuremail.Hub
	// sidebar is the server-owned catalog projection. HTTP reads immutable
	// snapshots from it; catalog and runtime events rebuild it off request paths.
	sidebar *sidebar.Controller
	// mail is the feature-owned durable inbox store. It is separate from the
	// canonical threads database and owns its generic unread notices.
	mail    *featuremail.Store
	mailErr string
	// attachments persists durable codex attachments so hosted threads survive
	// nano restarts (attachments.go). Nil when the store is unavailable, which
	// turns the feature off cleanly: no detached entries, hooks are no-ops.
	attachments *hostedstore.AttachmentStore
	// controlMetadata is the SQLite-backed hosted metadata authority.
	controlMetadata     *hostedstore.MetadataStore
	controlMetadataErr  string
	executionControlErr string
	// sendAttempts is a bounded, hash-only idempotency journal for submitted
	// prompts. It lives beside attachments/history and never stores prompt text.
	sendAttempts *prompts.LegacyJournal
	// queuedTurns is the compatibility view of the text-bearing prompt outbox.
	// The core prompt store owns it through provider acceptance.
	queuedTurns *prompts.LegacyQueue
	// turns is the core prompt store. It stops writing after acceptance; adapter
	// events write independent turn facts through the core session.
	turns *prompts.Store
	// agentExecutions is the durable provider-neutral control plane shared by
	// every hosted thread: stable identities, action-correlated turns and results.
	agentExecutions *hostedstore.ExecutionStore
	// agent owns feature HTTP adaptation and completion-hook publication.
	agent *agentapi.Controller
	// heartbeat owns the default-off wall-clock nudge scheduler and its
	// sibling SQLite evidence; see docs/decisions/nano-heartbeat.md.
	heartbeat      *featureheartbeat.Service
	completedTurns *hostedmodel.CompletedTurnSource
	notices        *interact.Notices
	// browserDiagnostics is a bounded, content-free flight recorder uploaded by
	// open Nano tabs. It explains browser/store/stream divergence after the hard
	// refresh that would otherwise erase the evidence.
	browserDiagnostics *diagnostics.Store
	// transcriptValidations exposes the latest intended-vs-mounted row snapshot
	// per browser pane so agents can validate the actual presentation surface.
	transcriptValidations *observability.ValidationStore
	// transcriptAudits holds only short-lived, content-free request/result
	// metadata. Browser captures and SQLite reference bodies are never durable.
	transcriptAudits      *observability.AuditStore
	transcriptAuditEvents *observability.AuditHub
	// historicalCodex resolves the current setup for an on-disk Codex thread
	// without starting or resuming it. Results are shared by workspace.
	historicalCodex *historical.CodexResolver
	// historicalClaude resolves documented current Claude inputs for an
	// on-disk thread using filesystem evidence only.
	historicalClaude *historical.ClaudeResolver
	// skills is the process-wide, cwd-keyed Codex skills/list cache. Session
	// state reads never call the provider and boot fan-out is bounded here.
	skills *setup.SkillsListCache

	// threadStars is the operator pin overlay for the sidebar. Nil
	// when the sibling SQLite file failed to open; listings then report every
	// row unstarred and PUT /starred returns 503.
	threadStars *sidebar.Stars
	threadPins  *hostedmodel.PinStore
	// feedback keeps the operator's Feedback notes with their evidence
	// (feedback_api.go). Nil when its SQLite file failed to open; the feedback
	// routes then return 503.
	feedback *feedback.Store
	// usageLimits polls all three providers' account-wide subscription usage and
	// keeps the append-on-change history (usage_limits.go). Nil when its store
	// is unavailable, which serves an empty strip rather than failing a page.
	usageLimits *usage.Store
	// usageLimitsPoller is retained so the Codex push notification can nudge an
	// out-of-band refresh. Nil whenever usageLimits is.
	usageLimitsPoller  *usage.Poller
	hostMonitor        *observability.HostMonitor
	requests           observability.RequestTimes
	hostedControl      *hostedmodel.Controller
	attachmentsControl *hostedmodel.AttachmentCoordinator
	bootAttachments    sessionruntime.BootAttachments
	// restart records this process start for GET /api/v1/restarts
	// (restart_tracking.go).
	restart *hostedmodel.RestartTracker
	// stopping is set once Nano begins to exit (stop). Every session shares
	// it, so no provider delivery starts that the exit could cut off.
	stopping atomic.Bool
	// deliveryExpiries counts each provider's recent delivery deadline
	// expiries for the panes' "slow right now" line.
	deliveryExpiries *sessionruntime.DeliveryExpiries

	sessionruntime.Registry
}

type application struct {
	config          config.Config
	server          *server
	absCwd          string
	serverLog       io.Closer
	catalogStore    *history.Store
	catalogService  *sidebar.CatalogSource
	remoteAuthStore *remoteauth.Store
	remoteAuth      *remoteauth.Authenticator
}

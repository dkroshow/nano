// nano: local inspector for coding-agent sessions. One binary, three
// providers: Codex uses its app-server daemon, Claude uses its shim daemon,
// and Grok uses its native durable leader through ACP stdio proxies.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/dkroshow/nano/internal/historical"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dkroshow/nano/internal/agentapi"
	state "github.com/dkroshow/nano/internal/app/statestore"
	"github.com/dkroshow/nano/internal/config"
	"github.com/dkroshow/nano/internal/configpaths"
	"github.com/dkroshow/nano/internal/core/prompts"
	feedback "github.com/dkroshow/nano/internal/features/feedback"
	featureheartbeat "github.com/dkroshow/nano/internal/features/heartbeat"
	featuremail "github.com/dkroshow/nano/internal/features/mail"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/history"
	hosted "github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/httpapi/remoteauth"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/observability"
	sessionruntime "github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/setup"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threads"
	codexwire "github.com/dkroshow/nano/internal/wire/codex"
	"github.com/dkroshow/nano/web"
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
	catalog            threadCatalog

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
	controlMetadata     *controlMetadataStore
	controlMetadataErr  string
	executionControlErr string
	// sendAttempts is a bounded, hash-only idempotency journal for submitted
	// prompts. It lives beside attachments/history and never stores prompt text.
	sendAttempts *sendAttemptStore
	// queuedTurns is the compatibility view of the text-bearing prompt outbox.
	// The core prompt store owns it through provider acceptance.
	queuedTurns *queuedTurnStore
	// turns is the core prompt store. It stops writing after acceptance; adapter
	// events write independent turn facts through the core session.
	turns *prompts.Store
	// agentExecutions is the durable provider-neutral control plane shared by
	// every hosted thread: stable identities, action-correlated turns and results.
	agentExecutions *agentExecutionStore
	// agent owns feature HTTP adaptation and completion-hook publication.
	agent     *agentapi.Controller
	agentOnce sync.Once
	// heartbeat owns the default-off wall-clock nudge scheduler and its
	// sibling SQLite evidence; see docs/decisions/nano-heartbeat.md.
	heartbeat      *featureheartbeat.Service
	completedTurns *completedTurnSource
	// claudeOwedRuns holds one detached Claude owed pass per hosted thread
	// (claude_answer.go).
	claudeOwedRuns sync.Map
	notices        *interact.Notices
	// browserDiagnostics is a bounded, content-free flight recorder uploaded by
	// open Nano tabs. It explains browser/store/stream divergence after the hard
	// refresh that would otherwise erase the evidence.
	browserDiagnostics *browserDiagnosticStore
	// transcriptValidations exposes the latest intended-vs-mounted row snapshot
	// per browser pane so agents can validate the actual presentation surface.
	transcriptValidations *transcriptValidationStore
	// transcriptAudits holds only short-lived, content-free request/result
	// metadata. Browser captures and SQLite reference bodies are never durable.
	transcriptAudits      *transcriptAuditStore
	transcriptAuditEvents *transcriptAuditHub
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
	threadPins  *threadPinsStore
	// feedback keeps the operator's Feedback notes with their evidence
	// (feedback_api.go). Nil when its SQLite file failed to open; the feedback
	// routes then return 503.
	feedback *feedback.Store
	// usageLimits polls all three providers' account-wide subscription usage and
	// keeps the append-on-change history (usage_limits.go). Nil when its store
	// is unavailable, which serves an empty strip rather than failing a page.
	usageLimits *usageLimitsStore
	// usageLimitsPoller is retained so the Codex push notification can nudge an
	// out-of-band refresh. Nil whenever usageLimits is.
	usageLimitsPoller  *usageLimitsPoller
	hostMonitor        *hostMonitor
	requests           observability.RequestTimes
	hostedControl      *hosted.Controller
	hostedControlOnce  sync.Once
	attachmentOnce     sync.Once
	attachmentsControl *hosted.AttachmentCoordinator
	bootAttachments    sessionruntime.BootAttachments
	// restart records this process start for GET /api/v1/restarts
	// (restart_tracking.go).
	restart *hosted.RestartTracker
	// stopping is set once Nano begins to exit (stop). Every session shares
	// it, so no provider delivery starts that the exit could cut off.
	stopping atomic.Bool
	// deliveryExpiries counts each provider's recent delivery deadline
	// expiries for the panes' "slow right now" line.
	deliveryExpiries *sessionruntime.DeliveryExpiries

	sessionruntime.Registry
}

// Main runs Nano using the process arguments and standard streams.
func Main() {
	cfg, err := config.Parse(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	run(cfg)
}

func run(cfg config.Config) {
	application := construct(cfg)
	local, remote := application.mount()
	application.run(local, remote)
}

type application struct {
	config          config.Config
	server          *server
	absCwd          string
	serverLog       io.Closer
	catalogStore    *history.Store
	catalogService  *threadCatalogService
	remoteAuthStore *remoteauth.Store
	remoteAuth      *remoteauth.Authenticator
}

func construct(cfg config.Config) *application {
	serverLog := observability.InstallPersistentServerLog(cfg.Paths.ServerLog)

	absCwd, err := filepath.Abs(cfg.WorkingDirectory)
	if err != nil {
		log.Fatal(err)
	}
	spoolDir, err := sessionruntime.PrepareWireSpoolDir(cfg.Paths.WireSpoolDir)
	if err != nil {
		log.Fatal(err)
	}
	// Parsed spools are unlinked on creation; this clears what older builds left.
	if removed, sweepErr := history.SweepStaleParsedSpools(os.TempDir(), time.Hour, time.Now()); sweepErr != nil || removed > 0 {
		log.Printf("parsed spool sweep: removed %d stale spools from %s (err=%v)", removed, os.TempDir(), sweepErr)
	}
	catalogStore, catalogService, catalogPath, err := openThreadCatalog(cfg.Paths.HistoryDB, cfg.Paths.ThreadNames, cfg.DisableWatch)
	if err != nil {
		log.Printf("thread catalog unavailable: %v", err)
	} else {
		log.Printf("thread catalog using %s", catalogPath)
	}
	remoteAuthStore, err := remoteauth.OpenStore(cfg.Paths.RemoteAuthDB)
	if err != nil {
		log.Fatalf("remote authentication unavailable: %v", err)
	}
	remoteAuth := remoteauth.New(remoteAuthStore, cfg.RemoteAuthHost)
	var catalog threadCatalog
	var catalogRefresh func(string)
	var catalogSources sessionruntime.AttachedCatalog
	if catalogService != nil {
		catalog, catalogSources = catalogService, catalogService
		// Hosted sessions hint their known native files into the same queue
		// file watches use, with a watch event's weight: a hint never moves a
		// boot catch-up waiting behind the visible threads. Not gated by
		// -no-watch, which disables watches only; an unknown path hints
		// nothing and never walks a root.
		catalogRefresh = func(path string) {
			if path = filepath.Clean(path); path != "." && path != "" {
				catalogService.FileChanged(path)
			}
		}
	}
	// Durable attachments live beside the history DB. Ordinary load failures
	// disable hosted control. A rollback-source conflict must instead stop boot
	// before HTTP can expose a catalog built from stale canonical identity.
	legacyCompletedPath := cfg.Paths.CompletedTurnsDB
	var completedTurns *completedTurnStore
	var attachments *hostedstore.AttachmentStore
	var controlMetadata *controlMetadataStore
	var controlMetadataErr string
	var executionControlErr string
	var sendAttempts *sendAttemptStore
	var queuedTurns *queuedTurnStore
	var turnStore *prompts.Store
	var agentExecutions *agentExecutionStore
	var mail *featuremail.Store
	var usageLimits *usageLimitsStore
	if catalogPath != "" {
		attachPath := cfg.Paths.Attachments
		store, err := hostedstore.NewAttachmentStore(attachPath)
		if err != nil {
			log.Printf("attachment store unavailable (%s): %v", attachPath, err)
		} else {
			attachments = store
			log.Printf("attachment store using %s", attachPath)
		}
		stores, err := openThreadRuntimeStoresAt(context.Background(), state.StorePaths{
			Destination: cfg.Paths.ThreadsDB,
			Control:     cfg.Paths.LegacyControlDB,
			BackupRoot:  cfg.Paths.ThreadsBackups,
			Turns: prompts.LegacyTurnPaths{
				Queue:     cfg.Paths.LegacyQueuedTurns,
				Attempts:  cfg.Paths.LegacySendAttempts,
				Completed: legacyCompletedPath,
			},
		}, sessionruntime.NewID())
		if err != nil {
			log.Fatalf("required schema %d turns store unavailable: %v", prompts.TurnSchemaVersion, err)
		} else {
			controlMetadata, agentExecutions = stores.control, stores.executions
			queuedTurns, sendAttempts, completedTurns, turnStore = stores.queue, stores.attempts, stores.completed, stores.turns
			startThreadsSizeWatch(stores.control.CanonicalDatabase(), stores.queue.Path())
			if err := hostedstore.ImportAndScrubLegacyAttachments(controlMetadata, attachments); err != nil {
				log.Printf("canonical attachment migration unavailable: %v", err)
				controlMetadataErr, executionControlErr = err.Error(), err.Error()
			} else {
				log.Printf("canonical hosted metadata and turn stores using %s", cfg.Paths.ThreadsDB)
			}
		}
		mailPath := cfg.Paths.MailDB
		legacyMailPath := cfg.Paths.ThreadsDB
		mailStore, report, err := featuremail.OpenStore(mailPath, legacyMailPath)
		if err != nil {
			log.Fatalf("required mail store unavailable: %v", err)
		}
		mail = mailStore
		retentionDays, retentionEnabled, deleted, err := mailStore.ConfigureRetention(cfg.MailRetentionDays, time.Now())
		if err != nil {
			log.Fatalf("mail retention unavailable: %v", err)
		}
		featuremail.StartRetention(context.Background(), mailStore, log.Printf)
		retention := "off"
		if retentionEnabled {
			retention = fmt.Sprintf("%d days", retentionDays)
		}
		log.Printf("mail store using %s (legacy copy: messages=%d events=%d refs=%d sequences=%d; dispositioned retention=%s; deleted=%d)", mailPath, report.Messages, report.Events, report.Refs, report.Sequences, retention, deleted)
		// Its own SQLite file, not the catalog DB: usage readings are the one
		// thing here that cannot be rebuilt from provider files, and
		// history.sqlite's delete-and-rebuild guarantee depends on holding
		// nothing of that kind. See usage_limits.go.
		usageStore, usagePath, err := openUsageLimitsAt(cfg.UsageEnabled, cfg.Paths.UsageDB)
		if err != nil {
			log.Printf("usage limits unavailable (%s): %v", usagePath, err)
		} else if usageStore != nil {
			usageLimits = usageStore
			log.Printf("usage limits using %s", usagePath)
		}
	}
	codexDaemon := sessionruntime.NewCodexDaemon(cfg.CodexWS, cfg.CodexSupervisorLabel, func() bool {
		return codexwire.SupervisorLoaded(cfg.CodexSupervisorLabel)
	})
	skills := setup.NewSkillsListCache(func(ctx context.Context, cwd string) (json.RawMessage, error) {
		var result json.RawMessage
		err := codexDaemon.Call(ctx, "skills/list", map[string]any{"cwds": []string{cwd}}, &result)
		return result, err
	})
	s := &server{
		requests:           observability.NewRequestTimes(time.Now),
		dropLiveTextDeltas: liveTextDeltasDisabled(),
		defaults: config.SessionDefaults{
			CWD:              absCwd,
			Model:            cfg.Model,
			Sandbox:          cfg.Sandbox,
			Approval:         cfg.Approval,
			ReasoningSummary: cfg.ReasoningSummary,
			AllowedTools:     cfg.AllowedTools,
			ClaudeBin:        sessionruntime.ResolveClaudeBin(cfg.ClaudeBin),
			ClaudeShimWS:     strings.TrimSpace(cfg.ClaudeShimWS),
			ClaudeShimSpools: cfg.ClaudeShimSpoolDir,
			GrokBin:          sessionruntime.ResolveGrokBin(cfg.GrokBin),
			GrokLeaderSocket: strings.TrimSpace(cfg.GrokLeaderSocket),
			APIOrigin:        fmt.Sprintf("http://127.0.0.1:%d", cfg.Port),
		},
		codex:                 codexDaemon,
		skills:                skills,
		spoolDir:              spoolDir,
		catalog:               catalog,
		catalogRefresh:        catalogRefresh,
		catalogSources:        catalogSources,
		runtimeEvents:         sidebar.NewRuntimeHub(),
		mailEvents:            featuremail.NewHub(),
		mail:                  mail,
		attachments:           attachments,
		controlMetadata:       controlMetadata,
		controlMetadataErr:    controlMetadataErr,
		executionControlErr:   executionControlErr,
		sendAttempts:          sendAttempts,
		queuedTurns:           queuedTurns,
		turns:                 turnStore,
		agentExecutions:       agentExecutions,
		notices:               interact.NewNotices(),
		browserDiagnostics:    newBrowserDiagnosticStore(defaultBrowserDiagnosticCapacity),
		transcriptValidations: newTranscriptValidationStore(defaultTranscriptValidationCapacity),
		transcriptAudits:      newTranscriptAuditStore(),
		transcriptAuditEvents: newTranscriptAuditHub(),
		usageLimits:           usageLimits,
		Registry:              sessionruntime.NewRegistry(),
	}
	s.deliveryExpiries = sessionruntime.NewDeliveryExpiries(deliveryExpiryWindow, s.publishProviderRuntime)
	if agentExecutions != nil {
		agentExecutions.UseSavedResults(s.savedOperationResult, s.savedOperationResultFinished)
	}
	s.restart = hosted.NewRestartTracker(filepath.Join(cfg.Paths.DataDir, hosted.RestartsFileName), buildStamp, s.browserDiagnostics.BootID(), processStarted)
	if err := agentapi.SyncMailNotices(s.mail, s.notices); err != nil {
		log.Fatalf("initialize mail notices: %v", err)
	}
	if catalogService != nil && controlMetadata != nil {
		catalogService.SetCreationNameSource(controlMetadata.CreationThreadNames)
	}
	s.hostMonitor = newHostMonitor(filepath.Join(cfg.Paths.DataDir, "health"), filepath.Join(cfg.Paths.DataDir, "deploys.jsonl"))
	go s.hostMonitor.run(context.Background())
	if usageLimits != nil {
		s.usageLimitsPoller = newUsageLimitsPoller(usageLimits, codexDaemon, s.defaults, cfg.UsagePollInterval)
		go s.usageLimitsPoller.run(context.Background())
	}
	if catalogStore != nil {
		if providerHomes, err := configuredProviderHomes(); err != nil {
			log.Printf("historical Codex resolver unavailable: %v", err)
		} else if len(providerHomes) > 0 {
			s.historicalCodex = historical.NewCodexResolver(codexDaemon, catalogStore, providerHomes[0])
		}
		s.historicalClaude = historical.NewClaudeResolver(catalogStore, s.defaults.ClaudeBin)
	}
	if store, err := sidebar.OpenStars(cfg.Paths.ThreadStarsDB); err != nil {
		log.Printf("thread stars store unavailable: %v", err)
	} else {
		s.threadStars = store
	}
	if store, err := openThreadPinsStore(cfg.Paths.ThreadPinsDB); err != nil {
		log.Printf("thread pins store unavailable: %v", err)
	} else {
		s.threadPins = store
	}
	if store, err := feedback.Open(cfg.Paths.FeedbackDB); err != nil {
		log.Printf("feedback store unavailable: %v", err)
	} else {
		s.feedback = store
	}
	if completedTurns != nil {
		s.completedTurns = hosted.NewCompletedTurnSource(completedTurns)
	}
	if cfg.HeartbeatMaxWakesPerHour < 1 {
		log.Fatal("heartbeat-max-wakes-per-hour must be positive")
	}
	if store, err := featureheartbeat.OpenStore(cfg.Paths.HeartbeatDB, threads.Backup); err != nil {
		log.Printf("heartbeat store unavailable: %v", err)
	} else {
		if deleted, err := store.PruneDecisionLog(time.Now().AddDate(0, 0, -30)); err != nil {
			log.Printf("heartbeat decision retention unavailable: %v", err)
		} else if deleted > 0 {
			log.Printf("heartbeat decision retention removed %d old skip rows", deleted)
		}
		s.heartbeat = featureheartbeat.NewService(store, interact.NewHTTPClient(s.defaults.APIOrigin, nil), cfg.HeartbeatEnabled, log.Printf)
		s.heartbeat.MaxWakesHour = cfg.HeartbeatMaxWakesPerHour
		log.Printf("heartbeat store ready (scheduler enabled=%v; max wakes/hour=%d)", cfg.HeartbeatEnabled, cfg.HeartbeatMaxWakesPerHour)
	}
	if s.historicalCodex != nil && catalog != nil {
		go s.historicalCodex.WarmCatalog(context.Background(), s.hostedCatalog())
	}
	if s.historicalClaude != nil && catalog != nil {
		go s.historicalClaude.WarmCatalog(context.Background(), s.hostedCatalog())
	}
	// Every stored attachment comes back as a live session so a deploy restart
	// never leaves threads detached (boot_reattach.go). HTTP opens first; until
	// its registration each thread reports "starting".
	s.attachmentCoordinator().PrepareBoot()
	if err := s.sidebarOwner().StartSidebarMaterializer(context.Background(), time.Now()); err != nil {
		log.Printf("sidebar materialization unavailable: %v", err)
	}
	_ = s.agentController()
	if catalogService != nil {
		catalogService.SetTranscriptRepairPriority(s.transcriptRepairPriority)
		catalogService.SetTranscriptChanged(s.confirmDurableThread)
	}
	return &application{
		config: cfg, server: s, absCwd: absCwd, serverLog: serverLog,
		catalogStore: catalogStore, catalogService: catalogService,
		remoteAuthStore: remoteAuthStore, remoteAuth: remoteAuth,
	}
}

func (a *application) mount() (http.Handler, http.Handler) {
	mux := http.NewServeMux()
	a.remoteAuth.RegisterRoutes(mux)
	mountRoutes(mux, applicationRoutes(a.server))
	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		log.Fatal(err)
	}
	// Cache discipline for the embedded SPA: the shell (index.html) must
	// revalidate on every load — the embed FS carries no modtimes, so without
	// this a browser can heuristically pin a stale bundle across deploys and
	// run old code indefinitely. Hash-named assets are immutable by
	// construction and cache forever.
	fileServer := http.FileServer(http.FS(dist))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	}))
	return listenerHandlersWithLoopback(httpapi.CompressResponses(a.server.refuseWritesWhileStopping(mux)), a.remoteAuth.Guard)
}

func (a *application) run(localHandler, remoteHandler http.Handler) {
	cfg, s := a.config, a.server
	stopSignals := make(chan os.Signal, 1)
	signal.Notify(stopSignals, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-stopSignals
		started := time.Now()
		report := s.stop(stopDeliveryWait, stopDeadline)
		s.restart.WriteUnsettled("interrupted", true)
		log.Printf("nano stopped in %s: %s", time.Since(started).Round(time.Millisecond), report)
		os.Exit(0)
	}()
	listenAddress := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	log.Printf("nano listening on http://%s (default cwd %s, codex daemon %s, claude shim %s, grok %s)",
		listenAddress, a.absCwd, cfg.CodexWS, s.defaults.ClaudeShimWS, s.defaults.GrokBin)
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		log.Fatal(err)
	}
	serveErr := a.serve(listener, localHandler, remoteHandler)
	if s.heartbeat != nil {
		s.heartbeat.Close()
		if err := s.heartbeat.Store.Close(); err != nil {
			log.Printf("close heartbeat store: %v", err)
		}
	}
	if s.mail != nil {
		if err := s.mail.Close(); err != nil {
			log.Printf("close mail store: %v", err)
		}
	}
	if a.catalogService != nil {
		a.catalogService.Close()
	}
	if a.catalogStore != nil {
		if err := a.catalogStore.Close(); err != nil {
			log.Printf("close thread catalog: %v", err)
		}
	}
	if err := a.remoteAuthStore.Close(); err != nil {
		log.Printf("close remote authentication store: %v", err)
	}
	if a.serverLog != nil {
		_ = a.serverLog.Close()
	}
	log.Fatal(serveErr)
}

// serve answers HTTP at once and reattaches stored threads behind it, so a
// restart is seconds of closed port rather than minutes. It returns the first
// serving error.
func (a *application) serve(listener net.Listener, localHandler, remoteHandler http.Handler) error {
	cfg, s := a.config, a.server
	serveErrors := make(chan error, 3)
	go func() { serveErrors <- http.Serve(listener, localHandler) }()
	s.restart.MarkHTTPOpen()
	go func() {
		s.attachmentCoordinator().ReattachOnBoot()
		// Features that prompt hosted threads by themselves start after every
		// stored thread is back. HTTP opens first so requests can prioritize them.
		if s.agentExecutions != nil && s.agentExecutions.Control() != nil {
			d := subscriptions.NewDispatcher(s.agentExecutions.Control().CanonicalDatabase(), interact.NewHTTPClient(s.defaults.APIOrigin, nil), s.agentDisplay, log.Printf)
			s.agentController().StartSubscriptions(d)
		}
		if s.heartbeat != nil {
			s.heartbeat.Start(context.Background())
		}
	}()
	if cfg.RemoteAuthPort != 0 {
		if a.remoteAuth.RemoteHost() == "" {
			log.Fatal("-remote-auth-host is required when -remote-auth-port is enabled")
		}
		remoteAddress := fmt.Sprintf("127.0.0.1:%d", cfg.RemoteAuthPort)
		log.Printf("nano protected remote listener on http://%s for host %s", remoteAddress, a.remoteAuth.RemoteHost())
		remoteServer := &http.Server{
			Addr:              remoteAddress,
			Handler:           remoteHandler,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		go func() { serveErrors <- remoteServer.ListenAndServe() }()
	}
	if cfg.LocalTLSPort != 0 {
		// The same trusted handler over TLS, so browsers get HTTP/2: one
		// connection carries every pane's stream instead of hitting the
		// six-connection limit browsers keep per HTTP/1.1 host.
		tlsAddress := fmt.Sprintf("127.0.0.1:%d", cfg.LocalTLSPort)
		tlsDir := filepath.Join(cfg.Paths.DataDir, "tls")
		log.Printf("nano trusted local listener on https://%s", tlsAddress)
		tlsServer := &http.Server{Addr: tlsAddress, Handler: localHandler}
		go func() {
			serveErrors <- tlsServer.ListenAndServeTLS(filepath.Join(tlsDir, "localhost.pem"), filepath.Join(tlsDir, "localhost-key.pem"))
		}()
	}
	return <-serveErrors
}

// launchd sends SIGKILL 5 s after SIGTERM (the job sets no ExitTimeOut). The
// stop finishes within that deadline.
const (
	stopDeliveryWait = 3 * time.Second
	stopDeadline     = 4 * time.Second
)

// stopReport says what the stop waited for. A delivery still running at exit
// is restored as uncertain on the next boot and is never resent.
type stopReport struct {
	finished, unfinished int
	deadlineReached      bool
}

func (r stopReport) String() string {
	if r.deadlineReached {
		return "stop deadline reached before in-flight deliveries were counted"
	}
	return fmt.Sprintf("%d in-flight deliveries finished, %d still running", r.finished, r.unfinished)
}

// stop begins Nano's exit. Writes are refused and no new provider delivery
// starts; deliveries already in flight get deliveryWait to record their
// outcome. It returns by deadline even if a step blocks. Provider daemons are
// never touched: their turns continue and the next process reattaches them.
func (s *server) stop(deliveryWait, deadline time.Duration) stopReport {
	started := time.Now()
	s.stopping.Store(true)
	waited := make(chan stopReport, 1)
	go func() { waited <- s.awaitInFlightDeliveries(started.Add(deliveryWait)) }()
	timer := time.NewTimer(deadline - time.Since(started))
	defer timer.Stop()
	select {
	case report := <-waited:
		return report
	case <-timer.C:
		return stopReport{deadlineReached: true}
	}
}

// awaitInFlightDeliveries waits until every delivery that started before the
// stop has finished, or until the given time. A claim holds its session's
// sendMu while it registers its operation, so each session's operations are
// complete once that lock is taken after stopping is set.
func (s *server) awaitInFlightDeliveries(until time.Time) stopReport {
	sessions := s.SnapshotSessions()
	var inFlight []chan struct{}
	for _, sess := range sessions {
		inFlight = append(inFlight, sess.InFlightDeliveries()...)
	}
	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()
	for i, done := range inFlight {
		select {
		case <-done:
		case <-timer.C:
			unfinished := 0
			for _, rest := range inFlight[i:] {
				select {
				case <-rest:
				default:
					unfinished++
				}
			}
			return stopReport{finished: len(inFlight) - unfinished, unfinished: unfinished}
		}
	}
	return stopReport{finished: len(inFlight)}
}

// refuseWritesWhileStopping answers every write with a retry once Nano has
// begun to stop, before any handler can record it. Reads and open streams
// continue until exit. It wraps the application inside the listener guards,
// so Host, Origin and remote authentication still decide first.
func (s *server) refuseWritesWhileStopping(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.stopping.Load() && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			w.Header().Set("Retry-After", "10")
			httpapi.WriteError(w, http.StatusServiceUnavailable, "restarting", "Nano is restarting. Nothing was recorded; retry in a few seconds.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if s.agentExecutions == nil || s.controlMetadata == nil || s.mail == nil || s.executionControlErr != "" || s.controlMetadataErr != "" || s.mailErr != "" {
		reason := s.executionControlErr
		if reason == "" {
			reason = s.controlMetadataErr
		}
		if reason == "" {
			reason = s.mailErr
		}
		if reason == "" {
			reason = "required control store is unavailable"
		}
		httpapi.WriteHTTPError(w, "hosted control unavailable: "+reason, http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

func openThreadCatalog(path, namesPath string, noWatch bool) (*history.Store, *threadCatalogService, string, error) {
	absolute, err := canonicalProspectivePath(path)
	if err != nil {
		return nil, nil, "", fmt.Errorf("resolve thread catalog database: %w", err)
	}
	namesPath, err = canonicalProspectivePath(namesPath)
	if err != nil {
		return nil, nil, absolute, fmt.Errorf("resolve thread names file: %w", err)
	}
	roots, err := history.ResolveStandardRoots(history.RootOptions{})
	if err != nil {
		return nil, nil, absolute, fmt.Errorf("resolve provider roots before opening catalog: %w", err)
	}
	providerHomes, err := configuredProviderHomes()
	if err != nil {
		return nil, nil, absolute, err
	}
	if err := validateThreadCatalogDBLocation(absolute, providerHomes); err != nil {
		return nil, nil, absolute, err
	}
	store, err := history.Open(absolute)
	if err != nil {
		return nil, nil, absolute, err
	}
	service, err := newThreadCatalogService(context.Background(), store, threadCatalogServiceOptions{
		Roots:                               roots,
		NamesPath:                           namesPath,
		DisableWatcher:                      noWatch,
		Logf:                                log.Printf,
		EnableConversationSearchScheduler:   true,
		EnableSupersededProjectionCollector: supersededProjectionCollectionEnabled(),
		EnableParserStateCompaction:         true,
		EnableUsageBackfillScheduler:        true,
		ParserBackfillPauseFile:             filepath.Join(filepath.Dir(absolute), "parser-backfill.pause"),
	})
	if err != nil {
		_ = store.Close()
		return nil, nil, absolute, err
	}
	return store, service, absolute, nil
}

// liveTextDeltasDisabled is an environment switch, not a flag, so a rollback
// to a binary that predates it still boots with the same launch plist.
func liveTextDeltasDisabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("NANO_LIVE_TEXT_DELTAS")), "off")
}

func supersededProjectionCollectionEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("NANO_SUPERSEDED_COLLECTION")), "on")
}

func configuredProviderHomes() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve provider home: %w", err)
	}
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	claudeHome := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if claudeHome == "" {
		claudeHome = filepath.Join(home, ".claude")
	}
	grokHome := filepath.Join(home, ".grok")
	result := make([]string, 0, 3)
	for _, providerHome := range []string{codexHome, claudeHome, grokHome} {
		canonical, err := canonicalProspectivePath(providerHome)
		if err != nil {
			return nil, fmt.Errorf("resolve provider-owned directory: %w", err)
		}
		result = append(result, canonical)
	}
	return result, nil
}

func validateThreadCatalogDBLocation(database string, providerHomes []string) error {
	canonicalDatabase, err := canonicalProspectivePath(database)
	if err != nil {
		return fmt.Errorf("resolve thread catalog database location: %w", err)
	}
	for _, providerHome := range providerHomes {
		canonicalHome, err := canonicalProspectivePath(providerHome)
		if err != nil {
			return fmt.Errorf("resolve provider-owned directory: %w", err)
		}
		relative, err := filepath.Rel(canonicalHome, canonicalDatabase)
		if err != nil {
			return fmt.Errorf("compare thread catalog and provider paths: %w", err)
		}
		if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return fmt.Errorf("thread catalog database must not be inside provider-owned directory %s", canonicalHome)
		}
	}
	return nil
}

// canonicalProspectivePath resolves symlinks in the existing portion of a
// path without creating the database or any parent directory.
func canonicalProspectivePath(path string) (string, error) {
	return configpaths.CanonicalProspective(path)
}

func (s *server) handleSessionsList(w http.ResponseWriter, _ *http.Request) {
	// The position comes first: a change after it is either in this answer or
	// replayed over it, never missed.
	var position *sidebar.RuntimePosition
	if s.runtimeEvents != nil {
		at := s.runtimeEvents.Position()
		position = &at
	}
	sessions := s.SnapshotSessions()

	listed := s.sidebarOwner().ListedSessions(sessions)
	live := make(map[attachmentKey]bool, len(listed))
	for _, sum := range listed {
		if sum.ThreadID != "" && sum.Status != sessionruntime.StatusExited {
			live[attachmentKey{Provider: sum.Provider, NativeThreadID: sum.ThreadID}] = true
		}
	}
	httpapi.WriteJSON(w, map[string]any{
		"sessions":      listed,
		"detached":      s.sidebarOwner().DetachedSessionViews(live),
		"runtimeStream": position,
		"defaults": map[string]string{
			"cwd": s.defaults.CWD, "model": s.defaults.Model,
			"sandbox": s.defaults.Sandbox, "approval": s.defaults.Approval,
			"allowedTools": s.defaults.AllowedTools, "claudeBin": s.defaults.ClaudeBin,
			"grokBin": s.defaults.GrokBin, "grokLeaderSocket": s.defaults.GrokLeaderSocket,
		},
	})
}

func (s *server) sessionOperation(operation func(http.ResponseWriter, *http.Request, *sessionruntime.Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("sessionId")
		sess := s.LookupSession(id)
		if sess == nil {
			httpapi.WriteHTTPError(w, "no such session", http.StatusNotFound)
			return
		}
		operation(w, r, sess)
	}
}

// handleSpawnPreview returns the exact protocol payload a spawn request
// would produce — built by the same resolveSpawn/codexSessionRequest/claudeArgs
// code that spawnSession uses, so the New Window dialog's JSON pane shows
// the wire truth, not a frontend reconstruction.
func (s *server) handleSpawnPreview(w http.ResponseWriter, r *http.Request) {
	var req hosted.SpawnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Provider == "" {
		httpapi.WriteHTTPError(w, "body must be a spawn request", http.StatusBadRequest)
		return
	}
	resolved, err := s.attachmentCoordinator().ResolveSpawn(req)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch resolved.Provider {
	case "codex":
		method, params := sessionruntime.CodexSessionRequest(resolved.ProviderRequest)
		out := map[string]any{
			"provider":      "codex",
			"method":        method,
			"params":        params,
			"turnOverrides": map[string]any{},
		}
		if resolved.Mode != "resume" {
			// The model rides thread/start at spawn time, so it is not a turn
			// override yet — the pane's per-turn control introduces one later.
			out["turnOverrides"] = sessionruntime.CodexTurnOverrides("", resolved.Effort, "", s.defaults.ReasoningSummary)
		}
		httpapi.WriteJSON(w, out)
	case "claude":
		httpapi.WriteJSON(w, map[string]any{
			"provider": "claude",
			"argv":     sessionruntime.ClaudeCommand(resolved.ProviderRequest),
		})
	case "grok":
		method, params := sessionruntime.GrokSessionRequest(resolved.ProviderRequest)
		httpapi.WriteJSON(w, map[string]any{
			"provider": "grok",
			"argv":     sessionruntime.GrokCommand(resolved.ProviderRequest),
			"initialize": map[string]any{
				"protocolVersion": 1, "clientCapabilities": map[string]any{},
			},
			"method": method,
			"params": params,
		})
	}
}

// handleCodexModels serves the (cached) codex model/list response so the New
// Window dialog can offer models, efforts, and service tiers from the wire.
func (s *server) handleCodexModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.codex.FetchModels(r.URL.Query().Get("refresh") == "true")
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(models)
}

func (s *server) handleGrokModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	models, err := sessionruntime.FetchGrokModels(ctx, sessionruntime.ProviderRequest{Mode: "start", Cwd: s.defaults.CWD, GrokBin: s.defaults.GrokBin, GrokLeaderSocket: s.defaults.GrokLeaderSocket})
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(models)
}

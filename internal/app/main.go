// nano: local inspector for coding-agent sessions. One binary, three
// providers: Codex uses its app-server daemon, Claude uses its shim daemon,
// and Grok uses its native durable leader through ACP stdio proxies.
package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	state "github.com/dkroshow/nano/internal/app/statestore"
	"github.com/dkroshow/nano/internal/core/prompts"
	feedback "github.com/dkroshow/nano/internal/features/feedback"
	nanofiles "github.com/dkroshow/nano/internal/files"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/httpapi/remoteauth"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/web"
)

// sessionDefaults come from flags and apply to every spawned session unless
// the spawn request overrides them (provider/cwd/model are per-spawn).
type sessionDefaults struct {
	cwd              string
	model            string
	sandbox          string
	approval         string
	reasoningSummary string
	allowedTools     string
	claudeBin        string
	claudeShimWS     string
	// claudeShimSpools is the shim daemon's spool directory, read-only here.
	claudeShimSpools string
	grokBin          string
	grokLeaderSocket string
	// apiOrigin is the address hosted agents are told to reach this server on;
	// it rides the injected identity block (agent_identity.go).
	apiOrigin string
}

type server struct {
	dropLiveTextDeltas bool
	defaults           sessionDefaults
	codex              *codexDaemon
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
	catalogSources attachedSourceCatalog
	// runtime answers "is a nano session attached to this thread?" for the
	// catalog API's volatile runtime overlay (runtime.go); main injects the
	// server itself. Nil leaves the overlay field absent on every response.
	runtime runtimeSource
	// runtimeEvents fans compact `runtime` SSE deltas to thread-index event
	// subscribers on session bind/unbind and turnActive flips.
	runtimeEvents *runtimeHub
	// mailEvents fans per-thread inbox count changes (`mail` SSE events) to
	// thread-index event subscribers so sidebar badges and open inbox panels
	// refresh without polling (agent_mail_overlay.go).
	mailEvents *mailHub
	// sidebar is the server-owned catalog projection. HTTP reads immutable
	// snapshots from it; catalog and runtime events rebuild it off request paths.
	sidebar *sidebarMaterializedState
	// warm holds the open warm streams and the threads they warm.
	warm warmTranscripts
	// mail is the feature-owned durable inbox store. It is separate from the
	// canonical threads database and owns its generic unread notices.
	mail              *agentMessageStore
	mailErr           string
	runtimeSnapshotMu sync.Mutex
	runtimePublishMu  sync.Mutex // one snapshot and its notify at a time (publishRuntimeDelta)
	runtimeSnapshot   map[attachmentKey]runtimeInfo
	runtimeSessions   map[string]string
	runtimeDetached   map[attachmentKey]string // detached records as last sent (detachedChanges)
	// attachments persists durable codex attachments so hosted threads survive
	// nano restarts (attachments.go). Nil when the store is unavailable, which
	// turns the feature off cleanly: no detached entries, hooks are no-ops.
	attachments *attachmentStore
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
	// subscriptions is set once, after boot, under mu.
	subscriptions *subscriptionDispatcher
	// heartbeat owns the default-off wall-clock nudge scheduler and its
	// sibling SQLite evidence; see docs/decisions/nano-heartbeat.md.
	heartbeat      *heartbeatService
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
	historicalCodex *historicalConfigResolver
	// historicalClaude resolves documented current Claude inputs for an
	// on-disk thread using filesystem evidence only.
	historicalClaude *historicalClaudeResolver
	// skills is the process-wide, cwd-keyed Codex skills/list cache. Session
	// state reads never call the provider and boot fan-out is bounded here.
	skills *skillsListCache
	// fleet watches the recent catalog for fleet-state transitions and serves
	// them as a bookmark feed (fleet_changes.go). Nil when the catalog is
	// unavailable, which reports the feed as unavailable rather than empty.
	fleet *fleetWatcher
	// threadStars is the operator pin overlay for the sidebar. Nil
	// when the sibling SQLite file failed to open; listings then report every
	// row unstarred and PUT /starred returns 503.
	threadStars *threadStarsStore
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
	usageLimitsPoller *usageLimitsPoller
	hostMonitor       *hostMonitor
	requests          requestTimes
	// relationshipMu serializes every mutation that can change or act on the
	// current hosted-thread tree: child creation, assignment, close, and bulk
	// close. Without it, a child could be created or moved after a bulk-close
	// snapshot and escape the operator's requested tree removal.
	relationshipMu sync.Mutex
	// The resume locks, one per provider, cover resume routing through the
	// provider's pre-claim. They make first registration and ordinary
	// concurrent resume of a thread share one winner without holding the
	// general session registry lock across provider work. A thread has one
	// provider, so one provider's slow pre-claim never holds another's. An
	// unknown provider shares resumeSpawnMu; spawnSession refuses it later.
	resumeSpawnMu, resumeSpawnClaude, resumeSpawnCodex, resumeSpawnGrok sync.Mutex
	// interrupted repair reads the configured catalog and writes only beneath
	// the configured maintenance root; neither path is inferred from another store.

	// boot holds the stored threads this process has not reattached yet
	// (boot_reattach.go). Set once before HTTP serves.
	boot *bootReattach
	// restart records this process start for GET /api/v1/restarts
	// (restart_tracking.go).
	restart *restartTracker
	// stopping is set once Nano begins to exit (stop). Every session shares
	// it, so no provider delivery starts that the exit could cut off.
	stopping atomic.Bool
	// deliveryExpiries counts each provider's recent delivery deadline
	// expiries for the panes' "slow right now" line.
	deliveryExpiries *deliveryExpiries

	mu       sync.Mutex
	sessions map[string]*session
}

// Main runs Nano using the process arguments and standard streams.
func Main() {
	config, err := parseConfig(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	run(config)
}

func run(config Config) {
	application := construct(config)
	local, remote := application.mount()
	application.run(local, remote)
}

type application struct {
	config          Config
	server          *server
	absCwd          string
	serverLog       io.Closer
	catalogStore    *history.Store
	catalogService  *threadCatalogService
	remoteAuthStore *remoteauth.Store
	remoteAuth      *remoteauth.Authenticator
}

func construct(config Config) *application {
	serverLog := installPersistentServerLog(config.Paths.ServerLog)

	absCwd, err := filepath.Abs(config.WorkingDirectory)
	if err != nil {
		log.Fatal(err)
	}
	spoolDir, err := prepareWireSpoolDir(config.Paths.WireSpoolDir)
	if err != nil {
		log.Fatal(err)
	}
	// Parsed spools are unlinked on creation; this clears what older builds left.
	if removed, sweepErr := history.SweepStaleParsedSpools(os.TempDir(), time.Hour, time.Now()); sweepErr != nil || removed > 0 {
		log.Printf("parsed spool sweep: removed %d stale spools from %s (err=%v)", removed, os.TempDir(), sweepErr)
	}
	catalogStore, catalogService, catalogPath, err := openThreadCatalog(config.Paths.HistoryDB, config.Paths.ThreadNames, config.DisableWatch)
	if err != nil {
		log.Printf("thread catalog unavailable: %v", err)
	} else {
		log.Printf("thread catalog using %s", catalogPath)
	}
	remoteAuthStore, err := remoteauth.OpenStore(config.Paths.RemoteAuthDB)
	if err != nil {
		log.Fatalf("remote authentication unavailable: %v", err)
	}
	remoteAuth := remoteauth.New(remoteAuthStore, config.RemoteAuthHost)
	var catalog threadCatalog
	var catalogRefresh func(string)
	var catalogSources attachedSourceCatalog
	if catalogService != nil {
		catalog, catalogSources = catalogService, catalogService
		// Hosted sessions hint their known native files into the same queue
		// file watches use, with a watch event's weight: a hint never moves a
		// boot catch-up waiting behind the visible threads. Not gated by
		// -no-watch, which disables watches only; an unknown path hints
		// nothing and never walks a root.
		catalogRefresh = func(path string) {
			if path = filepath.Clean(path); path != "." && path != "" {
				catalogService.fileChanged(path)
			}
		}
	}
	// Durable attachments live beside the history DB. Ordinary load failures
	// disable hosted control. A rollback-source conflict must instead stop boot
	// before HTTP can expose a catalog built from stale canonical identity.
	legacyCompletedPath := config.Paths.CompletedTurnsDB
	var completedTurns *completedTurnStore
	var attachments *attachmentStore
	var controlMetadata *controlMetadataStore
	var controlMetadataErr string
	var executionControlErr string
	var sendAttempts *sendAttemptStore
	var queuedTurns *queuedTurnStore
	var turnStore *prompts.Store
	var agentExecutions *agentExecutionStore
	var mail *agentMessageStore
	var usageLimits *usageLimitsStore
	if catalogPath != "" {
		attachPath := config.Paths.Attachments
		store, err := newAttachmentStore(attachPath)
		if err != nil {
			log.Printf("attachment store unavailable (%s): %v", attachPath, err)
		} else {
			attachments = store
			log.Printf("attachment store using %s", attachPath)
		}
		stores, err := openThreadRuntimeStoresAt(context.Background(), state.StorePaths{
			Destination: config.Paths.ThreadsDB,
			Control:     config.Paths.LegacyControlDB,
			BackupRoot:  config.Paths.ThreadsBackups,
			Turns: prompts.LegacyTurnPaths{
				Queue:     config.Paths.LegacyQueuedTurns,
				Attempts:  config.Paths.LegacySendAttempts,
				Completed: legacyCompletedPath,
			},
		}, newSessionID())
		if err != nil {
			log.Fatalf("required schema %d turns store unavailable: %v", prompts.TurnSchemaVersion, err)
		} else {
			controlMetadata, agentExecutions = stores.control, stores.executions
			queuedTurns, sendAttempts, completedTurns, turnStore = stores.queue, stores.attempts, stores.completed, stores.turns
			startThreadsSizeWatch(stores.control.db, stores.queue.path)
			if err := importAndScrubLegacyAttachments(controlMetadata, attachments); err != nil {
				log.Printf("canonical attachment migration unavailable: %v", err)
				controlMetadataErr, executionControlErr = err.Error(), err.Error()
			} else {
				log.Printf("canonical hosted metadata and turn stores using %s", config.Paths.ThreadsDB)
			}
		}
		mailPath := config.Paths.MailDB
		legacyMailPath := config.Paths.ThreadsDB
		mailStore, report, err := openAgentMessageStore(mailPath, legacyMailPath)
		if err != nil {
			log.Fatalf("required mail store unavailable: %v", err)
		}
		mail = mailStore
		retentionDays, retentionEnabled, deleted, err := mailStore.ConfigureRetention(config.MailRetentionDays, time.Now())
		if err != nil {
			log.Fatalf("mail retention unavailable: %v", err)
		}
		startAgentMailRetention(context.Background(), mailStore, log.Printf)
		retention := "off"
		if retentionEnabled {
			retention = fmt.Sprintf("%d days", retentionDays)
		}
		log.Printf("mail store using %s (legacy copy: messages=%d events=%d refs=%d sequences=%d; dispositioned retention=%s; deleted=%d)", mailPath, report.Messages, report.Events, report.Refs, report.Sequences, retention, deleted)
		// Its own SQLite file, not the catalog DB: usage readings are the one
		// thing here that cannot be rebuilt from provider files, and
		// history.sqlite's delete-and-rebuild guarantee depends on holding
		// nothing of that kind. See usage_limits.go.
		usageStore, usagePath, err := openUsageLimitsAt(config.UsageEnabled, config.Paths.UsageDB)
		if err != nil {
			log.Printf("usage limits unavailable (%s): %v", usagePath, err)
		} else if usageStore != nil {
			usageLimits = usageStore
			log.Printf("usage limits using %s", usagePath)
		}
	}
	codexDaemon := newCodexDaemon(config.CodexWS, config.CodexSupervisorLabel)
	skills := newSkillsListCache(func(ctx context.Context, cwd string) (json.RawMessage, error) {
		var result json.RawMessage
		err := codexDaemon.Call(ctx, "skills/list", map[string]any{"cwds": []string{cwd}}, &result)
		return result, err
	})
	s := &server{
		dropLiveTextDeltas: liveTextDeltasDisabled(),
		defaults: sessionDefaults{
			cwd:              absCwd,
			model:            config.Model,
			sandbox:          config.Sandbox,
			approval:         config.Approval,
			reasoningSummary: config.ReasoningSummary,
			allowedTools:     config.AllowedTools,
			claudeBin:        resolveClaudeBin(config.ClaudeBin),
			claudeShimWS:     strings.TrimSpace(config.ClaudeShimWS),
			claudeShimSpools: config.ClaudeShimSpoolDir,
			grokBin:          resolveGrokBin(config.GrokBin),
			grokLeaderSocket: strings.TrimSpace(config.GrokLeaderSocket),
			apiOrigin:        fmt.Sprintf("http://127.0.0.1:%d", config.Port),
		},
		codex:                 codexDaemon,
		skills:                skills,
		spoolDir:              spoolDir,
		catalog:               catalog,
		catalogRefresh:        catalogRefresh,
		catalogSources:        catalogSources,
		runtimeEvents:         newRuntimeHub(),
		mailEvents:            newMailHub(),
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
		sessions:              map[string]*session{},
	}
	s.deliveryExpiries = newDeliveryExpiries(deliveryExpiryWindow, s.publishProviderRuntime)
	if agentExecutions != nil {
		agentExecutions.useSavedResults(s.savedOperationResult, s.savedOperationResultFinished)
	}
	s.restart = newRestartTracker(filepath.Join(config.Paths.DataDir, restartsFileName), buildStamp, s.browserDiagnostics.BootID(), processStarted)
	if err := s.syncMailNotices(); err != nil {
		log.Fatalf("initialize mail notices: %v", err)
	}
	if catalogService != nil && controlMetadata != nil {
		catalogService.setCreationNameSource(controlMetadata.creationThreadNames)
	}
	s.hostMonitor = newHostMonitor(filepath.Join(config.Paths.DataDir, "health"), filepath.Join(config.Paths.DataDir, "deploys.jsonl"))
	go s.hostMonitor.run(context.Background())
	if usageLimits != nil {
		s.usageLimitsPoller = newUsageLimitsPoller(usageLimits, codexDaemon, s.defaults, config.UsagePollInterval)
		go s.usageLimitsPoller.run(context.Background())
	}
	if catalogService != nil {
		catalogService.setTranscriptRepairPriority(s.transcriptRepairPriority)
		catalogService.setTranscriptChanged(s.confirmDurableThread)
	}
	if catalogStore != nil {
		if providerHomes, err := configuredProviderHomes(); err != nil {
			log.Printf("historical Codex resolver unavailable: %v", err)
		} else if len(providerHomes) > 0 {
			s.historicalCodex = newHistoricalConfigResolver(codexDaemon, catalogStore, providerHomes[0])
		}
		s.historicalClaude = newHistoricalClaudeResolver(catalogStore, s.defaults.claudeBin)
	}
	// The catalog API reads runtime truth through the narrow runtimeSource
	// seam; the server implements it over its own sessions map.
	s.runtime = s
	// Fleet transitions: the materialized sidebar's changes feed the in-memory
	// bookmark log at /api/v1/fleet/changes. Loss-tolerant by design — a
	// consumer that misses the window re-snapshots.
	if catalogService != nil {
		s.sidebar = newSidebarMaterializedState()
		s.fleet = startFleetWatcher(context.Background(), s.sidebar)
	}
	if store, err := openThreadStarsStore(config.Paths.ThreadStarsDB); err != nil {
		log.Printf("thread stars store unavailable: %v", err)
	} else {
		s.threadStars = store
	}
	if store, err := openThreadPinsStore(config.Paths.ThreadPinsDB); err != nil {
		log.Printf("thread pins store unavailable: %v", err)
	} else {
		s.threadPins = store
	}
	if store, err := feedback.Open(config.Paths.FeedbackDB); err != nil {
		log.Printf("feedback store unavailable: %v", err)
	} else {
		s.feedback = store
	}
	if completedTurns != nil {
		s.completedTurns = newCompletedTurnSource(completedTurns)
	}
	if config.HeartbeatMaxWakesPerHour < 1 {
		log.Fatal("heartbeat-max-wakes-per-hour must be positive")
	}
	if store, err := openHeartbeatStore(config.Paths.HeartbeatDB); err != nil {
		log.Printf("heartbeat store unavailable: %v", err)
	} else {
		if deleted, err := store.PruneDecisionLog(time.Now().AddDate(0, 0, -30)); err != nil {
			log.Printf("heartbeat decision retention unavailable: %v", err)
		} else if deleted > 0 {
			log.Printf("heartbeat decision retention removed %d old skip rows", deleted)
		}
		s.heartbeat = newHeartbeatService(s, store, config.HeartbeatEnabled, log.Printf)
		s.heartbeat.MaxWakesHour = config.HeartbeatMaxWakesPerHour
		log.Printf("heartbeat store ready (scheduler enabled=%v; max wakes/hour=%d)", config.HeartbeatEnabled, config.HeartbeatMaxWakesPerHour)
	}
	if s.historicalCodex != nil && catalog != nil {
		go s.historicalCodex.WarmCatalog(context.Background(), catalog)
	}
	if s.historicalClaude != nil && catalog != nil {
		go s.historicalClaude.WarmCatalog(context.Background(), catalog)
	}
	// Every stored attachment comes back as a live session so a deploy restart
	// never leaves threads detached (boot_reattach.go). HTTP opens first; until
	// its registration each thread reports "starting".
	s.prepareBootReattach()
	if err := s.startSidebarMaterializer(context.Background(), time.Now()); err != nil {
		log.Printf("sidebar materialization unavailable: %v", err)
	}
	return &application{
		config: config, server: s, absCwd: absCwd, serverLog: serverLog,
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
	return listenerHandlersWithLoopback(compressResponses(a.server.refuseWritesWhileStopping(mux)), a.remoteAuth.Guard, nil)
}

func (a *application) run(localHandler, remoteHandler http.Handler) {
	config, s := a.config, a.server
	stopSignals := make(chan os.Signal, 1)
	signal.Notify(stopSignals, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-stopSignals
		started := time.Now()
		report := s.stop(stopDeliveryWait, stopDeadline)
		s.restart.writeUnsettled("interrupted", true)
		log.Printf("nano stopped in %s: %s", time.Since(started).Round(time.Millisecond), report)
		os.Exit(0)
	}()
	listenAddress := fmt.Sprintf("127.0.0.1:%d", config.Port)
	log.Printf("nano listening on http://%s (default cwd %s, codex daemon %s, claude shim %s, grok %s)",
		listenAddress, a.absCwd, config.CodexWS, s.defaults.claudeShimWS, s.defaults.grokBin)
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
	config, s := a.config, a.server
	serveErrors := make(chan error, 3)
	go func() { serveErrors <- http.Serve(listener, localHandler) }()
	s.restart.markHTTPOpen()
	go func() {
		s.reattachOnBoot()
		// Features that prompt hosted threads by themselves start after every
		// stored thread is back. HTTP opens first so requests can prioritize them.
		startSubscriptionDispatcher(s)
		if s.heartbeat != nil {
			s.heartbeat.Start(context.Background())
		}
	}()
	if config.RemoteAuthPort != 0 {
		if a.remoteAuth.RemoteHost() == "" {
			log.Fatal("-remote-auth-host is required when -remote-auth-port is enabled")
		}
		remoteAddress := fmt.Sprintf("127.0.0.1:%d", config.RemoteAuthPort)
		log.Printf("nano protected remote listener on http://%s for host %s", remoteAddress, a.remoteAuth.RemoteHost())
		remoteServer := &http.Server{
			Addr:              remoteAddress,
			Handler:           remoteHandler,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		go func() { serveErrors <- remoteServer.ListenAndServe() }()
	}
	if config.LocalTLSPort != 0 {
		// The same trusted handler over TLS, so browsers get HTTP/2: one
		// connection carries every pane's stream instead of hitting the
		// six-connection limit browsers keep per HTTP/1.1 host.
		tlsAddress := fmt.Sprintf("127.0.0.1:%d", config.LocalTLSPort)
		tlsDir := filepath.Join(config.Paths.DataDir, "tls")
		log.Printf("nano trusted local listener on https://%s", tlsAddress)
		tlsServer := &http.Server{Addr: tlsAddress, Handler: localHandler}
		go func() {
			serveErrors <- tlsServer.ListenAndServeTLS(filepath.Join(tlsDir, "localhost.pem"), filepath.Join(tlsDir, "localhost-key.pem"))
		}()
	}
	return <-serveErrors
}

// launchd sends SIGKILL 5 s after SIGTERM (the job sets no ExitTimeOut). The
// stop ends well before that; deploy.sh also waits only 10 s for the new
// process.
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
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	var inFlight []chan struct{}
	for _, sess := range sessions {
		sess.sendMu.Lock()
		for _, op := range sess.sendOps {
			select {
			case <-op.done:
			default:
				inFlight = append(inFlight, op.done)
			}
		}
		sess.sendMu.Unlock()
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
	return nanofiles.CanonicalProspective(path)
}

func (s *server) handleSessionsList(w http.ResponseWriter, _ *http.Request) {
	// The position comes first: a change after it is either in this answer or
	// replayed over it, never missed.
	var position *runtimePosition
	if s.runtimeEvents != nil {
		at := s.runtimeEvents.position()
		position = &at
	}
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()

	listed := s.listedSessions(sessions)
	live := make(map[attachmentKey]bool, len(listed))
	for _, sum := range listed {
		if sum.ThreadID != "" && sum.Status != sessionStatusExited {
			live[attachmentKey{provider: sum.Provider, nativeThreadID: sum.ThreadID}] = true
		}
	}
	httpapi.WriteJSON(w, map[string]any{
		"sessions":      listed,
		"detached":      s.detachedSessionViews(live),
		"runtimeStream": position,
		"defaults": map[string]string{
			"cwd": s.defaults.cwd, "model": s.defaults.model,
			"sandbox": s.defaults.sandbox, "approval": s.defaults.approval,
			"allowedTools": s.defaults.allowedTools, "claudeBin": s.defaults.claudeBin,
			"grokBin": s.defaults.grokBin, "grokLeaderSocket": s.defaults.grokLeaderSocket,
		},
	})
}

// listedSessions is the session list's rows for sessions, oldest first. The
// list read and the runtime event that names joined sessions share it, so a
// browser holds the same row either way. Every per-session fact is read in
// bounded batches, never once per session: views, attention, badges and names.
func (s *server) listedSessions(sessions []*session) []listedSession {
	var summaryDB *sql.DB
	if s.controlMetadata != nil {
		summaryDB = s.controlMetadata.readPool()
	}
	hostedIDs := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		if sess.hostedThreadID != "" {
			hostedIDs = append(hostedIDs, sess.hostedThreadID)
		}
	}
	views, viewsErr := s.controlMetadata.viewsFrom(summaryDB, hostedIDs)
	needsAttention := s.agentExecutions.sessionsNeedingAttention()
	summaries := make([]sessionSummary, 0, len(sessions))
	for _, sess := range sessions {
		view, found := views[sess.hostedThreadID]
		viewErr := viewsErr
		if viewErr == nil && !found {
			viewErr = sql.ErrNoRows
		}
		sum := sess.summaryWith(view, viewErr, sess.hostedThreadID != "" && needsAttention[sess.hostedThreadID])
		summaries = append(summaries, sum)
	}
	s.attachStarredOnSessions(summaries)
	sort.Slice(summaries, func(i, j int) bool { return timestampBefore(summaries[i].CreatedAt, summaries[j].CreatedAt) })
	listed := make([]listedSession, len(summaries))
	for i, sum := range summaries {
		listed[i] = listedSession{sessionSummary: sum, Metadata: listMetadata(sum.Metadata)}
	}
	return listed
}

// detachedSessionViews lists the attachments whose thread has no live
// session. Right after a restart these include threads the boot loop has not
// reached yet (boot_reattach.go), marked "starting"; the rest are records
// whose resume failed (daemon down, thread gone), offered as one-click
// re-attachable. Absent the store, the list is empty. Kept out of s.sessions
// by design, so they carry no runtime overlay in the Explorer.
func (s *server) detachedSessionViews(live map[attachmentKey]bool) []detachedSessionView {
	var records []attachmentRecord
	if s.attachments != nil {
		records = s.attachments.detachedExcept(live)
	}
	return s.detachedViewsOf(records)
}

// detachedViewsOf is the list rows of these detached records.
func (s *server) detachedViewsOf(records []attachmentRecord) []detachedSessionView {
	var summaryDB *sql.DB
	if s.controlMetadata != nil {
		summaryDB = s.controlMetadata.readPool()
	}
	detachedIDs := make([]string, 0, len(records))
	for _, record := range records {
		if record.HostedThreadID != "" {
			detachedIDs = append(detachedIDs, record.HostedThreadID)
		}
	}
	detachedViews, detachedErr := s.controlMetadata.viewsFrom(summaryDB, detachedIDs)
	detached := []detachedSessionView{}
	for _, record := range records {
		view := detachedSessionView{attachmentRecord: record}
		if s.boot.starting(record.key()) {
			view.Status = "starting"
		}
		// These retired JSON fields must never leak back into a hosted
		// response as a plausible current setting.
		view.Cwd, view.Model, view.TurnServiceTier = "", "", ""
		if record.HostedThreadID == "" || s.controlMetadata == nil || s.controlMetadataErr != "" {
			view.MetadataUnavailable = record.HostedThreadID != ""
		} else if metadata, ok := detachedViews[record.HostedThreadID]; ok && detachedErr == nil {
			view.Metadata = listMetadata(&metadata)
		} else {
			view.MetadataUnavailable = true
		}
		detached = append(detached, view)
	}
	return detached
}

type listedSession struct {
	sessionSummary
	Metadata *listedMetadata `json:"metadata,omitempty"`
}

type listedMetadata struct {
	Revision int64                          `json:"revision"`
	Fields   map[string]listedMetadataField `json:"fields"`
}

type listedMetadataField struct {
	DisplayValue json.RawMessage `json:"displayValue,omitempty"`
}

func listMetadata(view *hostedMetadataView) *listedMetadata {
	if view == nil {
		return nil
	}
	fields := make(map[string]listedMetadataField, len(view.Fields))
	for name, field := range view.Fields {
		fields[name] = listedMetadataField{DisplayValue: field.DisplayValue}
	}
	return &listedMetadata{Revision: view.Revision, Fields: fields}
}

func (s *server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	// Share admission serialization with hosted and legacy explicit close.
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	var req spawnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Provider == "" {
		httpapi.WriteHTTPError(w, "body must be {\"provider\": \"codex\"|\"claude\"|\"grok\", ...spawn options}", http.StatusBadRequest)
		return
	}
	sess, err := s.spawnSession(req)
	if err != nil {
		if errors.Is(err, errHostedControlUnavailable) {
			message := err.Error()
			if s.executionControlErr != "" {
				message += ": " + s.executionControlErr
			} else if s.controlMetadataErr != "" {
				message += ": " + s.controlMetadataErr
			}
			httpapi.WriteHTTPError(w, message, http.StatusServiceUnavailable)
			return
		}
		// Frozen contract: a resume onto an already-attached thread is a
		// 409 whose body carries the session to jump to.
		var attached *threadAttachedError
		if errors.As(err, &attached) {
			httpapi.WriteErrorEnvelope(w, http.StatusConflict, httpapi.ErrorEnvelope{Error: httpapi.Error{
				Code: "thread_attached", Message: "Thread is already attached", Details: map[string]string{"existingSessionId": attached.ExistingSessionID},
			}})

			return
		}
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	sum := sess.summary() // snapshot under the state lock; claude's threadID lands async
	log.Printf("spawned %s session %s (thread %q, cwd %s)", sum.Provider, sum.ID, sum.ThreadID, sum.Cwd)
	httpapi.WriteJSON(w, sum)
}

func (s *server) sessionOperation(operation func(http.ResponseWriter, *http.Request, *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("sessionId")
		s.mu.Lock()
		sess := s.sessions[id]
		s.mu.Unlock()
		if sess == nil {
			httpapi.WriteHTTPError(w, "no such session", http.StatusNotFound)
			return
		}
		operation(w, r, sess)
	}
}

func (s *server) handleSessionClose(w http.ResponseWriter, _ *http.Request, sess *session) {
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	if err := s.closeExplicitSession(sess); err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	log.Printf("closed %s session %s", sess.provider, sess.id)
	httpapi.WriteJSON(w, map[string]bool{"closed": true})
}

// handleAttachmentScoped handles DELETE /api/attachments/{nativeThreadId}:
// stop a detached Claude process without re-attaching or replaying it.
// Ambiguous provider identity is refused rather than broadening the target.
func (s *server) handleAttachmentScoped(w http.ResponseWriter, r *http.Request) {
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	nativeID := r.PathValue("nativeThreadId")
	if nativeID == "" {
		httpapi.WriteHTTPError(w, "native thread id required", http.StatusBadRequest)
		return
	}
	forget := r.URL.Query().Get("forget")
	if forget != "" && (forget != "unconfirmed" || r.URL.Query().Get("provider") != "claude" || !r.URL.Query().Has("expectedShimSessionId") || !r.URL.Query().Has("expectedAttachedAt")) {
		httpapi.WriteHTTPError(w, "unconfirmed forget requires Claude and the captured attachment identity", http.StatusBadRequest)
		return
	}
	if s.attachments != nil {
		provider := strings.TrimSpace(r.URL.Query().Get("provider"))
		var matches []attachmentRecord
		for _, record := range s.attachments.records() {
			if record.NativeThreadID == nativeID && (provider == "" || record.Provider == provider) {
				matches = append(matches, record)
			}
		}
		if len(matches) > 1 {
			httpapi.WriteHTTPError(w, "provider required for ambiguous attachment", http.StatusConflict)
			return
		}
		if len(matches) == 1 {
			var err error
			if forget == "unconfirmed" {
				err = s.forgetUnconfirmedClaude(matches[0], r.URL.Query().Get("expectedShimSessionId"), r.URL.Query().Get("expectedAttachedAt"))
			} else {
				err = s.closeExplicitAttachment(matches[0])
			}
			if err != nil {
				httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
				return
			}
		}
		// The dismissed row's detached overlay just disappeared; nudge open
		// Explorers the same way bind/unbind does.
		s.notifyRuntime()
	}
	if forget == "unconfirmed" {
		httpapi.WriteJSON(w, map[string]any{"forgotten": true, "processState": "unknown"})
		return
	}
	httpapi.WriteJSON(w, map[string]bool{"forgotten": true})
}

// handleSpawnPreview returns the exact protocol payload a spawn request
// would produce — built by the same resolveSpawn/codexSessionRequest/claudeArgs
// code that spawnSession uses, so the New Window dialog's JSON pane shows
// the wire truth, not a frontend reconstruction.
func (s *server) handleSpawnPreview(w http.ResponseWriter, r *http.Request) {
	var req spawnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Provider == "" {
		httpapi.WriteHTTPError(w, "body must be a spawn request", http.StatusBadRequest)
		return
	}
	resolved, err := s.resolveSpawn(req)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch resolved.Provider {
	case "codex":
		method, params := codexSessionRequest(resolved)
		out := map[string]any{
			"provider":      "codex",
			"method":        method,
			"params":        params,
			"turnOverrides": map[string]any{},
		}
		if resolved.Mode != "resume" {
			// The model rides thread/start at spawn time, so it is not a turn
			// override yet — the pane's per-turn control introduces one later.
			out["turnOverrides"] = codexTurnOverrides("", resolved.Effort, "", s.defaults.reasoningSummary)
		}
		httpapi.WriteJSON(w, out)
	case "claude":
		httpapi.WriteJSON(w, map[string]any{
			"provider": "claude",
			"argv":     claudeCommand(resolved),
		})
	case "grok":
		method, params := grokSessionRequest(resolved)
		httpapi.WriteJSON(w, map[string]any{
			"provider": "grok",
			"argv":     grokCommand(resolved),
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
	models, err := s.codex.fetchModels(r.URL.Query().Get("refresh") == "true")
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	models, err = ensureCodexAstraModel(models)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(models)
}

// ensureCodexAstraModel keeps the public catalog aligned with a model the
// installed Codex runtime already accepts, even while model/list lags behind.
// Once the provider advertises Astra, its response remains byte-for-byte
// authoritative so Nano cannot shadow newer metadata.
func ensureCodexAstraModel(models json.RawMessage) (json.RawMessage, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(models, &body); err != nil {
		return nil, fmt.Errorf("decode model/list: %w", err)
	}

	var data []json.RawMessage
	if err := json.Unmarshal(body["data"], &data); err != nil {
		return nil, fmt.Errorf("decode model/list data: %w", err)
	}
	for _, raw := range data {
		var model struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(raw, &model); err != nil {
			return nil, fmt.Errorf("decode model/list entry: %w", err)
		}
		if model.Model == "gpt-6-astra" {
			return models, nil
		}
	}

	data = append(data, json.RawMessage(`{
		"id":"gpt-6-astra",
		"model":"gpt-6-astra",
		"displayName":"GPT-6-Astra",
		"description":"Our most capable model for complex, demanding work.",
		"hidden":false,
		"defaultReasoningEffort":"low",
		"supportedReasoningEfforts":[
			{"reasoningEffort":"low"},
			{"reasoningEffort":"medium"},
			{"reasoningEffort":"high"},
			{"reasoningEffort":"xhigh"},
			{"reasoningEffort":"max"},
			{"reasoningEffort":"ultra"}
		]
	}`))
	encodedData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode model/list data: %w", err)
	}
	body["data"] = encodedData
	merged, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode model/list: %w", err)
	}
	return merged, nil
}

func (s *server) handleGrokModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	models, err := fetchGrokModels(ctx, s.defaults)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(models)
}

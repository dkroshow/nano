package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/agentapi"
	"github.com/dkroshow/nano/internal/config"
	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/diagnostics"
	"github.com/dkroshow/nano/internal/edge/stateopen"
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
	state "github.com/dkroshow/nano/internal/state/store"
	"github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/usage"
	codexwire "github.com/dkroshow/nano/internal/wire/codex"
)

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
	var catalog sidebar.CatalogReader
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
	var completedTurns *hostedmodel.CompletedTurnStore
	var attachments *hostedstore.AttachmentStore
	var controlMetadata *hostedstore.MetadataStore
	var controlMetadataErr string
	var executionControlErr string
	var sendAttempts *prompts.LegacyJournal
	var queuedTurns *prompts.LegacyQueue
	var turnStore *prompts.Store
	var agentExecutions *hostedstore.ExecutionStore
	var mail *featuremail.Store
	var usageLimits *usage.Store
	if catalogPath != "" {
		attachPath := cfg.Paths.Attachments
		store, err := hostedstore.NewAttachmentStore(attachPath)
		if err != nil {
			log.Printf("attachment store unavailable (%s): %v", attachPath, err)
		} else {
			attachments = store
			log.Printf("attachment store using %s", attachPath)
		}
		stores, err := stateopen.OpenRuntimeStoresAt(context.Background(), state.StorePaths{
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
			controlMetadata, agentExecutions = stores.Control, stores.Executions
			queuedTurns, sendAttempts, completedTurns, turnStore = stores.Queue, stores.Attempts, stores.Completed, stores.Turns
			state.StartSizeWatch(stores.Control.CanonicalDatabase(), stores.Queue.Path())
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
		usageStore, usagePath, err := usage.OpenConfigured(cfg.UsageEnabled, cfg.Paths.UsageDB)
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
		dropLiveTextDeltas: config.LiveTextDeltasDisabled(),
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
		browserDiagnostics:    diagnostics.NewStore(diagnostics.DefaultCapacity, sessionruntime.NewID()),
		transcriptValidations: observability.NewValidationStore(observability.DefaultValidationCapacity),
		transcriptAudits:      observability.NewAuditStore(),
		transcriptAuditEvents: observability.NewAuditHub(),
		usageLimits:           usageLimits,
		Registry:              sessionruntime.NewRegistry(),
	}
	s.deliveryExpiries = sessionruntime.NewDeliveryExpiries(deliveryExpiryWindow, s.Registry.PublishProviderRuntime)
	s.restart = hostedmodel.NewRestartTracker(filepath.Join(cfg.Paths.DataDir, hostedmodel.RestartsFileName), buildStamp, s.browserDiagnostics.BootID(), processStarted)
	if err := agentapi.SyncMailNotices(s.mail, s.notices); err != nil {
		log.Fatalf("initialize mail notices: %v", err)
	}
	if catalogService != nil && controlMetadata != nil {
		catalogService.SetCreationNameSource(controlMetadata.CreationThreadNames)
	}
	s.hostMonitor = observability.NewHostMonitor(filepath.Join(cfg.Paths.DataDir, "health"), filepath.Join(cfg.Paths.DataDir, "deploys.jsonl"))
	go s.hostMonitor.Run(context.Background())
	if usageLimits != nil {
		s.usageLimitsPoller = newUsageLimitsPoller(usageLimits, codexDaemon, s.defaults, cfg.UsagePollInterval)
		go s.usageLimitsPoller.Run(context.Background())
	}
	if catalogStore != nil {
		if providerHomes, err := config.ProviderHomes(); err != nil {
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
		s.completedTurns = hostedmodel.NewCompletedTurnSource(completedTurns)
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
	// Owners capture complete sources once, after every store has been opened.
	s.attachmentsControl = newAttachmentCoordinator(s)
	s.hostedControl = newHostedController(s)
	s.agent = newAgentController(s)
	if agentExecutions != nil {
		agentExecutions.UseSavedResults(s.attachmentCoordinator().SavedOperationResult, s.attachmentCoordinator().SavedOperationResultFinished)
	}
	// Every stored attachment comes back as a live session so a deploy restart
	// never leaves threads detached (boot_reattach.go). HTTP opens first; until
	// its registration each thread reports "starting".
	s.attachmentCoordinator().PrepareBoot()
	if err := s.sidebarOwner().StartSidebarMaterializer(context.Background(), time.Now()); err != nil {
		log.Printf("sidebar materialization unavailable: %v", err)
	}
	if catalogService != nil {
		catalogService.SetTranscriptRepairPriority(s.attachmentCoordinator().TranscriptRepairPriority)
		catalogService.SetTranscriptChanged(s.attachmentCoordinator().ConfirmDurableThread)
	}
	return &application{
		config: cfg, server: s, absCwd: absCwd, serverLog: serverLog,
		catalogStore: catalogStore, catalogService: catalogService,
		remoteAuthStore: remoteAuthStore, remoteAuth: remoteAuth,
	}
}

package app

import (
	"net/http"
	"sort"
	"strings"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/observability"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
	"github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/usage"
)

type applicationRoute struct {
	Method    string
	Path      string
	Operation string
	Handler   http.HandlerFunc
}

func applicationRoutes(s *server) []applicationRoute {
	var indexCatalog threadindex.Index
	if s.catalog != nil {
		indexCatalog = s.catalog
	}
	index := threadindex.NewHandlers(indexCatalog)
	monitor := s.monitorHandlers()
	audits := observability.NewAuditHandlers(s.hostedCatalog(), s.transcriptAudits, s.transcriptAuditEvents, embeddedBundleName())
	browser := observability.NewBrowserHandlers(s.browserDiagnostics, s.transcriptValidations, s.controlMetadata, s.controlMetadataErr)
	accountUsage := &usage.Handlers{Store: s.usageLimits, Poller: s.usageLimitsPoller}
	sessionRoute := func(operation func(http.ResponseWriter, *http.Request, *sessions.Session)) http.HandlerFunc {
		return s.SessionOperation(operation)
	}
	routes := []applicationRoute{
		{http.MethodGet, "/healthz", "health.read", s.handleHealth},
		{http.MethodGet, "/api/sessions", "sessions.list", s.sidebarOwner().SessionsHandler(s.defaults)},
		{http.MethodPost, "/api/sessions", "sessions.create", s.hostedController().HandleLegacyCreate},
		{http.MethodPost, "/api/sessions/preview", "sessions.preview", s.attachmentCoordinator().HandleSpawnPreview},
		{http.MethodDelete, "/api/sessions/{sessionId}", "sessions.close", sessionRoute(s.hostedController().HandleLegacyClose)},
		{http.MethodGet, "/api/sessions/{sessionId}/events", "sessions.events.stream", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) { value.HandleEvents(w, r) })},
		{http.MethodGet, "/api/sessions/{sessionId}/events/page", "sessions.events.page", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleEventsPage(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/conversation", "sessions.conversation.read", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleConversation(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/conversation/history", "sessions.conversation.history", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			func(w http.ResponseWriter, r *http.Request, sess *sessions.Session) {
				index.HandleConversationHistory(w, r, sess.ThreadID())
			}(w, r, value)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/conversation/transcript", "sessions.conversation.transcript", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			func(w http.ResponseWriter, r *http.Request, sess *sessions.Session) {
				index.HandleConversationHistory(w, r, sess.ThreadID())
			}(w, r, value)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/conversation/events", "sessions.conversation.events", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleConversationEvents(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/instruction-files", "sessions.instructions.list", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleInstructionFiles(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/instruction-files/read", "sessions.instructions.read", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleInstructionFileRead(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/instruction-files/mtime", "sessions.instructions.mtime", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleInstructionFileMtime(w, r)
		})},
		{http.MethodPut, "/api/sessions/{sessionId}/instruction-files/write", "sessions.instructions.write", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleInstructionFileWrite(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/state", "sessions.state.read", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) { value.HandleState(w, r) })},
		{http.MethodGet, "/api/sessions/{sessionId}/mcp", "sessions.mcp.list", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleMCPServers(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/skills", "sessions.skills.list", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) { value.HandleSkills(w, r) })},
		{http.MethodPut, "/api/sessions/{sessionId}/name", "sessions.name.update", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			func(w http.ResponseWriter, r *http.Request, sess *sessions.Session) {
				var namer threadindex.Namer
				if s.catalog != nil {
					namer = s.catalog
				}
				s.attachmentCoordinator().HandleSessionName(w, r, sess, namer)
			}(w, r, value)
		})},
		{http.MethodPost, "/api/sessions/{sessionId}/turn", "sessions.turn.submit", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) { value.HandleTurn(w, r) })},
		{http.MethodGet, "/api/sessions/{sessionId}/turn-delivery", "sessions.turnDelivery.list", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleTurnDeliveryList(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/turn-delivery/{clientActionId}", "sessions.turnDelivery.read", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleTurnDeliveryRead(w, r, r.PathValue("clientActionId"))
		})},
		{http.MethodDelete, "/api/sessions/{sessionId}/turn-delivery/{clientActionId}", "sessions.turnDelivery.cancel", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleTurnDeliveryCancel(w, r, r.PathValue("clientActionId"))
		})},
		{http.MethodPost, "/api/sessions/{sessionId}/turn-delivery/{clientActionId}/resend", "sessions.turnDelivery.resend", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleTurnDeliveryResend(w, r, r.PathValue("clientActionId"))
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/turn-options", "sessions.turnOptions.read", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleTurnOptionsRead(w, r)
		})},
		{http.MethodPost, "/api/sessions/{sessionId}/turn-options", "sessions.turnOptions.update", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleTurnOptionsUpdate(w, r)
		})},
		{http.MethodPost, "/api/sessions/{sessionId}/interrupt", "sessions.turn.interrupt", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleInterrupt(w, r)
		})},
		{http.MethodPost, "/api/sessions/{sessionId}/transcript-repair", "sessions.transcript.repair", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleTranscriptRepair(w, r)
		})},
		{http.MethodPost, "/api/sessions/{sessionId}/interactions/{requestId}", "sessions.interactions.answer", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessions.Session) {
			value.HandleInteraction(w, r, r.PathValue("requestId"))
		})},
		{http.MethodGet, "/api/codex/models", "models.codex.list", s.attachmentCoordinator().HandleCodexModels},
		{http.MethodGet, "/api/grok/models", "models.grok.list", s.attachmentCoordinator().HandleGrokModels},
		{http.MethodDelete, "/api/attachments/{nativeThreadId}", "attachments.forget", s.hostedController().HandleAttachmentScoped},
		{http.MethodGet, "/api/v1/build", "build.read", handleBuildInfo},
		{http.MethodGet, "/api/v1/connection-test", "connection.test", httpapi.HandleConnectionTest},
		{http.MethodGet, "/api/v1/monitor", "monitor.read", monitor.HandleMonitor},
		{http.MethodGet, "/api/v1/monitor/ingest-receipts", "monitor.ingestReceipts", monitor.HandleIngestReceipts},
		{http.MethodGet, "/api/v1/restarts", "restarts.read", s.restart.HandleRestarts},
		{http.MethodGet, "/api/v1/heartbeat/attachments", "heartbeat.attachments.list", s.agentController().HandleHeartbeatAttachmentsList},
		{http.MethodPost, "/api/v1/heartbeat/attachments", "heartbeat.attachments.create", s.agentController().HandleHeartbeatAttachmentCreate},
		{http.MethodGet, "/api/v1/heartbeat/attachments/{attachmentId}", "heartbeat.attachments.read", s.agentController().HandleHeartbeatAttachmentRead},
		{http.MethodPatch, "/api/v1/heartbeat/attachments/{attachmentId}", "heartbeat.attachments.update", s.agentController().HandleHeartbeatAttachmentUpdate},
		{http.MethodDelete, "/api/v1/heartbeat/attachments/{attachmentId}", "heartbeat.attachments.remove", s.agentController().HandleHeartbeatAttachmentRemove},
		{http.MethodPost, "/api/v1/heartbeat/attachments/{attachmentId}/pause", "heartbeat.attachments.pause", s.agentController().HandleHeartbeatAttachmentPause},
		{http.MethodPost, "/api/v1/heartbeat/attachments/{attachmentId}/resume", "heartbeat.attachments.resume", s.agentController().HandleHeartbeatAttachmentResume},
		{http.MethodGet, "/api/v1/heartbeat/script-templates", "heartbeat.templates.list", s.agentController().HandleHeartbeatScriptTemplates},
		{http.MethodGet, "/api/v1/heartbeat/scripts", "heartbeat.scripts.list", s.agentController().HandleHeartbeatScripts},
		{http.MethodGet, "/api/v1/heartbeat/scripts/{scriptId}", "heartbeat.scripts.read", s.agentController().HandleHeartbeatScriptRead},
		{http.MethodGet, "/api/v1/heartbeat/scripts/{scriptId}/{action}", "heartbeat.scripts.unsupported", s.agentController().HandleHeartbeatScriptUnsupported},
		{http.MethodGet, "/api/v1/heartbeat/log", "heartbeat.log.read", s.agentController().HandleHeartbeatLog},
		{http.MethodGet, "/api/v1/messages/{messageId}", "messages.read", s.agentController().HandleAgentMessageRead},
		{http.MethodPost, "/api/v1/messages/{messageId}/status", "messages.disposition.update", s.agentController().HandleAgentMessageDisposition},
		{http.MethodPost, "/api/v1/messages/{messageId}/edit", "messages.edit", s.agentController().HandleAgentMessageEditRoute},
		{http.MethodGet, "/api/v1/agent-messages", "messages.global.list", s.agentController().HandleAgentMessagesGlobal},
		{http.MethodGet, "/api/v1/turn-operations", "turnOperations.list", s.hostedViews().HandleOperationsCollection},
		{http.MethodGet, "/api/v1/turn-operations/changes", "turnOperations.changes", s.hostedViews().HandleOperationChanges},
		{http.MethodGet, "/api/v1/turn-operations/{operationId}", "turnOperations.read", s.hostedViews().HandleOperationRead},
		{http.MethodDelete, "/api/v1/turn-operations/{operationId}", "turnOperations.cancel", s.hostedController().HandleOperationCancel},
		{http.MethodGet, "/api/v1/usage-limits", "usageLimits.read", accountUsage.HandleSummary},
		{http.MethodGet, "/api/v1/usage-limits/detail", "usageLimits.detail", accountUsage.HandleDetail},
		{http.MethodGet, "/api/v1/browser-diagnostics", "browserDiagnostics.list", browser.HandleBrowserDiagnosticsList},
		{http.MethodPost, "/api/v1/browser-diagnostics", "browserDiagnostics.record", browser.HandleBrowserDiagnosticsRecord},
		{http.MethodGet, "/api/v1/feedback", "feedback.list", s.agentController().HandleFeedbackList},
		{http.MethodPost, "/api/v1/feedback", "feedback.create", s.agentController().HandleFeedbackCreate},
		{http.MethodGet, "/api/v1/feedback/{feedbackId}", "feedback.read", s.agentController().HandleFeedbackRead},
		{http.MethodGet, "/api/v1/browser-validations", "browserValidations.list", browser.HandleTranscriptValidationsList},
		{http.MethodPost, "/api/v1/browser-validations", "browserValidations.record", browser.HandleTranscriptValidationRecord},
		{http.MethodGet, "/api/v1/transcript-validations", "transcriptValidations.list", browser.HandleTranscriptValidationsList},
		{http.MethodPost, "/api/v1/transcript-validations", "transcriptValidations.record", browser.HandleTranscriptValidationRecord},
		{http.MethodPost, "/api/v1/transcript-audits", "transcriptAudits.create", audits.HandleTranscriptAuditCreate},
		{http.MethodGet, "/api/v1/transcript-audits/{auditId}", "transcriptAudits.read", func(w http.ResponseWriter, r *http.Request) {
			audits.HandleTranscriptAuditRead(w, r, r.PathValue("auditId"))
		}},
		{http.MethodGet, "/api/v1/transcript-audits/{auditId}/reference", "transcriptAudits.reference", func(w http.ResponseWriter, r *http.Request) {
			audits.HandleTranscriptAuditReference(w, r, r.PathValue("auditId"))
		}},
		{http.MethodPost, "/api/v1/transcript-audits/{auditId}/result", "transcriptAudits.result", func(w http.ResponseWriter, r *http.Request) {
			audits.HandleTranscriptAuditResult(w, r, r.PathValue("auditId"))
		}},
		{http.MethodGet, "/api/v1/thread-references/{reference}", "threadReferences.resolve", s.hostedIdentity().HandleThreadReference},
		{http.MethodGet, "/api/v1/identity/resolve", "identity.resolve", s.hostedIdentity().HandleIdentityResolve},
		{http.MethodPost, "/api/v1/maintenance/retired-hosted-records", "maintenance.retiredHostedRecords", s.hostedController().HandleRetirement},
		{http.MethodPost, "/api/v1/maintenance/creation-names", "maintenance.creationNames", s.creationNames().HandleRepair},
		{http.MethodPost, "/api/v1/maintenance/stale-prompts", "maintenance.stalePrompts", s.attachmentCoordinator().HandleStalePrompts},
		{http.MethodGet, "/api/v1/files/read", "files.read", s.fileHandlers().HandleRead},
		{http.MethodGet, "/api/v1/files/mtime", "files.mtime", s.fileHandlers().HandleMtime},
		{http.MethodPut, "/api/v1/files/write", "files.write", s.fileHandlers().HandleWrite},
		{http.MethodGet, "/api/v1/search/messages", "search.messages", index.HandleConversationSearchMessages},
		{http.MethodGet, "/api/v1/search/status", "search.status", index.HandleConversationSearchStatus},
		{http.MethodGet, "/api/v1/usage/threads", "usage.threads", index.HandleThreadUsageList},
		{http.MethodGet, "/api/v1/threads", "threads.list", s.catalogViews().HandleThreads},
		{http.MethodGet, "/api/v1/threads/{threadId}", "threads.read", func(w http.ResponseWriter, r *http.Request) {
			s.catalogViews().HandleThread(w, r, r.PathValue("threadId"))
		}},
		{http.MethodGet, "/api/v1/threads/{threadId}/transcript/body/{itemId}", "threads.transcript.body", func(w http.ResponseWriter, r *http.Request) {
			index.HandleThreadTranscriptBody(w, r, r.PathValue("threadId"), r.PathValue("itemId"))
		}},
		{http.MethodGet, "/api/v1/threads/{threadId}/transcript/evidence/{itemId}", "threads.transcript.evidence", func(w http.ResponseWriter, r *http.Request) {
			index.HandleThreadTranscriptEvidence(w, r, r.PathValue("threadId"), r.PathValue("itemId"))
		}},
		{http.MethodGet, "/api/v1/threads/{threadId}/usage", "threads.usage", func(w http.ResponseWriter, r *http.Request) { index.HandleThreadUsage(w, r, r.PathValue("threadId")) }},
		{http.MethodGet, "/api/v1/threads/{threadId}/transcript", "threads.transcript", func(w http.ResponseWriter, r *http.Request) {
			index.HandleThreadTranscript(w, r, r.PathValue("threadId"))
		}},
		{http.MethodGet, "/api/v1/threads/{threadId}/messages", "threads.messages", func(w http.ResponseWriter, r *http.Request) {
			s.catalogViews().HandleMessages(w, r, r.PathValue("threadId"))
		}},
		{http.MethodPut, "/api/v1/threads/{threadId}/starred", "threads.starred", func(w http.ResponseWriter, r *http.Request) {
			s.sidebarOwner().HandleThreadStarred(w, r, r.PathValue("threadId"))
		}},
		{http.MethodPut, "/api/v1/threads/{threadId}/name", "threads.name", index.HandleThreadNameSet},
		{http.MethodDelete, "/api/v1/threads/{threadId}/name", "threads.name.clear", index.HandleThreadNameClear},
		{http.MethodGet, "/api/v1/thread-index/status", "threadIndex.status", index.HandleCatalogIndexStatus},
		{http.MethodPost, "/api/v1/thread-index/refresh", "threadIndex.refresh", index.HandleCatalogRefresh},
		{http.MethodGet, "/api/v1/thread-index/events", "threadIndex.events", s.catalogViews().HandleEvents},
		{http.MethodGet, "/api/v1/fleet/changes", "fleet.changes", s.sidebarOwner().HandleFleetChanges},
		{http.MethodGet, "/api/v1/sidebar/threads", "sidebar.threads", s.sidebarOwner().HandleSidebarThreads},
		{http.MethodGet, "/api/v1/sidebar/threads/{threadId}/history", "sidebar.threadHistory", s.sidebarOwner().HandleSidebarThreadHistory},
		{http.MethodGet, "/api/v1/pins/monitor", "pins.monitor", s.hostedPins().HandleMonitor},
	}
	routes = append(routes, apiDiscoveryRoutes()...)
	routes = append(routes, hostedThreadRoutes(s, "/api/v1/hosted-threads")...)
	routes = append(routes, applicationRoute{http.MethodGet, "/api/v1/warm-transcripts", "warmTranscripts.stream", s.sidebarOwner().HandleWarmTranscripts})
	// The first agent-only spelling remains a compatibility alias for the same
	// operations and records; it does not define a second dispatch path.
	routes = append(routes, hostedThreadRoutes(s, "/api/v1/agent-sessions")...)
	for i := range routes {
		routes[i].Handler = s.requests.Timed(routes[i].Operation, routes[i].Handler)
	}
	return routes
}

func hostedThreadRoutes(s *server, prefix string) []applicationRoute {
	h := s.hostedController().HostedOperation
	routes := []applicationRoute{
		{http.MethodGet, prefix, "hostedThreads.list", s.hostedViews().HandleSessionsList},
		{http.MethodPost, prefix, "hostedThreads.create", s.hostedController().HandleCreate},
		{http.MethodGet, prefix + "/{threadRef}", "hostedThreads.read", h(s.hostedController().HandleRead)},
		{http.MethodDelete, prefix + "/{threadRef}", "hostedThreads.delete", h(s.hostedController().HandleDelete)},
		{http.MethodPost, prefix + "/{threadRef}/close", "hostedThreads.close", h(s.hostedController().HandleClose)},
		{http.MethodGet, prefix + "/{threadRef}/metadata", "hostedThreads.metadata.read", h(s.hostedViews().HandleMetadataRead)},
		{http.MethodGet, prefix + "/{threadRef}/metadata/changes", "hostedThreads.metadata.changes", h(s.hostedViews().HandleMetadataChanges)},
		{http.MethodGet, prefix + "/{threadRef}/metadata/events", "hostedThreads.metadata.events", h(s.hostedViews().HandleMetadataEvents)},
		{http.MethodGet, prefix + "/{threadRef}/permission-events", "hostedThreads.permissionEvents", h(s.hostedViews().HandlePermissionEvents)},
		{http.MethodGet, prefix + "/{threadRef}/availability", "hostedThreads.availability", h(func(w http.ResponseWriter, r *http.Request, record threads.SessionRecord) {
			s.hostedViews().HandleAvailability(w, r, record.ID)
		})},
		{http.MethodPost, prefix + "/{threadRef}/messages", "hostedThreads.messages.send", h(s.agentController().HandleHostedThreadMessageSend)},
		{http.MethodGet, prefix + "/{threadRef}/inbox", "hostedThreads.inbox", h(s.agentController().HandleHostedThreadInbox)},
		{http.MethodGet, prefix + "/{threadRef}/outbox", "hostedThreads.outbox", h(s.agentController().HandleHostedThreadOutbox)},
		{http.MethodGet, prefix + "/{threadRef}/family", "hostedThreads.family", h(s.hostedViews().HandleFamily)},
		{http.MethodGet, prefix + "/{threadRef}/family/close", "hostedThreads.familyClose.preview", h(func(w http.ResponseWriter, _ *http.Request, record threads.SessionRecord) {
			s.hostedController().HandleBulkClosePreview(w, record)
		})},
		{http.MethodPost, prefix + "/{threadRef}/family/close", "hostedThreads.familyClose.apply", h(s.hostedController().HandleBulkClose)},
		{http.MethodPut, prefix + "/{threadRef}/presentation", "hostedThreads.presentation", h(s.hostedController().HandlePresentation)},
		{http.MethodPut, prefix + "/{threadRef}/subscriptions", "hostedThreads.subscriptions", h(s.hostedController().HandleSubscriptions)},
		{http.MethodPut, prefix + "/{threadRef}/sidebar-placement", "hostedThreads.sidebarPlacement", h(s.hostedController().HandleSidebarPlacement)},
		{http.MethodPut, prefix + "/{threadRef}/assignment", "hostedThreads.assignment", h(s.hostedController().HandleAssignment)},
		{http.MethodPost, prefix + "/{threadRef}/detach", "hostedThreads.detach", h(s.hostedController().HandleDetach)},
		{http.MethodPost, prefix + "/{threadRef}/prioritize", "hostedThreads.prioritize", h(s.attachmentCoordinator().HandlePrioritize)},
		{http.MethodPost, prefix + "/{threadRef}/turns", "hostedThreads.turns.submit", h(func(w http.ResponseWriter, r *http.Request, record threads.SessionRecord) {
			s.hostedController().HandleTurnSubmit(w, r, record.ID)
		})},
		{http.MethodGet, prefix + "/{threadRef}/completed-turns", "hostedThreads.completedTurns", h(func(w http.ResponseWriter, r *http.Request, record threads.SessionRecord) {
			s.attachmentCoordinator().CompletedFeed().HandleCompletedTurns(w, r, record.ID)
		})},
		{http.MethodGet, prefix + "/{threadRef}/conversation", "hostedThreads.conversation", h(s.hostedController().HandleHostedThreadConversation)},
		{http.MethodGet, prefix + "/{threadRef}/conversation/events", "hostedThreads.conversation.events", h(s.hostedController().HandleHostedThreadConversationEvents)},
		{http.MethodGet, prefix + "/{threadRef}/live", "hostedThreads.live", h(s.hostedController().HandleHostedThreadLive)},
		{http.MethodPost, prefix + "/{threadRef}/interactions/{requestId}", "hostedThreads.interactions.answer", h(s.hostedController().HandleHostedThreadInteraction)},
		{http.MethodPost, prefix + "/{threadRef}/interrupt", "hostedThreads.interrupt", h(s.hostedController().HandleHostedThreadInterrupt)},
		{http.MethodGet, prefix + "/{threadRef}/pins", "hostedThreads.pins.list", h(s.hostedPins().HandleList)},
		{http.MethodPost, prefix + "/{threadRef}/pins", "hostedThreads.pins.create", h(s.hostedPins().HandleCreate)},
		{http.MethodGet, prefix + "/{threadRef}/pins/candidates", "hostedThreads.pins.candidates", h(s.hostedPins().HandleCandidates)},
		{http.MethodGet, prefix + "/{threadRef}/pins/{pinId}", "hostedThreads.pins.read", h(s.hostedPins().HandleRead)},
		{http.MethodGet, prefix + "/{threadRef}/pins/{pinId}/context", "hostedThreads.pins.context", h(s.hostedPins().HandleContext)},
	}
	return routes
}

func mountExactRoutes(mux *http.ServeMux, routes []applicationRoute) {
	paths := make(map[string]map[string]struct{}, len(routes))
	for _, route := range routes {
		mux.HandleFunc(route.Method+" "+route.Path, route.Handler)
		if paths[route.Path] == nil {
			paths[route.Path] = map[string]struct{}{}
		}
		paths[route.Path][route.Method] = struct{}{}
	}
	for path, allowed := range paths {
		allow := make([]string, 0, len(allowed))
		for method := range allowed {
			allow = append(allow, method)
		}
		sort.Strings(allow)
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
			if _, ok := allowed[method]; ok {
				continue
			}
			mux.HandleFunc(method+" "+path, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Allow", strings.Join(allow, ", "))
				httpapi.WriteHTTPError(w, "method not allowed", http.StatusMethodNotAllowed)
			})
		}
	}
}

func mountRoutes(mux *http.ServeMux, routes []applicationRoute) {
	mountExactRoutes(mux, routes)
	mux.HandleFunc("/api/{path...}", func(w http.ResponseWriter, r *http.Request) {
		httpapi.WriteNotFound(w)
	})
}

func applicationHandler(s *server) http.Handler {
	mux := http.NewServeMux()
	mountRoutes(mux, applicationRoutes(s))
	return mux
}

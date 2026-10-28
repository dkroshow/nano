package app

import (
	"net/http"
	"sort"
	"strings"

	"github.com/dkroshow/nano/internal/httpapi"
	sessionruntime "github.com/dkroshow/nano/internal/sessions"
)

type applicationRoute struct {
	Method    string
	Path      string
	Operation string
	Handler   http.HandlerFunc
}

func applicationRoutes(s *server) []applicationRoute {
	sessionRoute := func(operation func(http.ResponseWriter, *http.Request, *sessionruntime.Session)) http.HandlerFunc {
		return s.sessionOperation(operation)
	}
	routes := []applicationRoute{
		{http.MethodGet, "/healthz", "health.read", s.handleHealth},
		{http.MethodGet, "/api/sessions", "sessions.list", s.handleSessionsList},
		{http.MethodPost, "/api/sessions", "sessions.create", s.handleSessionCreate},
		{http.MethodPost, "/api/sessions/preview", "sessions.preview", s.handleSpawnPreview},
		{http.MethodDelete, "/api/sessions/{sessionId}", "sessions.close", sessionRoute(s.handleSessionClose)},
		{http.MethodGet, "/api/sessions/{sessionId}/events", "sessions.events.stream", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) { value.HandleEvents(w, r) })},
		{http.MethodGet, "/api/sessions/{sessionId}/events/page", "sessions.events.page", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleEventsPage(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/conversation", "sessions.conversation.read", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleConversation(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/conversation/history", "sessions.conversation.history", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			s.handleConversationHistory(w, r, value)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/conversation/transcript", "sessions.conversation.transcript", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			s.handleConversationHistory(w, r, value)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/conversation/events", "sessions.conversation.events", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleConversationEvents(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/instruction-files", "sessions.instructions.list", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleInstructionFiles(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/instruction-files/read", "sessions.instructions.read", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleInstructionFileRead(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/instruction-files/mtime", "sessions.instructions.mtime", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleInstructionFileMtime(w, r)
		})},
		{http.MethodPut, "/api/sessions/{sessionId}/instruction-files/write", "sessions.instructions.write", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleInstructionFileWrite(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/state", "sessions.state.read", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) { value.HandleState(w, r) })},
		{http.MethodGet, "/api/sessions/{sessionId}/mcp", "sessions.mcp.list", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleMCPServers(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/skills", "sessions.skills.list", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) { value.HandleSkills(w, r) })},
		{http.MethodPut, "/api/sessions/{sessionId}/name", "sessions.name.update", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			s.handleSessionName(w, r, value)
		})},
		{http.MethodPost, "/api/sessions/{sessionId}/turn", "sessions.turn.submit", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) { value.HandleTurn(w, r) })},
		{http.MethodGet, "/api/sessions/{sessionId}/turn-delivery", "sessions.turnDelivery.list", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleTurnDeliveryList(w, r)
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/turn-delivery/{clientActionId}", "sessions.turnDelivery.read", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleTurnDeliveryRead(w, r, r.PathValue("clientActionId"))
		})},
		{http.MethodDelete, "/api/sessions/{sessionId}/turn-delivery/{clientActionId}", "sessions.turnDelivery.cancel", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleTurnDeliveryCancel(w, r, r.PathValue("clientActionId"))
		})},
		{http.MethodPost, "/api/sessions/{sessionId}/turn-delivery/{clientActionId}/resend", "sessions.turnDelivery.resend", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleTurnDeliveryResend(w, r, r.PathValue("clientActionId"))
		})},
		{http.MethodGet, "/api/sessions/{sessionId}/turn-options", "sessions.turnOptions.read", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleTurnOptionsRead(w, r)
		})},
		{http.MethodPost, "/api/sessions/{sessionId}/turn-options", "sessions.turnOptions.update", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleTurnOptionsUpdate(w, r)
		})},
		{http.MethodPost, "/api/sessions/{sessionId}/interrupt", "sessions.turn.interrupt", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleInterrupt(w, r)
		})},
		{http.MethodPost, "/api/sessions/{sessionId}/transcript-repair", "sessions.transcript.repair", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			s.handleTranscriptRepair(w, r, value)
		})},
		{http.MethodPost, "/api/sessions/{sessionId}/interactions/{requestId}", "sessions.interactions.answer", sessionRoute(func(w http.ResponseWriter, r *http.Request, value *sessionruntime.Session) {
			value.HandleInteraction(w, r, r.PathValue("requestId"))
		})},
		{http.MethodGet, "/api/codex/models", "models.codex.list", s.handleCodexModels},
		{http.MethodGet, "/api/grok/models", "models.grok.list", s.handleGrokModels},
		{http.MethodDelete, "/api/attachments/{nativeThreadId}", "attachments.forget", s.handleAttachmentScoped},
		{http.MethodGet, "/api/v1/build", "build.read", handleBuildInfo},
		{http.MethodGet, "/api/v1/connection-test", "connection.test", httpapi.HandleConnectionTest},
		{http.MethodGet, "/api/v1/monitor", "monitor.read", s.handleMonitor},
		{http.MethodGet, "/api/v1/monitor/ingest-receipts", "monitor.ingestReceipts", s.handleIngestReceipts},
		{http.MethodGet, "/api/v1/restarts", "restarts.read", s.handleRestarts},
		{http.MethodGet, "/api/v1/heartbeat/attachments", "heartbeat.attachments.list", s.handleHeartbeatAttachmentsList},
		{http.MethodPost, "/api/v1/heartbeat/attachments", "heartbeat.attachments.create", s.handleHeartbeatAttachmentCreate},
		{http.MethodGet, "/api/v1/heartbeat/attachments/{attachmentId}", "heartbeat.attachments.read", s.handleHeartbeatAttachmentRead},
		{http.MethodPatch, "/api/v1/heartbeat/attachments/{attachmentId}", "heartbeat.attachments.update", s.handleHeartbeatAttachmentUpdate},
		{http.MethodDelete, "/api/v1/heartbeat/attachments/{attachmentId}", "heartbeat.attachments.remove", s.handleHeartbeatAttachmentRemove},
		{http.MethodPost, "/api/v1/heartbeat/attachments/{attachmentId}/pause", "heartbeat.attachments.pause", s.handleHeartbeatAttachmentPause},
		{http.MethodPost, "/api/v1/heartbeat/attachments/{attachmentId}/resume", "heartbeat.attachments.resume", s.handleHeartbeatAttachmentResume},
		{http.MethodGet, "/api/v1/heartbeat/script-templates", "heartbeat.templates.list", s.handleHeartbeatScriptTemplates},
		{http.MethodGet, "/api/v1/heartbeat/scripts", "heartbeat.scripts.list", s.handleHeartbeatScripts},
		{http.MethodGet, "/api/v1/heartbeat/scripts/{scriptId}", "heartbeat.scripts.read", s.handleHeartbeatScriptRead},
		{http.MethodGet, "/api/v1/heartbeat/scripts/{scriptId}/{action}", "heartbeat.scripts.unsupported", s.handleHeartbeatScriptUnsupported},
		{http.MethodGet, "/api/v1/heartbeat/log", "heartbeat.log.read", s.handleHeartbeatLog},
		{http.MethodGet, "/api/v1/messages/{messageId}", "messages.read", s.handleAgentMessageRead},
		{http.MethodPost, "/api/v1/messages/{messageId}/status", "messages.disposition.update", s.handleAgentMessageDisposition},
		{http.MethodPost, "/api/v1/messages/{messageId}/edit", "messages.edit", s.handleAgentMessageEditRoute},
		{http.MethodGet, "/api/v1/agent-messages", "messages.global.list", s.handleAgentMessagesGlobal},
		{http.MethodGet, "/api/v1/turn-operations", "turnOperations.list", s.handleTurnOperationsCollection},
		{http.MethodGet, "/api/v1/turn-operations/changes", "turnOperations.changes", s.handleTurnOperationChanges},
		{http.MethodGet, "/api/v1/turn-operations/{operationId}", "turnOperations.read", s.handleTurnOperationRead},
		{http.MethodDelete, "/api/v1/turn-operations/{operationId}", "turnOperations.cancel", s.handleTurnOperationCancel},
		{http.MethodGet, "/api/v1/usage-limits", "usageLimits.read", s.handleUsageLimits},
		{http.MethodGet, "/api/v1/usage-limits/detail", "usageLimits.detail", s.handleUsageLimitsDetail},
		{http.MethodGet, "/api/v1/browser-diagnostics", "browserDiagnostics.list", s.handleBrowserDiagnosticsList},
		{http.MethodPost, "/api/v1/browser-diagnostics", "browserDiagnostics.record", s.handleBrowserDiagnosticsRecord},
		{http.MethodGet, "/api/v1/feedback", "feedback.list", s.handleFeedbackList},
		{http.MethodPost, "/api/v1/feedback", "feedback.create", s.handleFeedbackCreate},
		{http.MethodGet, "/api/v1/feedback/{feedbackId}", "feedback.read", s.handleFeedbackRead},
		{http.MethodGet, "/api/v1/browser-validations", "browserValidations.list", s.handleTranscriptValidationsList},
		{http.MethodPost, "/api/v1/browser-validations", "browserValidations.record", s.handleTranscriptValidationRecord},
		{http.MethodGet, "/api/v1/transcript-validations", "transcriptValidations.list", s.handleTranscriptValidationsList},
		{http.MethodPost, "/api/v1/transcript-validations", "transcriptValidations.record", s.handleTranscriptValidationRecord},
		{http.MethodPost, "/api/v1/transcript-audits", "transcriptAudits.create", s.handleTranscriptAuditCreate},
		{http.MethodGet, "/api/v1/transcript-audits/{auditId}", "transcriptAudits.read", func(w http.ResponseWriter, r *http.Request) {
			s.handleTranscriptAuditRead(w, r, r.PathValue("auditId"))
		}},
		{http.MethodGet, "/api/v1/transcript-audits/{auditId}/reference", "transcriptAudits.reference", func(w http.ResponseWriter, r *http.Request) {
			s.handleTranscriptAuditReference(w, r, r.PathValue("auditId"))
		}},
		{http.MethodPost, "/api/v1/transcript-audits/{auditId}/result", "transcriptAudits.result", func(w http.ResponseWriter, r *http.Request) {
			s.handleTranscriptAuditResult(w, r, r.PathValue("auditId"))
		}},
		{http.MethodGet, "/api/v1/thread-references/{reference}", "threadReferences.resolve", s.handleThreadReference},
		{http.MethodGet, "/api/v1/identity/resolve", "identity.resolve", s.handleIdentityResolve},
		{http.MethodPost, "/api/v1/maintenance/retired-hosted-records", "maintenance.retiredHostedRecords", s.handleRetiredHostedRecords},
		{http.MethodPost, "/api/v1/maintenance/creation-names", "maintenance.creationNames", s.handleCreationNameRepair},
		{http.MethodPost, "/api/v1/maintenance/stale-prompts", "maintenance.stalePrompts", s.handleStalePrompts},
		{http.MethodGet, "/api/v1/files/read", "files.read", s.handleGeneralFileRead},
		{http.MethodGet, "/api/v1/files/mtime", "files.mtime", s.handleGeneralFileMtime},
		{http.MethodPut, "/api/v1/files/write", "files.write", s.handleGeneralFileWrite},
		{http.MethodGet, "/api/v1/search/messages", "search.messages", s.handleConversationSearchMessages},
		{http.MethodGet, "/api/v1/search/status", "search.status", s.handleConversationSearchStatus},
		{http.MethodGet, "/api/v1/usage/threads", "usage.threads", s.handleThreadUsageList},
		{http.MethodGet, "/api/v1/threads", "threads.list", s.handleCatalogThreads},
		{http.MethodGet, "/api/v1/threads/{threadId}", "threads.read", func(w http.ResponseWriter, r *http.Request) { s.handleCatalogThreadRead(w, r, r.PathValue("threadId")) }},
		{http.MethodGet, "/api/v1/threads/{threadId}/transcript/body/{itemId}", "threads.transcript.body", func(w http.ResponseWriter, r *http.Request) {
			s.handleThreadTranscriptBody(w, r, r.PathValue("threadId"), r.PathValue("itemId"))
		}},
		{http.MethodGet, "/api/v1/threads/{threadId}/transcript/evidence/{itemId}", "threads.transcript.evidence", func(w http.ResponseWriter, r *http.Request) {
			s.handleThreadTranscriptEvidence(w, r, r.PathValue("threadId"), r.PathValue("itemId"))
		}},
		{http.MethodGet, "/api/v1/threads/{threadId}/usage", "threads.usage", func(w http.ResponseWriter, r *http.Request) { s.handleThreadUsage(w, r, r.PathValue("threadId")) }},
		{http.MethodGet, "/api/v1/threads/{threadId}/transcript", "threads.transcript", func(w http.ResponseWriter, r *http.Request) { s.handleThreadTranscript(w, r, r.PathValue("threadId")) }},
		{http.MethodGet, "/api/v1/threads/{threadId}/messages", "threads.messages", func(w http.ResponseWriter, r *http.Request) { s.handleThreadMessages(w, r, r.PathValue("threadId")) }},
		{http.MethodPut, "/api/v1/threads/{threadId}/starred", "threads.starred", func(w http.ResponseWriter, r *http.Request) { s.handleThreadStarred(w, r, r.PathValue("threadId")) }},
		{http.MethodPut, "/api/v1/threads/{threadId}/name", "threads.name", s.handleThreadNameSet},
		{http.MethodDelete, "/api/v1/threads/{threadId}/name", "threads.name.clear", s.handleThreadNameClear},
		{http.MethodGet, "/api/v1/thread-index/status", "threadIndex.status", s.handleCatalogIndexStatus},
		{http.MethodPost, "/api/v1/thread-index/refresh", "threadIndex.refresh", s.handleCatalogRefresh},
		{http.MethodGet, "/api/v1/thread-index/events", "threadIndex.events", s.handleCatalogIndexEvents},
		{http.MethodGet, "/api/v1/fleet/changes", "fleet.changes", s.handleFleetChanges},
		{http.MethodGet, "/api/v1/sidebar/threads", "sidebar.threads", s.handleSidebarThreads},
		{http.MethodGet, "/api/v1/sidebar/threads/{threadId}/history", "sidebar.threadHistory", s.handleSidebarThreadHistory},
		{http.MethodGet, "/api/v1/pins/monitor", "pins.monitor", s.handleThreadPinMonitor},
	}
	routes = append(routes, apiDiscoveryRoutes()...)
	routes = append(routes, hostedThreadRoutes(s, "/api/v1/hosted-threads")...)
	routes = append(routes, applicationRoute{http.MethodGet, "/api/v1/warm-transcripts", "warmTranscripts.stream", s.handleWarmTranscripts})
	// The first agent-only spelling remains a compatibility alias for the same
	// operations and records; it does not define a second dispatch path.
	routes = append(routes, hostedThreadRoutes(s, "/api/v1/agent-sessions")...)
	for i := range routes {
		routes[i].Handler = s.requests.Timed(routes[i].Operation, routes[i].Handler)
	}
	return routes
}

func hostedThreadRoutes(s *server, prefix string) []applicationRoute {
	h := s.hostedThreadOperation
	routes := []applicationRoute{
		{http.MethodGet, prefix, "hostedThreads.list", s.handleAgentSessionsList},
		{http.MethodPost, prefix, "hostedThreads.create", s.handleAgentSessionCreate},
		{http.MethodGet, prefix + "/{threadRef}", "hostedThreads.read", h(s.handleHostedThreadRead)},
		{http.MethodDelete, prefix + "/{threadRef}", "hostedThreads.delete", h(s.handleHostedThreadDelete)},
		{http.MethodPost, prefix + "/{threadRef}/close", "hostedThreads.close", h(s.handleHostedThreadClose)},
		{http.MethodGet, prefix + "/{threadRef}/metadata", "hostedThreads.metadata.read", h(s.handleHostedThreadMetadataRead)},
		{http.MethodGet, prefix + "/{threadRef}/metadata/changes", "hostedThreads.metadata.changes", h(s.handleHostedThreadMetadataChanges)},
		{http.MethodGet, prefix + "/{threadRef}/metadata/events", "hostedThreads.metadata.events", h(s.handleHostedThreadMetadataEvents)},
		{http.MethodGet, prefix + "/{threadRef}/permission-events", "hostedThreads.permissionEvents", h(s.handleHostedThreadPermissionEvents)},
		{http.MethodGet, prefix + "/{threadRef}/availability", "hostedThreads.availability", h(func(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
			s.handleHostedThreadAvailability(w, r, record.ID)
		})},
		{http.MethodPost, prefix + "/{threadRef}/messages", "hostedThreads.messages.send", h(s.handleHostedThreadMessageSend)},
		{http.MethodGet, prefix + "/{threadRef}/inbox", "hostedThreads.inbox", h(s.handleHostedThreadInbox)},
		{http.MethodGet, prefix + "/{threadRef}/outbox", "hostedThreads.outbox", h(s.handleHostedThreadOutbox)},
		{http.MethodGet, prefix + "/{threadRef}/family", "hostedThreads.family", h(s.handleHostedThreadFamily)},
		{http.MethodGet, prefix + "/{threadRef}/family/close", "hostedThreads.familyClose.preview", h(func(w http.ResponseWriter, _ *http.Request, record agentSessionRecord) {
			s.handleBulkCloseTreePreview(w, record)
		})},
		{http.MethodPost, prefix + "/{threadRef}/family/close", "hostedThreads.familyClose.apply", h(s.handleBulkCloseTree)},
		{http.MethodPut, prefix + "/{threadRef}/presentation", "hostedThreads.presentation", h(s.handleHostedThreadPresentation)},
		{http.MethodPut, prefix + "/{threadRef}/subscriptions", "hostedThreads.subscriptions", h(s.handleHostedThreadSubscriptions)},
		{http.MethodPut, prefix + "/{threadRef}/sidebar-placement", "hostedThreads.sidebarPlacement", h(s.handleHostedThreadSidebarPlacement)},
		{http.MethodPut, prefix + "/{threadRef}/assignment", "hostedThreads.assignment", h(s.handleHostedThreadAssignment)},
		{http.MethodPost, prefix + "/{threadRef}/detach", "hostedThreads.detach", h(s.handleHostedThreadDetach)},
		{http.MethodPost, prefix + "/{threadRef}/prioritize", "hostedThreads.prioritize", h(s.handleHostedThreadPrioritize)},
		{http.MethodPost, prefix + "/{threadRef}/turns", "hostedThreads.turns.submit", h(func(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
			s.handleAgentTurnSubmit(w, r, record.ID)
		})},
		{http.MethodGet, prefix + "/{threadRef}/completed-turns", "hostedThreads.completedTurns", h(func(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
			s.handleCompletedTurns(w, r, record.ID)
		})},
		{http.MethodGet, prefix + "/{threadRef}/conversation", "hostedThreads.conversation", h(s.handleHostedThreadConversation)},
		{http.MethodGet, prefix + "/{threadRef}/conversation/events", "hostedThreads.conversation.events", h(s.handleHostedThreadConversationEvents)},
		{http.MethodGet, prefix + "/{threadRef}/live", "hostedThreads.live", h(s.handleHostedThreadLive)},
		{http.MethodPost, prefix + "/{threadRef}/interactions/{requestId}", "hostedThreads.interactions.answer", h(s.handleHostedThreadInteraction)},
		{http.MethodPost, prefix + "/{threadRef}/interrupt", "hostedThreads.interrupt", h(s.handleHostedThreadInterrupt)},
		{http.MethodGet, prefix + "/{threadRef}/pins", "hostedThreads.pins.list", h(s.handleThreadPinList)},
		{http.MethodPost, prefix + "/{threadRef}/pins", "hostedThreads.pins.create", h(s.handleThreadPinCreate)},
		{http.MethodGet, prefix + "/{threadRef}/pins/candidates", "hostedThreads.pins.candidates", h(s.handleThreadPinCandidates)},
		{http.MethodGet, prefix + "/{threadRef}/pins/{pinId}", "hostedThreads.pins.read", h(s.handleThreadPinRead)},
		{http.MethodGet, prefix + "/{threadRef}/pins/{pinId}/context", "hostedThreads.pins.context", h(s.handleThreadPinContext)},
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

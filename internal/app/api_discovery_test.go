package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/dkroshow/nano/api"
	"github.com/dkroshow/nano/internal/history"
)

func TestAPICapabilitiesDescribesTheAgentWorkflow(t *testing.T) {
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mountRoutes(mux, apiDiscoveryRoutes())
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("capabilities status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	if got := rec.Header().Get("Link"); got != `</api/v1/openapi.json>; rel="service-desc"` {
		t.Fatalf("discovery link = %q", got)
	}
	var document struct {
		SchemaVersion int    `json:"schemaVersion"`
		APIVersion    string `json:"apiVersion"`
		Discovery     struct {
			Capabilities string `json:"capabilities"`
			OpenAPI      string `json:"openapi"`
		} `json:"discovery"`
		Providers []string `json:"providers"`
		Identity  struct {
			Preferred        string   `json:"preferred"`
			WhenInDoubt      string   `json:"whenInDoubt"`
			ThreadRefAccepts []string `json:"threadRefAccepts"`
			Identifiers      map[string]struct {
				Durability      string `json:"durability"`
				ProvisionalForm string `json:"provisionalForm"`
				Use             string `json:"use"`
			} `json:"identifiers"`
			AgentCreation struct {
				RequiredField            string   `json:"requiredField"`
				NormalizedResponseFields []string `json:"normalizedResponseFields"`
			} `json:"agentCreation"`
		} `json:"identity"`
		SelfIdentity struct {
			BlockFields []string        `json:"blockFields"`
			Guarantee   map[string]bool `json:"guarantee"`
		} `json:"selfIdentity"`
		Capabilities []struct {
			ID     string `json:"id"`
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"capabilities"`
		Conversation struct {
			DefaultView string   `json:"defaultView"`
			Views       []string `json:"views"`
		} `json:"conversation"`
		TurnOperations struct {
			States []string `json:"states"`
		} `json:"turnOperations"`
		Coordination struct {
			DefaultMode        string `json:"defaultMode"`
			AfterSubmit        string `json:"afterSubmit"`
			ForegroundWait     string `json:"foregroundWait"`
			CompletionDelivery string `json:"completionDelivery"`
		} `json:"coordination"`
		Subscriptions struct {
			Field                      string `json:"field"`
			Fallback                   string `json:"fallback"`
			ReservedClientActionPrefix string `json:"reservedClientActionPrefix"`
		} `json:"subscriptions"`
		Messaging struct {
			Status       string `json:"status"`
			Boundary     string `json:"boundary"`
			Send         string `json:"send"`
			Read         string `json:"read"`
			Disposition  string `json:"disposition"`
			UnreadFooter string `json:"unreadFooter"`
		} `json:"messaging"`
		Examples   map[string]json.RawMessage `json:"examples"`
		Quickstart []struct {
			Step int    `json:"step"`
			Use  string `json:"use"`
		} `json:"quickstart"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.SchemaVersion != 1 || document.APIVersion != "v1" {
		t.Fatalf("versions = schema:%d api:%q", document.SchemaVersion, document.APIVersion)
	}
	if document.Discovery.Capabilities != "/api/v1/capabilities" || document.Discovery.OpenAPI != "/api/v1/openapi.json" {
		t.Fatalf("discovery = %#v", document.Discovery)
	}
	if len(document.Providers) != 3 || document.Providers[0] != "codex" || document.Providers[1] != "claude" || document.Providers[2] != "grok" {
		t.Fatalf("providers = %#v", document.Providers)
	}
	if document.Identity.Preferred != "hostedThreadId" || len(document.Identity.ThreadRefAccepts) != 4 {
		t.Fatalf("identity = %#v", document.Identity)
	}
	if document.Identity.AgentCreation.RequiredField != "invokerThreadRef" ||
		len(document.Identity.AgentCreation.NormalizedResponseFields) != 3 {
		t.Fatalf("agent creation identity = %#v", document.Identity.AgentCreation)
	}
	// The ID-disambiguation section is part of the identity feature, not doc
	// garnish: agents mix these four up unless the guide preempts it (the operator
	// directive, slice-1 design A6). It may not silently regress.
	if !strings.Contains(document.Identity.WhenInDoubt, "hostedThreadId") {
		t.Fatalf("whenInDoubt = %q, want the retain-rule naming hostedThreadId", document.Identity.WhenInDoubt)
	}
	if !strings.Contains(document.Identity.WhenInDoubt, "display name") || !strings.Contains(document.Identity.Identifiers["hostedThreadId"].Use, "Do not cite it in reports") {
		t.Fatalf("identity guide still directs agents to show hosted ids to people: %#v", document.Identity)
	}
	if len(document.Identity.Identifiers) != 4 {
		t.Fatalf("identifiers = %d entries, want the four thread ids", len(document.Identity.Identifiers))
	}
	for _, id := range []string{"hostedThreadId", "nativeThreadId", "nanoSessionId", "clientSessionId"} {
		if entry, ok := document.Identity.Identifiers[id]; !ok || entry.Durability == "" {
			t.Fatalf("identifier %q missing or lacking a durability rule", id)
		}
	}
	if !strings.Contains(document.Identity.Identifiers["nativeThreadId"].ProvisionalForm, "cs_") {
		t.Fatal("nativeThreadId identifier does not document the cs_ provisional form")
	}
	if g := document.SelfIdentity.Guarantee; !g["codex"] || !g["claude"] || g["grok"] {
		t.Fatalf("selfIdentity guarantee = %#v, want codex+claude true, grok false", g)
	}
	if len(document.SelfIdentity.BlockFields) == 0 {
		t.Fatal("selfIdentity block fields undocumented")
	}
	wantCapabilities := map[string]bool{
		"threadReferences.resolve": false,
		"identity.resolve":         false,
		"hostedThreads.list":       false, "hostedThreads.create": false, "hostedThreads.read": false,
		"hostedThreads.executionFamily":  false,
		"hostedThreads.bulkClosePreview": false,
		"hostedThreads.bulkCloseTree":    false,
		"conversation.snapshot":          false, "conversation.events": false, "turns.submit": false,
		"turns.completed": false, "transcript.window": false,
		"turnOperations.read": false, "turnOperations.cancelQueued": false,
		"turnOperations.changes": false, "turnOperations.bulkStatus": false,
		"interactions.answer": false, "turns.interrupt": false,
		"hostedThreads.close": false, "hostedThreads.detach": false,
		"hostedThreads.delete":   false,
		"historySearch.messages": false, "historySearch.status": false,
		"sidebar.threads": false,
		"threads.starred": false,
	}
	for _, capability := range document.Capabilities {
		if _, ok := wantCapabilities[capability.ID]; ok && capability.Method != "" && capability.Path != "" {
			wantCapabilities[capability.ID] = true
		}
	}
	for id, found := range wantCapabilities {
		if !found {
			t.Errorf("missing capability %q", id)
		}
	}
	if document.Conversation.DefaultView != "messages" || len(document.Conversation.Views) != 2 {
		t.Fatalf("conversation = %#v", document.Conversation)
	}
	if len(document.TurnOperations.States) != 8 || len(document.Examples) < 5 || len(document.Quickstart) < 5 {
		t.Fatalf("operation states / examples / quickstart = %#v / %#v / %#v", document.TurnOperations, document.Examples, document.Quickstart)
	}
	if document.Coordination.DefaultMode != "detached" || document.Coordination.CompletionDelivery != "pull" ||
		document.Coordination.AfterSubmit == "" || document.Coordination.ForegroundWait == "" {
		t.Fatalf("coordination = %#v", document.Coordination)
	}
	if document.Subscriptions.Field != "subscriptions" ||
		!strings.Contains(document.Subscriptions.Fallback, "/api/v1/turn-operations/changes") ||
		!strings.Contains(document.Subscriptions.ReservedClientActionPrefix, "nano:") {
		t.Fatalf("subscriptions = %#v", document.Subscriptions)
	}
	if !strings.Contains(document.Messaging.Status, "mail.sqlite") || !strings.Contains(document.Messaging.Status, "indefinite retention") || !strings.Contains(document.Messaging.Boundary, "never creates a prompt turn") ||
		!strings.Contains(document.Messaging.Send, "/messages") || !strings.Contains(document.Messaging.Read, "status=seen") ||
		!strings.Contains(document.Messaging.Send, "defaults off") || !strings.Contains(document.Messaging.Disposition, "deferred") || !strings.Contains(document.Messaging.UnreadFooter, "N unread") {
		t.Fatalf("durable inbox capability = %#v", document.Messaging)
	}
}

func TestOpenAPIDescribesEveryHostedThreadCapability(t *testing.T) {
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mountRoutes(mux, apiDiscoveryRoutes())
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Link"); got != `</api/v1/capabilities>; rel="service-meta"` {
		t.Fatalf("discovery link = %q", got)
	}
	var document struct {
		OpenAPI    string                     `json:"openapi"`
		Paths      map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.OpenAPI != "3.1.0" {
		t.Fatalf("openapi version = %q", document.OpenAPI)
	}
	want := map[string][]string{
		"/api/v1":                                                     {"get"},
		"/api/v1/capabilities":                                        {"get"},
		"/api/v1/openapi.json":                                        {"get"},
		"/api/v1/thread-references/{reference}":                       {"get"},
		"/api/v1/identity/resolve":                                    {"get"},
		"/api/v1/sidebar/threads":                                     {"get"},
		"/api/v1/sidebar/threads/{id}/history":                        {"get"},
		"/api/v1/threads/{threadId}/starred":                          {"put"},
		"/api/v1/search/messages":                                     {"get"},
		"/api/v1/search/status":                                       {"get"},
		"/api/v1/usage/threads":                                       {"get"},
		"/api/v1/threads/{id}/usage":                                  {"get"},
		"/api/v1/threads/{threadId}/transcript":                       {"get"},
		"/api/v1/hosted-threads":                                      {"get", "post"},
		"/api/v1/hosted-threads/{threadRef}":                          {"get", "delete"},
		"/api/v1/hosted-threads/{threadRef}/availability":             {"get"},
		"/api/v1/hosted-threads/{threadRef}/messages":                 {"post"},
		"/api/v1/hosted-threads/{threadRef}/inbox":                    {"get"},
		"/api/v1/hosted-threads/{threadRef}/outbox":                   {"get"},
		"/api/v1/messages/{messageId}":                                {"get"},
		"/api/v1/messages/{messageId}/status":                         {"post"},
		"/api/v1/messages/{messageId}/edit":                           {"post"},
		"/api/v1/agent-messages":                                      {"get"},
		"/api/v1/feedback":                                            {"get", "post"},
		"/api/v1/feedback/{feedbackId}":                               {"get"},
		"/api/v1/heartbeat/attachments":                               {"get", "post"},
		"/api/v1/heartbeat/attachments/{attachmentId}":                {"get", "patch", "delete"},
		"/api/v1/heartbeat/attachments/{attachmentId}/{action}":       {"post"},
		"/api/v1/heartbeat/script-templates":                          {"get"},
		"/api/v1/heartbeat/scripts":                                   {"get"},
		"/api/v1/heartbeat/scripts/{scriptId}":                        {"get"},
		"/api/v1/heartbeat/log":                                       {"get"},
		"/api/v1/hosted-threads/{threadRef}/family":                   {"get"},
		"/api/v1/hosted-threads/{threadRef}/family/close":             {"get", "post"},
		"/api/v1/hosted-threads/{threadRef}/close":                    {"post"},
		"/api/v1/hosted-threads/{threadRef}/subscriptions":            {"put"},
		"/api/v1/hosted-threads/{threadRef}/presentation":             {"put"},
		"/api/v1/hosted-threads/{threadRef}/sidebar-placement":        {"put"},
		"/api/v1/hosted-threads/{threadRef}/detach":                   {"post"},
		"/api/v1/hosted-threads/{threadRef}/metadata":                 {"get"},
		"/api/v1/hosted-threads/{threadRef}/metadata/events":          {"get"},
		"/api/v1/hosted-threads/{threadRef}/permission-events":        {"get"},
		"/api/v1/hosted-threads/{threadRef}/metadata/changes":         {"get"},
		"/api/v1/hosted-threads/{threadRef}/assignment":               {"put"},
		"/api/v1/hosted-threads/{threadRef}/conversation":             {"get"},
		"/api/v1/hosted-threads/{threadRef}/conversation/events":      {"get"},
		"/api/v1/browser-validations":                                 {"get", "post"},
		"/api/v1/transcript-audits":                                   {"post"},
		"/api/v1/transcript-audits/{auditId}":                         {"get"},
		"/api/v1/transcript-audits/{auditId}/reference":               {"get"},
		"/api/v1/transcript-audits/{auditId}/result":                  {"post"},
		"/api/v1/hosted-threads/{threadRef}/turns":                    {"post"},
		"/api/v1/hosted-threads/{threadRef}/completed-turns":          {"get"},
		"/api/v1/hosted-threads/{threadRef}/interactions/{requestId}": {"post"},
		"/api/v1/hosted-threads/{threadRef}/interrupt":                {"post"},
		"/api/v1/hosted-threads/{threadRef}/pins":                     {"get", "post"},
		"/api/v1/hosted-threads/{threadRef}/pins/candidates":          {"get"},
		"/api/v1/hosted-threads/{threadRef}/pins/{pinId}":             {"get"},
		"/api/v1/hosted-threads/{threadRef}/pins/{pinId}/context":     {"get"},
		"/api/v1/pins/monitor":                                        {"get"},
		"/api/v1/monitor":                                             {"get"},
		"/api/v1/monitor/ingest-receipts":                             {"get"},
		"/api/v1/restarts":                                            {"get"},
		"/api/v1/turn-operations":                                     {"get"},
		"/api/v1/turn-operations/changes":                             {"get"},
		"/api/v1/turn-operations/{operationId}":                       {"get", "delete"},
		"/api/v1/maintenance/creation-names":                          {"post"},
		"/api/v1/maintenance/stale-prompts":                           {"post"},
		"/api/v1/maintenance/retired-hosted-records":                  {"post"},
	}
	if len(document.Paths) != len(want) {
		t.Errorf("OpenAPI paths = %d, want exactly %d agent-facing paths", len(document.Paths), len(want))
	}
	for path, methods := range want {
		pathDocument, ok := document.Paths[path]
		if !ok {
			t.Errorf("missing OpenAPI path %q", path)
			continue
		}
		var operations map[string]json.RawMessage
		if err := json.Unmarshal(pathDocument, &operations); err != nil {
			t.Errorf("decode OpenAPI path %q: %v", path, err)
			continue
		}
		for _, method := range methods {
			operationDocument, ok := operations[method]
			if !ok {
				t.Errorf("missing OpenAPI operation %s %s", method, path)
				continue
			}
			var operation struct {
				OperationID string `json:"operationId"`
				Summary     string `json:"summary"`
			}
			if err := json.Unmarshal(operationDocument, &operation); err != nil {
				t.Errorf("decode OpenAPI operation %s %s: %v", method, path, err)
				continue
			}
			if operation.OperationID == "" || operation.Summary == "" {
				t.Errorf("incomplete OpenAPI operation %s %s: %#v", method, path, operation)
			}
		}
	}
	for _, schema := range []string{"HostedMetadataView", "HostedMetadataCurrentField", "HostedMetadataEvent", "HostedMetadataEventPage", "PermissionEvent", "PermissionEventPage", "HostedMetadataChanges", "HostedAttachment", "HostedNameProvenance", "OrchestrationAssignment", "OrchestrationAssignmentView", "OrchestrationAuditTargetRequest", "UpdateOrchestrationAssignmentRequest", "AssignmentMutationResult", "AgentMessage", "AgentMessageSendRequest", "AgentMessageStatusRequest", "AgentMessageDetail", "AgentMessagePage"} {
		if _, ok := document.Components.Schemas[schema]; !ok {
			t.Errorf("missing canonical metadata schema %q", schema)
		}
	}
	for _, schema := range []string{"APIErrorEnvelope", "APIError"} {
		if _, ok := document.Components.Schemas[schema]; !ok {
			t.Errorf("missing common API error schema %q", schema)
		}
	}
	var requestTarget, assignmentView map[string]any
	if err := json.Unmarshal(document.Components.Schemas["OrchestrationAuditTargetRequest"], &requestTarget); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(document.Components.Schemas["OrchestrationAssignmentView"], &assignmentView); err != nil {
		t.Fatal(err)
	}
	if _, ok := requestTarget["allOf"]; ok || requestTarget["properties"].(map[string]any)["unresolved"] != nil {
		t.Fatalf("request target exposes projection-only unresolved: %#v", requestTarget)
	}
	if _, ok := assignmentView["allOf"]; ok || assignmentView["properties"].(map[string]any)["eventId"] == nil {
		t.Fatalf("assignment view schema is not explicit: %#v", assignmentView)
	}

}

func TestOpenAPIRequiresAndReturnsInvocationLineage(t *testing.T) {
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mountRoutes(mux, apiDiscoveryRoutes())
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	var document struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	var create struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["CreateHostedThreadRequest"], &create); err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{}
	for _, field := range create.Required {
		required[field] = true
	}
	if !required["provider"] || !required["invokerThreadRef"] || create.Properties["invokerThreadRef"] == nil || create.Properties["subscriptions"] == nil {
		t.Fatalf("create lineage contract = required %#v, properties %#v", create.Required, create.Properties)
	}
	var hosted struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["HostedThread"], &hosted); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"invokerHostedThreadId", "invokerNativeThreadId", "invokerProvider", "subscriptions"} {
		if hosted.Properties[field] == nil {
			t.Errorf("HostedThread missing %s", field)
		}
	}
}

func TestOpenAPIExposesThreadDiagnostics(t *testing.T) {
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mountRoutes(mux, apiDiscoveryRoutes())
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi status = %d, body %s", rec.Code, rec.Body.String())
	}
	var document struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	var hostedThread struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["HostedThread"], &hostedThread); err != nil {
		t.Fatal(err)
	}
	var threadDiagnostics struct {
		Type     string `json:"type"`
		MaxItems int    `json:"maxItems"`
		Items    struct {
			Ref string `json:"$ref"`
		} `json:"items"`
	}
	if err := json.Unmarshal(hostedThread.Properties["threadDiagnostics"], &threadDiagnostics); err != nil {
		t.Fatal(err)
	}
	if threadDiagnostics.Type != "array" || threadDiagnostics.MaxItems != threadDiagnosticLimit || threadDiagnostics.Items.Ref != "#/components/schemas/ThreadDiagnostic" {
		t.Fatalf("threadDiagnostics schema = %#v", threadDiagnostics)
	}
	var threadDiagnosticSchema struct {
		Required             []string                   `json:"required"`
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties bool                       `json:"additionalProperties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["ThreadDiagnostic"], &threadDiagnosticSchema); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"code", "message", "source", "occurrences"} {
		if !slices.Contains(threadDiagnosticSchema.Required, field) || threadDiagnosticSchema.Properties[field] == nil {
			t.Errorf("thread diagnostic missing %s: %#v", field, threadDiagnosticSchema)
		}
	}
	if threadDiagnosticSchema.Properties["actionId"] == nil || threadDiagnosticSchema.AdditionalProperties {
		t.Fatalf("thread diagnostic schema = %#v", threadDiagnosticSchema)
	}
}

func TestOpenAPILocalReferencesResolve(t *testing.T) {
	var document map[string]any
	if err := json.Unmarshal(api.OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	var walk func(any)
	walk = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			if ref, ok := node["$ref"].(string); ok {
				if !strings.HasPrefix(ref, "#/") {
					t.Errorf("OpenAPI contains unsupported external reference %q", ref)
				} else if !openAPIReferenceExists(document, ref) {
					t.Errorf("OpenAPI reference does not resolve: %q", ref)
				}
			}
			for _, child := range node {
				walk(child)
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(document)
	components, _ := document["components"].(map[string]any)
	responses, _ := components["responses"].(map[string]any)
	metadataUnavailable, ok := responses["MetadataUnavailable"].(map[string]any)
	if !ok {
		t.Fatal("MetadataUnavailable must be an OpenAPI response component")
	}
	if _, ok := metadataUnavailable["content"].(map[string]any); !ok {
		t.Fatalf("MetadataUnavailable response has no content: %#v", metadataUnavailable)
	}
	if schemas, _ := components["schemas"].(map[string]any); schemas["MetadataUnavailable"] != nil {
		t.Fatal("MetadataUnavailable response was incorrectly retained in schemas")
	}
	attachmentSchema, _ := components["schemas"].(map[string]any)["HostedAttachment"].(map[string]any)
	state, _ := attachmentSchema["properties"].(map[string]any)["state"].(map[string]any)
	if _, constrained := state["enum"]; constrained || !strings.Contains(state["description"].(string), "reconnecting") {
		t.Fatalf("HostedAttachment state must be an open execution-status contract: %#v", state)
	}
	nameSchema, _ := components["schemas"].(map[string]any)["HostedNameProvenance"].(map[string]any)
	provenance, _ := nameSchema["properties"].(map[string]any)["provenance"].(map[string]any)
	values, _ := provenance["enum"].([]any)
	if len(values) != 3 || values[0] != "manual" || values[1] != string(history.ThreadNameHistoricalImport) || values[2] != string(history.ThreadNameCreation) {
		t.Fatalf("HostedNameProvenance enum = %#v", values)
	}
}

func TestOpenAPIHistoricalSearchParametersAndErrorsMatchServer(t *testing.T) {
	var document struct {
		Paths map[string]struct {
			Get struct {
				Parameters []struct {
					Name        string          `json:"name"`
					Style       string          `json:"style"`
					Explode     *bool           `json:"explode"`
					Description string          `json:"description"`
					Schema      json.RawMessage `json:"schema"`
				} `json:"parameters"`
				Responses map[string]struct {
					Content map[string]struct {
						Schema json.RawMessage `json:"schema"`
					} `json:"content"`
				} `json:"responses"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(api.OpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	search := document.Paths["/api/v1/search/messages"].Get
	params := map[string]struct {
		Style       string
		Explode     *bool
		Description string
		Schema      json.RawMessage
	}{}
	for _, parameter := range search.Parameters {
		params[parameter.Name] = struct {
			Style       string
			Explode     *bool
			Description string
			Schema      json.RawMessage
		}{parameter.Style, parameter.Explode, parameter.Description, parameter.Schema}
	}
	for _, name := range []string{"role", "provider", "kind"} {
		var schema struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(params[name].Schema, &schema); err != nil || schema.Type != "array" || params[name].Style != "form" || params[name].Explode == nil || !*params[name].Explode {
			t.Fatalf("%s repeatable query parameter = %#v schema=%s err=%v", name, params[name], params[name].Schema, err)
		}
	}
	var querySchema map[string]any
	if err := json.Unmarshal(params["q"].Schema, &querySchema); err != nil || querySchema["maxLength"] != nil || !strings.Contains(params["q"].Description, "UTF-8 bytes") {
		t.Fatalf("q byte-bound documentation = %#v description=%q err=%v", querySchema, params["q"].Description, err)
	}
	for _, code := range []string{"400", "409", "503"} {
		body, ok := search.Responses[code].Content["application/json"]
		if !ok || len(body.Schema) == 0 {
			t.Fatalf("search response %s has no JSON error envelope", code)
		}
	}
	var unavailableRef struct {
		Ref string `json:"$ref"`
	}
	if err := json.Unmarshal(search.Responses["503"].Content["application/json"].Schema, &unavailableRef); err != nil || unavailableRef.Ref != "#/components/schemas/APIErrorEnvelope" {
		t.Fatalf("503 schema = %#v, %v", unavailableRef, err)
	}
}

func openAPIReferenceExists(document map[string]any, ref string) bool {
	var current any = document
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		node, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current, ok = node[part]
		if !ok {
			return false
		}
	}
	return true
}

func TestAPIDiscoveryIsGetOnly(t *testing.T) {
	for name, path := range map[string]string{
		"capabilities": "/api/v1/capabilities",
		"openapi":      "/api/v1/openapi.json",
	} {
		rec := httptest.NewRecorder()
		applicationHandler(fixtureOwners(&server{})).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s POST status = %d", name, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodGet {
			t.Errorf("%s Allow = %q", name, got)
		}
	}
}

package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/interact"
)

func TestInteractHTTPContractMatchesOpenAPI(t *testing.T) {
	var document struct {
		Paths      map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(apiOpenAPIJSON, &document); err != nil {
		t.Fatal(err)
	}
	type contract struct {
		capability        reflect.Type
		method            string
		verb              string
		path              string
		request           reflect.Type
		requestRef        string
		response          reflect.Type
		responseRef       string
		responseFields    []string
		capabilityMethods int
	}
	contracts := []contract{
		{
			capability: reflect.TypeOf((*interact.PromptSubmitter)(nil)).Elem(), method: "SubmitPrompt",
			verb: "post", path: "/api/v1/hosted-threads/{threadRef}/turns",
			request: reflect.TypeOf(interact.SubmitPromptRequest{}), requestRef: "SubmitTurnRequest",
			response: reflect.TypeOf(interact.Turn{}), responseRef: "TurnOperation",
		},
		{
			capability: reflect.TypeOf((*interact.MessageSender)(nil)).Elem(), method: "SendMessage",
			verb: "post", path: "/api/v1/hosted-threads/{threadRef}/messages",
			request: reflect.TypeOf(interact.SendMessageRequest{}), requestRef: "AgentMessageSendRequest",
			responseRef: "AgentMessage",
		},
		{
			method: "ReadInbox",
			verb:   "get", path: "/api/v1/hosted-threads/{threadRef}/inbox",
			responseRef: "AgentMessagePage",
		},
		{
			method: "ReadOutbox",
			verb:   "get", path: "/api/v1/hosted-threads/{threadRef}/outbox",
			responseRef: "AgentMessagePage",
		},
		{
			method: "ReadMessage",
			verb:   "get", path: "/api/v1/messages/{messageId}",
			responseRef: "AgentMessageDetail",
		},
		{
			capability: reflect.TypeOf((*interact.CompletedTurnSubscriber)(nil)).Elem(), method: "SubscribeCompletedTurns",
			verb: "get", path: "/api/v1/hosted-threads/{threadRef}/completed-turns",
			response: reflect.TypeOf(interact.CompletedTurnPage{}), responseRef: "CompletedTurnPage",
		},
		{
			capability: reflect.TypeOf((*interact.TranscriptReader)(nil)).Elem(), method: "ReadTranscript",
			verb: "get", path: "/api/v1/threads/{threadId}/transcript",
			response: reflect.TypeOf(interact.TranscriptWindow{}), responseRef: "TranscriptWindow",
		},
		{
			capability: reflect.TypeOf((*interact.LifecycleReader)(nil)).Elem(), method: "ReadLifecycle",
			verb: "get", path: "/api/v1/hosted-threads/{threadRef}",
			response: reflect.TypeOf(interact.LifecycleState{}), responseRef: "HostedThread",
			capabilityMethods: 2,
		},
		{
			capability: reflect.TypeOf((*interact.LifecycleReader)(nil)).Elem(), method: "ReadAvailability",
			verb: "get", path: "/api/v1/hosted-threads/{threadRef}/availability",
			responseFields:    []string{"hostedThreadId", "state", "reasons", "facts", "observedAt"},
			capabilityMethods: 2,
		},
	}
	clientType := reflect.TypeOf((*interact.HTTPClient)(nil))
	routes := make(map[string]bool, len(contracts))
	for _, item := range contracts {
		methodCount := item.capabilityMethods
		if methodCount == 0 {
			methodCount = 1
		}
		if item.capability != nil {
			_, hasMethod := item.capability.MethodByName(item.method)
			if item.capability.NumMethod() != methodCount || !hasMethod {
				t.Errorf("%s interface methods = %v", item.capability.Name(), interfaceMethodNames(item.capability))
			}
		} else {
			if _, ok := clientType.MethodByName(item.method); !ok {
				t.Errorf("HTTPClient lacks %s", item.method)
			}
		}
		key := item.verb + " " + item.path
		if routes[key] {
			t.Errorf("interact capabilities share route %s", key)
		}
		routes[key] = true
		var path map[string]json.RawMessage
		if err := json.Unmarshal(document.Paths[item.path], &path); err != nil {
			t.Errorf("decode path %s: %v", item.path, err)
			continue
		}
		rawOperation, ok := path[item.verb]
		if !ok {
			t.Errorf("missing interact route %s", key)
			continue
		}
		var operation struct {
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
			Responses map[string]struct {
				Content map[string]struct {
					Schema struct {
						Ref        string                     `json:"$ref"`
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		}
		if err := json.Unmarshal(rawOperation, &operation); err != nil {
			t.Errorf("decode operation %s: %v", key, err)
			continue
		}
		if item.requestRef != "" {
			got := operation.RequestBody.Content["application/json"].Schema.Ref
			if got != "#/components/schemas/"+item.requestRef {
				t.Errorf("%s request schema = %q", key, got)
			}
			assertInteractJSONFields(t, item.request, item.requestRef, document.Components.Schemas[item.requestRef])
		}
		got := operation.Responses["200"].Content["application/json"].Schema.Ref
		if item.responseRef != "" && got != "#/components/schemas/"+item.responseRef {
			t.Errorf("%s response schema = %q", key, got)
		}
		if item.method == "SubmitPrompt" {
			got = operation.Responses["202"].Content["application/json"].Schema.Ref
			if got != "#/components/schemas/"+item.responseRef {
				t.Errorf("%s accepted response schema = %q", key, got)
			}
		}
		if item.response != nil {
			assertInteractJSONFields(t, item.response, item.responseRef, document.Components.Schemas[item.responseRef])
		}
		if len(item.responseFields) > 0 {
			assertOpenAPIFields(t, item.responseFields, operation.Responses["200"].Content["application/json"].Schema.Properties)
		}
	}
	if len(routes) != 9 {
		t.Fatalf("interact routes = %d, want 9", len(routes))
	}
}

func assertOpenAPIFields(t *testing.T, fields []string, properties map[string]json.RawMessage) {
	t.Helper()
	for _, field := range fields {
		if properties[field] == nil {
			t.Errorf("inline OpenAPI response lacks field %q", field)
		}
	}
}

func interfaceMethodNames(value reflect.Type) []string {
	methods := make([]string, value.NumMethod())
	for index := range methods {
		methods[index] = value.Method(index).Name
	}
	return methods
}

func assertInteractJSONFields(t *testing.T, goType reflect.Type, schemaName string, raw json.RawMessage) {
	t.Helper()
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Errorf("decode schema %s: %v", schemaName, err)
		return
	}
	for index := 0; index < goType.NumField(); index++ {
		field := goType.Field(index)
		name := field.Tag.Get("json")
		name, _, _ = strings.Cut(name, ",")
		if name == "" || name == "-" {
			continue
		}
		if schema.Properties[name] == nil {
			t.Errorf("OpenAPI schema %s lacks %s.%s json field %q", schemaName, goType.Name(), field.Name, name)
		}
	}
}

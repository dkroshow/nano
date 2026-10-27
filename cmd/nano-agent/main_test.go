package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunTurnRequiresAndSendsSender(t *testing.T) {
	if err := runTurn(context.Background(), &client{base: "http://example.test", http: http.DefaultClient}, []string{"--session", "target", "--action", "one", "--prompt", "hello"}); err == nil {
		t.Fatal("run without --sender succeeded")
	}
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/hosted-threads/target/turns" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"aop-one","status":"delivery_pending"}`))
	}))
	defer server.Close()
	if err := runTurn(context.Background(), &client{base: server.URL, http: server.Client()}, []string{"--session", "target", "--action", "one", "--sender", "sender-native", "--prompt", "hello"}); err != nil {
		t.Fatal(err)
	}
	if got := body["senderThreadRef"]; got != "sender-native" {
		t.Fatalf("senderThreadRef = %#v", got)
	}
}

func TestRunMessageSendExpandsMeAndCarriesRefs(t *testing.T) {
	t.Setenv("NANO_HOSTED_THREAD_ID", "nht_sender")
	var method, path string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"messageId":"nmsg_one"}`))
	}))
	defer server.Close()
	c := &client{base: server.URL, http: server.Client()}
	if err := runMessageSend(context.Background(), c, []string{"--to", "target name", "--from", "me", "--client-id", "finding-1", "--subject", "Finding", "--body", "Inspect it", "--ref", "issue.ref:sample-task", "--ref", "nano.thread:native-1"}); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/api/v1/hosted-threads/target name/messages" {
		t.Fatalf("request = %s %s", method, path)
	}
	if body["senderThreadRef"] != "nht_sender" || body["clientMessageId"] != "finding-1" {
		t.Fatalf("send body = %#v", body)
	}
	if refs, ok := body["refs"].([]any); !ok || len(refs) != 2 {
		t.Fatalf("refs = %#v", body["refs"])
	}
}

func TestRunMessageInboxAndAckUseSelfIdentity(t *testing.T) {
	t.Setenv("NANO_HOSTED_THREAD_ID", "nht_self")
	var requests []struct {
		Method string
		URI    string
		Body   map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry := struct {
			Method string
			URI    string
			Body   map[string]any
		}{Method: r.Method, URI: r.URL.RequestURI()}
		if r.Body != nil && r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&entry.Body)
		}
		requests = append(requests, entry)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer server.Close()
	c := &client{base: server.URL, http: server.Client()}
	if err := runMessageInbox(context.Background(), c, []string{"--as", "me", "--status", "seen", "--ref", "issue.ref:sample-task", "--peek"}); err != nil {
		t.Fatal(err)
	}
	if err := runMessageAck(context.Background(), c, []string{"nmsg_one", "--as", "me", "--status", "deferred", "--note", "waiting"}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || !strings.HasPrefix(requests[0].URI, "/api/v1/hosted-threads/nht_self/inbox?") || !strings.Contains(requests[0].URI, "actorThreadRef=nht_self") || !strings.Contains(requests[0].URI, "peek=true") {
		t.Fatalf("inbox request = %#v", requests)
	}
	if requests[1].Method != http.MethodPost || requests[1].URI != "/api/v1/messages/nmsg_one/status" || requests[1].Body["actorThreadRef"] != "nht_self" || requests[1].Body["status"] != "deferred" {
		t.Fatalf("ack request = %#v", requests[1])
	}
}

func TestMessageMeRequiresEnvironmentIdentity(t *testing.T) {
	t.Setenv("NANO_HOSTED_THREAD_ID", "")
	c := &client{base: "http://127.0.0.1:1", http: &http.Client{Timeout: time.Second}}
	if err := runMessageInbox(context.Background(), c, []string{"--as", "me"}); err == nil || !strings.Contains(err.Error(), "NANO_HOSTED_THREAD_ID") {
		t.Fatalf("inbox me error = %v", err)
	}
}

func TestRunHeartbeatAttachSendsCadenceOffsetAndTemplate(t *testing.T) {
	var path string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"hba_1","state":"active"}`))
	}))
	defer server.Close()
	c := &client{base: server.URL, http: server.Client()}
	if err := runHeartbeat(context.Background(), c, []string{"attach", "--target", "nht_target", "--cadence", "5", "--offset", "2", "--prompt", "Keep going.", "--template", "inbox_match", "--settings", `{"refKind":"issue.ref","refId":"sample-task"}`, "--actor", "Operator"}); err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/heartbeat/attachments" {
		t.Fatalf("path = %s", path)
	}
	if body["targetHostedThreadId"] != "nht_target" || body["cadenceMinutes"] != float64(5) || body["offsetMinutes"] != float64(2) {
		t.Fatalf("attach body = %#v", body)
	}
	prePing, ok := body["prePing"].(map[string]any)
	if !ok || prePing["templateId"] != "inbox_match" {
		t.Fatalf("prePing = %#v", body["prePing"])
	}
	// The retired status-agent/tag fields must never be sent again.
	for _, retired := range []string{"overridePrompt", "tagPolicy", "statusProvider", "statusModel", "statusEffort", "contextThreshold"} {
		if _, present := body[retired]; present {
			t.Fatalf("attach body still sends retired field %q: %#v", retired, body)
		}
	}
}

func TestRunHeartbeatRejectsRemovedScriptCommand(t *testing.T) {
	c := &client{base: "http://127.0.0.1:1", http: &http.Client{Timeout: time.Second}}
	err := runHeartbeat(context.Background(), c, []string{"script-create"})
	if err == nil || !strings.Contains(err.Error(), "unknown heartbeat command") {
		t.Fatalf("script command error = %v", err)
	}
}

func TestRunHeartbeatPauseResumeRemove(t *testing.T) {
	for _, tc := range []struct {
		action, method, path string
	}{
		{"pause", http.MethodPost, "/api/v1/heartbeat/attachments/hba_1/pause"},
		{"resume", http.MethodPost, "/api/v1/heartbeat/attachments/hba_1/resume"},
		{"remove", http.MethodDelete, "/api/v1/heartbeat/attachments/hba_1"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			var gotMethod, gotPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"hba_1"}`))
			}))
			defer server.Close()
			c := &client{base: server.URL, http: server.Client()}
			if err := runHeartbeat(context.Background(), c, []string{tc.action, "--id", "hba_1", "--reason", "testing", "--actor", "Operator"}); err != nil {
				t.Fatal(err)
			}
			if gotMethod != tc.method || gotPath != tc.path {
				t.Fatalf("%s = %s %s, want %s %s", tc.action, gotMethod, gotPath, tc.method, tc.path)
			}
		})
	}
}

func TestRunHeartbeatUnknownCommandsAreRejected(t *testing.T) {
	c := &client{base: "http://127.0.0.1:1", http: &http.Client{Timeout: time.Second}}
	for _, removed := range []string{"status", "hold", "release", "enable", "disable"} {
		if err := runHeartbeat(context.Background(), c, []string{removed, "--id", "hba_1"}); err == nil {
			t.Fatalf("retired heartbeat command %q was still accepted", removed)
		}
	}
}

func TestRunStartSendsRequiredInvokerReference(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"nht_worker","status":"ready","revision":1}`))
	}))
	defer server.Close()
	c := &client{base: server.URL, http: &http.Client{Timeout: time.Second}}
	if err := runStart(context.Background(), c, []string{"--session", "worker-1", "--provider", "claude", "--invoker", "native-parent", "--wait", "0"}); err != nil {
		t.Fatal(err)
	}
	if body["invokerThreadRef"] != "native-parent" {
		t.Fatalf("start body = %#v", body)
	}
}

func TestRunStartRejectsMissingInvokerReference(t *testing.T) {
	c := &client{base: "http://127.0.0.1:1", http: &http.Client{Timeout: time.Second}}
	if err := runStart(context.Background(), c, []string{"--session", "worker-1", "--provider", "codex"}); err == nil {
		t.Fatal("start accepted no invoker")
	}
}

func TestRunAssignmentSendsCompleteAssignment(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/hosted-threads/nht_worker/assignment" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"nht_worker"}`))
	}))
	defer server.Close()
	c := &client{base: server.URL, http: &http.Client{Timeout: time.Second}}
	if err := runAssignment(context.Background(), c, []string{"--session", "nht_worker", "--action", "audit-1", "--assignment-role", "auditor", "--focus", "Review it", "--related-thread", "nht_parent", "--audit-targets", "nht_target"}); err != nil {
		t.Fatal(err)
	}
	assignment, ok := body["assignment"].(map[string]any)
	if !ok || assignment["assignmentRole"] != "auditor" || assignment["focus"] != "Review it" {
		t.Fatalf("assignment body = %#v", body)
	}
	if assignment["relatedHostedThreadId"] != "nht_parent" {
		t.Fatalf("assignment relationship = %#v", assignment)
	}
}

func TestRunTurnDetachesByDefault(t *testing.T) {
	var posts atomic.Int32
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			posts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"aop_test","status":"running","terminal":false,"revision":2}`))
		case http.MethodGet:
			gets.Add(1)
			http.Error(w, "detached run must not poll", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	c := &client{base: server.URL, http: &http.Client{Timeout: time.Second}}
	if err := runTurn(context.Background(), c, []string{"--session", "worker-1", "--action", "task-1", "--sender", "sender-1", "--prompt", "work independently"}); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 || gets.Load() != 0 {
		t.Fatalf("requests after detached run = POST %d, GET %d", posts.Load(), gets.Load())
	}
}

func TestRunTurnForegroundWaitIsExplicit(t *testing.T) {
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id":"aop_test","status":"running","terminal":false,"revision":2}`))
			return
		}
		gets.Add(1)
		_, _ = w.Write([]byte(`{"id":"aop_test","status":"completed","terminal":true,"revision":3}`))
	}))
	defer server.Close()
	c := &client{base: server.URL, http: &http.Client{Timeout: time.Second}}
	if err := runTurn(context.Background(), c, []string{"--session", "worker-1", "--action", "task-1", "--sender", "sender-1", "--prompt", "work independently", "--wait"}); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != 2 {
		t.Fatalf("foreground wait GET requests = %d, want long-poll plus final read", gets.Load())
	}
}

func TestRunResolveEncodesRef(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"displayName":"graph classifier","nativeThreadId":"native-1","hostedThreadId":"nht_1","provider":"codex","matchCount":1}`))
	}))
	defer server.Close()
	c := &client{base: server.URL, http: &http.Client{Timeout: time.Second}}
	if err := runResolve(context.Background(), c, []string{"graph classifier"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/identity/resolve?ref=graph+classifier" {
		t.Fatalf("resolve path = %q", gotPath)
	}
}

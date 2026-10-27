package interact

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPClientReadsTranscriptWindowByCursorAndLimit(t *testing.T) {
	if got := NewHTTPClient("", nil).origin; got != DefaultOrigin {
		t.Fatalf("default origin = %q", got)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/threads/thread-1/transcript" || r.URL.Query().Get("before") != "cursor-2" || r.URL.Query().Get("limit") != "20" {
			t.Fatalf("request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"threadId":"thread-1","items":[{"id":"row-1","projectedSeq":7,"turnId":"turn-1","type":"agent_message","category":"messages","text":"done","filterTags":[],"synthetic":false,"sidechain":false,"isFinalAnswer":true,"contentTruncated":false}],"page":{"startSeq":7,"endSeq":7,"latestSeq":9,"snapshotHeadSeq":9,"hasMoreBefore":true,"startsMidTurn":false,"nextBeforeCursor":"cursor-1"}}`))
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, server.Client())
	window, err := client.ReadTranscript(context.Background(), PageRequest{ThreadRef: "thread-1", Cursor: "cursor-2", Limit: 20})
	if err != nil || window.ThreadID != "thread-1" || len(window.Rows) != 1 || window.Page.NextCursor == nil || *window.Page.NextCursor != "cursor-1" {
		t.Fatalf("window = %#v, err = %v", window, err)
	}
	var row struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(window.Rows[0], &row); err != nil || row.Text != "done" {
		t.Fatalf("row = %#v, err = %v", row, err)
	}
}

func TestHTTPClientUsesPublicMailRoutes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/hosted-threads/recipient/messages":
			if r.Method != http.MethodPost {
				t.Fatalf("send method = %s", r.Method)
			}
			body, _ := io.ReadAll(r.Body)
			if !json.Valid(body) || !containsAll(string(body), `"clientMessageId":"stable-1"`, `"senderThreadRef":"sender"`, `"dispositionFeedback":true`) {
				t.Fatalf("send body = %s", body)
			}
			_, _ = w.Write([]byte(`{"messageId":"nmsg_1","clientMessageId":"stable-1","origin":"agent","sender":{"kind":"thread"},"recipient":{"hostedThreadId":"recipient"},"seq":1,"body":"hello","refs":[],"status":"unread","dispositionFeedback":true,"createdAt":"now","updatedAt":"now"}`))
		case "/api/v1/hosted-threads/recipient/inbox":
			if r.URL.Query().Get("actorThreadRef") != "recipient" || r.URL.Query().Get("peek") != "true" || r.URL.Query().Get("since") != "4" || r.URL.Query().Get("limit") != "5" || r.URL.Query().Get("status") != "unread" || r.URL.Query().Get("ref") != "issue.ref:sample-task" {
				t.Fatalf("inbox query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"items":[],"nextSince":4}`))
		case "/api/v1/hosted-threads/sender/outbox":
			_, _ = w.Write([]byte(`{"items":[],"nextSince":0}`))
		case "/api/v1/messages/nmsg_1":
			_, _ = w.Write([]byte(`{"message":{"messageId":"nmsg_1"},"events":[]}`))
		default:
			t.Fatalf("unexpected route %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, server.Client())
	message, err := client.SendMessage(context.Background(), SendMessageRequest{RecipientThreadRef: "recipient", ClientMessageID: "stable-1", SenderThreadRef: "sender", Body: "hello", DispositionFeedback: true})
	if err != nil || !containsAll(string(message), `"messageId":"nmsg_1"`, `"dispositionFeedback":true`) {
		t.Fatalf("send = %s err=%v", message, err)
	}
	if _, err := client.ReadInbox(context.Background(), MessagePageRequest{ThreadRef: "recipient", ActorRef: "recipient", Since: 4, Limit: 5, Status: "unread", Ref: "issue.ref:sample-task", Peek: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadOutbox(context.Background(), MessagePageRequest{ThreadRef: "sender"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadMessage(context.Background(), "nmsg_1"); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPClientReadsDedicatedAvailabilityFacts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/hosted-threads/target/availability" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"hostedThreadId":"target","state":"awaiting-human","reasons":["pending_interaction"],"facts":{"lastHumanMessageAt":"2026-09-12T12:00:00Z","attachment":"live"}}`))
	}))
	defer server.Close()

	state, err := NewHTTPClient(server.URL, server.Client()).ReadAvailability(context.Background(), "target")
	if err != nil || state.Availability != "awaiting-human" || len(state.AvailabilityReasons) != 1 || state.LastHumanMessageAt == "" || state.AttachmentState != "live" {
		t.Fatalf("availability = %#v, %v", state, err)
	}
}

func TestHTTPClientUsesObserverLifecycleRoutes(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/hosted-threads":
			body, _ := io.ReadAll(r.Body)
			if !containsAll(string(body), `"invokerThreadRef":"owner"`, `"clientSessionId":"observer:one"`, `"sandbox":"read-only"`) {
				t.Fatalf("create body = %s", body)
			}
			_, _ = w.Write([]byte(`{"id":"child","provider":"codex","status":"ready","ready":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/hosted-threads/child":
			_, _ = w.Write([]byte(`{"id":"child","provider":"codex","status":"ready","ready":true}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/hosted-threads/child/close":
			_, _ = w.Write([]byte(`{"id":"child","provider":"codex","status":"exited"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/turn-operations/op_one":
			if r.URL.Query().Get("afterRevision") != "3" || r.URL.Query().Get("waitMs") != "500" {
				t.Fatalf("turn query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"id":"op_one","hostedThreadId":"child","clientActionId":"nano:observer:one","provider":"codex","status":"completed","terminal":true,"finalResponse":"done","revision":4}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/identity/resolve":
			if r.URL.Query().Get("ref") != "owner" {
				t.Fatalf("identity query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"displayName":"Owner","nativeThreadId":"native","nativeThreadIdProvisional":false,"hostedThreadId":"owner","provider":"codex","matchCount":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, server.Client())
	created, err := client.CreateHostedThread(context.Background(), HostedThreadCreateRequest{Provider: "codex", InvokerThreadRef: "owner", ClientSessionID: "observer:one", Sandbox: "read-only"})
	if err != nil || created.ID != "child" {
		t.Fatalf("create=%#v err=%v", created, err)
	}
	if read, err := client.ReadLifecycle(context.Background(), "child"); err != nil || read.ThreadID != "child" || read.Status != "ready" {
		t.Fatalf("read=%#v err=%v", read, err)
	}
	if err := client.CloseHostedThread(context.Background(), "child"); err != nil {
		t.Fatal(err)
	}
	if turn, err := client.ReadTurn(context.Background(), "op_one", 3, 500); err != nil || turn.FinalResponse != "done" || turn.Revision != 4 {
		t.Fatalf("turn=%#v err=%v", turn, err)
	}
	if identity, err := client.ResolveIdentity(context.Background(), "owner"); err != nil || identity.MatchCount != 1 || identity.HostedThreadID != "owner" {
		t.Fatalf("identity=%#v err=%v", identity, err)
	}
	if len(calls) != 5 {
		t.Fatalf("calls=%v", calls)
	}
}

func containsAll(value string, wants ...string) bool {
	for _, want := range wants {
		if !strings.Contains(value, want) {
			return false
		}
	}
	return true
}

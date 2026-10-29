package sessions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coresession "github.com/dkroshow/nano/internal/core/session"
	"github.com/dkroshow/nano/internal/inspection"
	"github.com/dkroshow/nano/internal/wire"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// Claude's registry rides its system.init snapshot on /state; there is no
// request surface to ask, so the endpoint says so rather than calling codex.
func TestMCPEndpointRejectsClaudeSessions(t *testing.T) {
	sess := sessionWithOwners(&Session{id: "claude-1",
		provider: "claude",
		stateOwner: State{
			lifecycleState: testLifecycleSnapshot("ready", false)}})
	rec := httptest.NewRecorder()
	sess.view.HandleMCPServers(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/claude-1/mcp", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("got %d, want %d", rec.Code, http.StatusNotImplemented)
	}
}

// A generation bump during an in-flight fetch must not cache the stale result.
func TestMCPCacheDropsResultInvalidatedMidFetch(t *testing.T) {
	cache := &inspection.MCPCache{}
	_, generation := cache.Cached()
	cache.Invalidate() // as if a startup notification landed mid-fetch
	cache.Store(&inspection.MCPInventory{Servers: []inspection.MCPServer{{Name: "stale"}}}, generation)
	if cached, _ := cache.Cached(); cached != nil {
		t.Errorf("cached a stale inventory: %+v", cached)
	}

	_, generation = cache.Cached()
	cache.Store(&inspection.MCPInventory{Servers: []inspection.MCPServer{{Name: "fresh"}}}, generation)
	cached, _ := cache.Cached()
	if cached == nil || cached.Servers[0].Name != "fresh" {
		t.Errorf("did not cache a current inventory: %+v", cached)
	}
}

func resumedSession(t *testing.T, fake *fakeAppServer) *Session {
	t.Helper()
	daemon := fake.daemon()
	sess := reconnectTestSession(t, daemon, "thread-mcp-1")
	sess.setStatus("starting")
	if err := startCodexSession(sess, daemon, ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: "thread-mcp-1"}); err != nil {
		t.Fatal(err)
	}
	waitReconnectCondition(t, func() bool { return sess.view.Summary().Status == "ready" })
	return sess
}

func getSessionPath(t *testing.T, handler func(http.ResponseWriter, *http.Request), path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// The whole point of the slice: /state must not reach for the MCP inventory,
// because that call is the multi-second member of the old aggregate.
func TestStateDoesNotFetchMCPInventory(t *testing.T) {
	fake := newFakeAppServer(t)
	sess := resumedSession(t, fake)

	rec := getSessionPath(t, sess.view.HandleState, "/api/sessions/"+sess.ID()+"/state")
	if rec.Code != http.StatusOK {
		t.Fatalf("/state: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if _, present := state["mcpServers"]; present {
		t.Error("/state still carries mcpServers; it belongs to /mcp now")
	}
	if _, present := state["skills"]; present {
		t.Error("/state still carries the skills list; it belongs to /skills now")
	}
	if _, present := state["thread"]; !present {
		t.Error("/state lost the thread snapshot the right sidebar renders from")
	}
	if calls, _ := fake.McpListCalls(); calls != 0 {
		t.Errorf("/state made %d mcpServerStatus/list calls, want 0", calls)
	}
}

func TestMCPEndpointProjectsAndCaches(t *testing.T) {
	fake := newFakeAppServer(t)
	sess := resumedSession(t, fake)

	var first inspection.MCPInventory
	rec := getSessionPath(t, sess.view.HandleMCPServers, "/api/sessions/"+sess.ID()+"/mcp")
	if rec.Code != http.StatusOK {
		t.Fatalf("/mcp: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	want := inspection.MCPInventory{Servers: []inspection.MCPServer{
		{Name: "docs", Tools: []string{"fetch_doc", "list_endpoints"}}, // sorted
		{Name: "empty", Tools: []string{}},
	}}
	if len(first.Servers) != len(want.Servers) {
		t.Fatalf("servers: got %+v, want %+v", first.Servers, want.Servers)
	}
	for i, server := range want.Servers {
		got := first.Servers[i]
		if got.Name != server.Name {
			t.Errorf("server %d name: got %q, want %q", i, got.Name, server.Name)
		}
		if len(got.Tools) != len(server.Tools) {
			t.Fatalf("server %s tools: got %v, want %v", server.Name, got.Tools, server.Tools)
		}
		for j, tool := range server.Tools {
			if got.Tools[j] != tool {
				t.Errorf("server %s tool %d: got %q, want %q", server.Name, j, got.Tools[j], tool)
			}
		}
	}
	// Tool schemas must not survive the projection — they are the payload bulk.
	if body := rec.Body.String(); strings.Contains(body, "inputSchema") {
		t.Errorf("/mcp leaked tool schemas: %s", body)
	}

	// Second request is served from the cache: no new wire call.
	if rec := getSessionPath(t, sess.view.HandleMCPServers, "/api/sessions/"+sess.ID()+"/mcp"); rec.Code != http.StatusOK {
		t.Fatalf("second /mcp: got %d, want 200", rec.Code)
	}
	calls, details := fake.McpListCalls()
	if calls != 1 {
		t.Errorf("mcpServerStatus/list calls: got %d, want 1 (second read should hit the cache)", calls)
	}
	if len(details) != 1 || details[0] != mcpInventoryDetail {
		t.Errorf("detail params: got %v, want [%s]", details, mcpInventoryDetail)
	}
}

func TestMCPCacheInvalidatedByStartupNotification(t *testing.T) {
	fake := newFakeAppServer(t)
	sess := resumedSession(t, fake)

	getSessionPath(t, sess.view.HandleMCPServers, "/api/sessions/"+sess.ID()+"/mcp")
	sess.wire.recordWire(protocol.Recv, []byte(`{"method":"mcpServer/startupStatus/updated","params":{"name":"docs","status":"ready"}}`))
	// The ingress queues work; wait for its processor before reading the cache.
	if err := sess.runtime.core.Observe(context.Background(), coresession.AdapterEvent{}); err != nil {
		t.Fatal(err)
	}
	getSessionPath(t, sess.view.HandleMCPServers, "/api/sessions/"+sess.ID()+"/mcp")

	if calls, _ := fake.McpListCalls(); calls != 2 {
		t.Errorf("mcpServerStatus/list calls: got %d, want 2 (startup transition invalidates)", calls)
	}
}

func TestMCPInventoryStale(t *testing.T) {
	startup := []byte(`{"method":"mcpServer/startupStatus/updated","params":{"name":"docs"}}`)
	cases := []struct {
		name     string
		provider string
		dir      wire.Direction
		line     []byte
		want     bool
	}{
		{"startup transition", "codex", protocol.Recv, startup, true},
		{"our own send", "codex", protocol.Send, startup, false},
		{"claude session", "claude", protocol.Recv, startup, false},
		{"unrelated notification", "codex", protocol.Recv, []byte(`{"method":"turn/completed","params":{}}`), false},
		{"not json", "codex", protocol.Recv, []byte(`not json`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inspection.MCPInventoryStale(tc.provider, tc.dir, tc.line); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProjectMCPInventoryMalformed(t *testing.T) {
	inventory := inspection.ProjectMCPInventory(json.RawMessage(`not json`))
	if inventory == nil || len(inventory.Servers) != 0 {
		t.Errorf("malformed response: got %+v, want an empty inventory", inventory)
	}
}

package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCatalogOverlaysCrossProviderInvocationIdentity(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	parent, _, err := store.beginSession("parent", "parent-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(parent.ID, "nano-parent", "native-parent", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	parent, _ = store.session(parent.ID)
	child, _, err := store.beginSessionWithInvoker("child", "child-hash", "claude", presentationBackground, parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(child.ID, "nano-child", "native-child", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}

	catalog := &fakeThreadCatalog{page: catalogPage{Items: []catalogThread{
		{ID: "nth_parent", Provider: "codex", NativeID: "native-parent", Kind: "main"},
		{ID: "nth_child", Provider: "claude", NativeID: "native-child", Kind: "main"},
	}}}
	s := &server{catalog: catalog, sessionRegistry: sessionRegistry{sessions: map[string]*session{}}, agentExecutions: store}
	rec := httptest.NewRecorder()
	s.handleCatalogThreads(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d, body %s", rec.Code, rec.Body.String())
	}
	var page catalogPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("catalog items = %#v", page.Items)
	}
	got := page.Items[1]
	if got.HostedThreadID != child.ID || got.InvokerHostedThreadID != parent.ID ||
		got.InvokerNativeThreadID != "native-parent" || got.InvokerProvider != "codex" {
		t.Fatalf("catalog invocation overlay = %#v", got)
	}
}

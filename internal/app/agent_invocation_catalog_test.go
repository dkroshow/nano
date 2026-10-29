package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

func TestCatalogOverlaysCrossProviderInvocationIdentity(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	parent, _, err := fixtureBeginSession(store, "parent", "parent-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(parent.ID, "nano-parent", "native-parent", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	parent, _ = store.Session(parent.ID)
	child, _, err := fixtureBeginSessionWithInvoker(store, "child", "child-hash", "claude", presentationBackground, parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(child.ID, "nano-child", "native-child", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}

	catalog := &fakeThreadCatalog{page: sidebar.CatalogPage{Items: []sidebar.Thread{
		{Thread: threadindex.Thread{ID: "nth_parent", Provider: "codex", NativeID: "native-parent", Kind: "main"}},
		{Thread: threadindex.Thread{ID: "nth_child", Provider: "claude", NativeID: "native-child", Kind: "main"}},
	}}}
	s := &server{catalog: catalog, Registry: newSessionRegistry(), agentExecutions: store}
	rec := httptest.NewRecorder()
	s.catalogViews().HandleThreads(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d, body %s", rec.Code, rec.Body.String())
	}
	var page sidebar.CatalogPage
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

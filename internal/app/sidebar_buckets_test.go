package app

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/sidebar"
)

// The operator's bucket contract, through the real routes and store: buckets
// keep the order the operator set, a thread is in at most one bucket, a
// stale order cannot drop a bucket another device created, deleting a bucket
// ungroups its threads, everything survives a reopen of the store, and every
// open browser hears each change on the shared event stream.
func TestSidebarBucketsKeepOperatorOrderAndReachEveryBrowser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-stars.sqlite")
	store, err := sidebar.OpenStars(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog, threadID := starEndpointCatalog(t)
	s := fixtureOwners(&server{catalog: catalog, threadStars: store, Registry: newSessionRegistry()})
	handler := applicationHandler(s)
	call := func(method, target, body string, want int) sidebar.BucketState {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader(body)))
		if rec.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, target, rec.Code, want, rec.Body.String())
		}
		var state sidebar.BucketState
		if want < 300 {
			if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
				t.Fatal(err)
			}
		}
		return state
	}
	names := func(state sidebar.BucketState) string {
		var out []string
		for _, bucket := range state.Buckets {
			out = append(out, bucket.Name)
		}
		return strings.Join(out, ",")
	}

	srv := httptest.NewServer(http.HandlerFunc(s.catalogViews().HandleEvents))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	stream := bufio.NewReader(response.Body)
	// Each event carries the whole state, so a reader may see several
	// changes folded into one; wait for the state that names want.
	awaitBuckets := func(want string) {
		t.Helper()
		for {
			if event, data := readSSEEvent(t, stream); event == "buckets" && strings.Contains(data, want) {
				return
			}
		}
	}
	awaitBuckets(`{"buckets":[]}`)

	call(http.MethodPost, "/api/v1/sidebar/buckets", `{"name":"  "}`, http.StatusBadRequest)
	call(http.MethodPost, "/api/v1/sidebar/buckets", `{"name":"Audit"}`, http.StatusCreated)
	state := call(http.MethodPost, "/api/v1/sidebar/buckets", `{"name":"Release"}`, http.StatusCreated)
	if names(state) != "Audit,Release" {
		t.Fatalf("new buckets go last: %s", names(state))
	}
	audit, release := state.Buckets[0].ID, state.Buckets[1].ID
	awaitBuckets(`"Release"`)

	call(http.MethodPut, "/api/v1/sidebar/buckets/order", `{"bucketIds":["`+release+`"]}`, http.StatusConflict)
	state = call(http.MethodPut, "/api/v1/sidebar/buckets/order", `{"bucketIds":["`+release+`","`+audit+`"]}`, http.StatusOK)
	if names(state) != "Release,Audit" {
		t.Fatalf("order = %s", names(state))
	}
	call(http.MethodPatch, "/api/v1/sidebar/buckets/"+audit, `{"name":"Machine audit"}`, http.StatusOK)
	call(http.MethodPut, "/api/v1/threads/"+threadID+"/bucket", `{"bucketId":"bkt_missing"}`, http.StatusNotFound)
	call(http.MethodPut, "/api/v1/threads/"+threadID+"/bucket", `{}`, http.StatusBadRequest)
	call(http.MethodPut, "/api/v1/threads/"+threadID+"/bucket", `{"bucketId":"`+release+`"}`, http.StatusOK)
	state = call(http.MethodPut, "/api/v1/threads/"+threadID+"/bucket", `{"bucketId":"`+audit+`"}`, http.StatusOK)
	if len(state.Buckets[0].Threads) != 0 || len(state.Buckets[1].Threads) != 1 || state.Buckets[1].Threads[0].NativeID != "native-1" {
		t.Fatalf("a thread is in one bucket at a time: %+v", state.Buckets)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sidebar.OpenStars(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	s = fixtureOwners(&server{catalog: catalog, threadStars: reopened, Registry: newSessionRegistry()})
	handler = applicationHandler(s)
	state = call(http.MethodGet, "/api/v1/sidebar/buckets", "", http.StatusOK)
	if names(state) != "Release,Machine audit" || len(state.Buckets[1].Threads) != 1 {
		t.Fatalf("after reopen: %+v", state.Buckets)
	}
	call(http.MethodPut, "/api/v1/threads/"+threadID+"/bucket", `{"bucketId":null}`, http.StatusOK)
	call(http.MethodPut, "/api/v1/threads/"+threadID+"/bucket", `{"bucketId":"`+audit+`"}`, http.StatusOK)
	state = call(http.MethodDelete, "/api/v1/sidebar/buckets/"+audit, "", http.StatusOK)
	if names(state) != "Release" || len(state.Buckets[0].Threads) != 0 {
		t.Fatalf("delete left %+v", state.Buckets)
	}
	state = call(http.MethodPost, "/api/v1/sidebar/buckets", `{"name":"Machine audit"}`, http.StatusCreated)
	if len(state.Buckets[1].Threads) != 0 {
		t.Fatalf("a deleted bucket's thread came back: %+v", state.Buckets)
	}
}

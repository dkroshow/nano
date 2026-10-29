package sidebar

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/sessions"
)

// warmReader is the browser's end of one warm stream.
type warmReader struct {
	t        *testing.T
	status   int
	messages chan sessions.LiveTextMessage
}

// openWarm opens a warm stream for a set of threads; extra query parameters
// (lines, cols, markers) ride along.
func openWarm(t *testing.T, srv *Controller, set []string, extra url.Values) *warmReader {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleWarmTranscripts))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ts.Close() })
	query := url.Values{"set": {strings.Join(set, ",")}}
	for key, values := range extra {
		query[key] = values
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/warm-transcripts?"+query.Encode(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := &warmReader{t: t, status: resp.StatusCode, messages: make(chan sessions.LiveTextMessage, 64)}
	go func() {
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1<<20), 4<<20)
		for scanner.Scan() {
			data, ok := strings.CutPrefix(scanner.Text(), "data: ")
			if !ok {
				continue
			}
			var message sessions.LiveTextMessage
			if err := json.Unmarshal([]byte(data), &message); err != nil {
				t.Error(err)
				return
			}
			reader.messages <- message
		}
	}()
	return reader
}

func (r *warmReader) next(what string) sessions.LiveTextMessage {
	r.t.Helper()
	select {
	case message := <-r.messages:
		return message
	case <-time.After(20 * time.Second):
		r.t.Fatalf("timed out waiting for %s", what)
	}
	return sessions.LiveTextMessage{}
}

// A thread the server cannot warm gets one error and stays out of the set:
// no live session holds it, or the server-wide budget is spent. It is tried
// again when the sidebar changes, with no second error.
func TestWarmStreamRefusesThreadsItCannotWarm(t *testing.T) {
	controller, _ := sidebarActiveFixture(t, time.Now())
	sess := controller.registry.SnapshotSessions()[0]
	sess.AttachLiveText(sessions.NewLiveTextLoop(sessions.LiveTextSources{Catalog: controller.catalog}, sess.State()))
	for i := range warmBudget {
		controller.warm.admit(fmt.Sprintf("nht_elsewhere_%d", i))
	}
	warm := openWarm(t, controller, []string{"nht_not_held", sess.HostedThreadID()}, nil)
	for _, want := range []struct{ ref, reason string }{
		{"nht_not_held", "no live session holds this thread"},
		{sess.HostedThreadID(), "the warm budget is spent"},
	} {
		if got := warm.next("refusal"); got.Type != "error" || got.Ref != want.ref || got.Error != want.reason {
			t.Fatalf("got %+v, want error %q for %s", got, want.reason, want.ref)
		}
	}
	if set := warm.next("set"); set.Type != "set" || len(set.Refs) != 0 {
		t.Fatalf("got %+v, want an empty set", set)
	}
	if bad := openWarm(t, controller, []string{sess.HostedThreadID()}, url.Values{"markers": {"not json"}}); bad.status != http.StatusBadRequest {
		t.Fatalf("malformed markers: status %d, want 400", bad.status)
	}
}

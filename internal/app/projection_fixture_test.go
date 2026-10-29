package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
)

func newLiveProjection(provider string, ids ...string) *sessions.LiveProjection {
	id := ""
	if len(ids) > 0 {
		id = ids[0]
	}
	return sessions.NewLiveProjection(provider, id, false)
}
func newLiveProjectionWithDeltaSetting(provider string, drop bool) *sessions.LiveProjection {
	return sessions.NewLiveProjection(provider, "", drop)
}

type firstStatusWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *firstStatusWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(data)
	if bytes.Contains(w.Body.Bytes(), []byte(`"type":"status"`)) {
		w.cancel()
	}
	return n, err
}
func liveStatusForTest(t *testing.T, sess *sessions.Session) liveview.Status {
	t.Helper()
	if sess.View().LiveTextService() == nil {
		sess.AttachLiveText(sessions.NewLiveTextLoop(sessions.LiveTextSources{}, sess))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &firstStatusWriter{httptest.NewRecorder(), cancel}
	sess.View().HandleLiveStream(w, httptest.NewRequest("GET", "/live?view=enhanced", nil).WithContext(ctx))
	for _, line := range bytes.Split(w.Body.Bytes(), []byte("\n")) {
		if raw, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
			var message struct {
				Type   string          `json:"type"`
				Status liveview.Status `json:"status"`
			}
			if json.Unmarshal(raw, &message) == nil && message.Type == "status" {
				return message.Status
			}
		}
	}
	t.Fatal("live stream returned no initial status", w.Body.String())
	return liveview.Status{}
}

// projectionStream observes the same ordered turn and runtime events as a
// conversation client. Tests subscribe before triggering provider activity.
type projectionStream struct {
	mu       sync.Mutex
	header   http.Header
	pending  []byte
	snapshot liveview.Snapshot
	ready    chan struct{}
	once     sync.Once
}

func (w *projectionStream) Header() http.Header { return w.header }
func (w *projectionStream) WriteHeader(int)     {}
func (w *projectionStream) Flush()              {}
func (w *projectionStream) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, data...)
	for {
		frame, rest, ok := bytes.Cut(w.pending, []byte("\n\n"))
		if !ok {
			break
		}
		w.pending = rest
		raw, ok := bytes.CutPrefix(frame, []byte("data: "))
		if !ok {
			continue
		}
		var u liveview.Update
		if json.Unmarshal(raw, &u) != nil {
			continue
		}
		if u.Snapshot != nil {
			w.snapshot = *u.Snapshot
			w.once.Do(func() { close(w.ready) })
		}
		if u.Runtime != nil {
			w.snapshot.Runtime = *u.Runtime
		}
		if u.Turn != nil && u.Turn.ID != "" {
			found := false
			for i := range w.snapshot.Turns {
				if w.snapshot.Turns[i].ID == u.Turn.ID {
					w.snapshot.Turns[i] = *u.Turn
					found = true
					break
				}
			}
			if !found {
				w.snapshot.Turns = append(w.snapshot.Turns, *u.Turn)
			}
		}
	}
	return len(data), nil
}
func (w *projectionStream) Snapshot() liveview.Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.snapshot
	s.Turns = append([]liveview.Turn(nil), s.Turns...)
	return s
}
func observeProjection(t *testing.T, sess *sessions.Session) *projectionStream {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	catalog, err := newThreadCatalogService(context.Background(), store, threadindex.Options{DisableWatcher: true, Roots: []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: t.TempDir()}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(catalog.Close)
	sess.AttachLiveText(sessions.NewLiveTextLoop(sessions.LiveTextSources{Catalog: catalog.Service}, sess))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	w := &projectionStream{header: make(http.Header), ready: make(chan struct{})}
	go func() {
		defer close(done)
		sess.View().HandleConversationEvents(w, httptest.NewRequest("GET", "/conversation/events", nil).WithContext(ctx))
	}()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-w.ready:
	case <-done:
		t.Fatal("conversation stream ended before its snapshot")
	case <-time.After(5 * time.Second):
		t.Fatal("conversation stream did not publish its snapshot")
	}
	return w
}

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/sessions"
)

// These tests protect session routing from metadata-read contention and from
// losing the stored hosted identity during resume.

// /fast enters through the ordinary turn-options endpoint. An open browser
// concurrently reads canonical metadata for the session registry, so that
// read must not monopolize control.sqlite and make this durable local write
// wait behind the whole registry traversal.
func TestCodexFastModeDoesNotWaitBehindMetadataReader(t *testing.T) {
	metadata := newTestControlMetadata(t)
	if err := metadata.EnsureHostedThread("nht_fast", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}

	sess := configuredFixtureSession(t, sessions.Config{ID: "sess-fast", Provider: "codex", HostedThreadID: "nht_fast", Metadata: metadata}, "", "ready", "")

	rows, err := metadata.ReadPool().Query(`SELECT hosted_thread_id FROM hosted_threads`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("metadata reader returned no hosted thread")
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		sess.HandleTurnOptionsUpdate(rec, httptest.NewRequest(http.MethodPost,
			"/api/sessions/sess-fast/turn-options", strings.NewReader(`{"serviceTier":"priority"}`)))
		done <- rec
	}()

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("/fast turn options = %d %s", rec.Code, rec.Body.String())
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("/fast turn-options write waited behind an unrelated metadata reader")
	}
}

// The browser enters through GET /api/sessions while /fast enters through
// the session-scoped router. A slow registry projection must not keep the
// global session-map mutex that the command router needs to find its target.
func TestCodexFastModeDoesNotWaitBehindSessionRegistryProjection(t *testing.T) {
	directory := t.TempDir()
	stores, err := openThreadRuntimeStores(context.Background(), directory, filepath.Join(directory, "completed.sqlite"), "routing-test")
	if err != nil {
		t.Fatal(err)
	}
	metadata := stores.Control
	t.Cleanup(func() { _ = metadata.Close() })
	if err := metadata.EnsureHostedThread("nht_fast_route", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}

	sess := configuredFixtureSession(t, sessions.Config{ID: "sess-fast-route", Provider: "codex", HostedThreadID: "nht_fast_route", Metadata: metadata}, "", "ready", "")

	readDB := metadata.ReadPool()
	waitsBefore := readDB.Stats().WaitCount
	held := make([]*sql.Conn, 0, 4)
	for i := 0; i < 4; i++ {
		conn, err := readDB.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	released := false
	releaseReaders := func() {
		if released {
			return
		}
		released = true
		for _, conn := range held {
			_ = conn.Close()
		}
	}
	defer releaseReaders()

	s := testServerWithSessions(&server{
		Registry:        newSessionRegistry(),
		controlMetadata: metadata,
	}, map[string]*sessions.Session{sess.ID(): sess})
	registryDone := make(chan struct{})
	go func() {
		s.sidebarOwner().SessionsHandler(s.defaults)(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
		close(registryDone)
	}()

	deadline := time.Now().Add(time.Second)
	for readDB.Stats().WaitCount == waitsBefore && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if readDB.Stats().WaitCount == waitsBefore {
		t.Fatal("session registry did not reach the blocked metadata projection")
	}

	commandDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		applicationHandler(fixtureOwners(s)).ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/api/sessions/sess-fast-route/turn-options", strings.NewReader(`{"serviceTier":"priority"}`)))
		commandDone <- rec
	}()

	select {
	case rec := <-commandDone:
		if rec.Code != http.StatusOK {
			t.Fatalf("/fast turn options = %d %s", rec.Code, rec.Body.String())
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("/fast session routing waited behind the session registry projection")
	}

	releaseReaders()
	select {
	case <-registryDone:
	case <-time.After(time.Second):
		t.Fatal("session registry did not finish after releasing metadata readers")
	}
}

// Operator requires unsupported model/speed combinations to fail before changing
// intent. Boundary: the real HTTP turn-options handler and canonical store.
func TestUltrafastTurnOptionsValidateModelAndPreserveIntent(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread("nht_speed_controls", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	fixture := newProviderRuntimeFixture(t, sessions.Config{ID: "speed-controls", Provider: "codex", HostedThreadID: "nht_speed_controls", Metadata: store}, "native", "gpt-6-astra")
	sess, fake := fixture.session, fixture.codex
	fake.Mu.Lock()
	fake.ModelList = map[string]any{"data": []any{
		map[string]any{"model": "gpt-6-astra", "serviceTiers": []any{map[string]any{"id": "ultrafast"}}},
		map[string]any{"model": "gpt-6.1-sol", "serviceTiers": []any{map[string]any{"id": "ultrafast"}}},
		map[string]any{"model": "gpt-6-sol", "serviceTiers": []any{map[string]any{"id": "priority"}}},
	}}
	fake.Mu.Unlock()
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"serviceTier":"ultrafast"}`, 200},
		{`{"model":"gpt-6.1-sol"}`, 200},
		{`{"model":"gpt-6-sol"}`, 400},
	} {
		rec := httptest.NewRecorder()
		sess.HandleTurnOptionsUpdate(rec, httptest.NewRequest(http.MethodPost, "/turn-options", strings.NewReader(tc.body)))
		if rec.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.body, rec.Code, rec.Body)
		}
	}
	f, desired, effective := fieldStrings(t, store, sess.HostedThreadID(), postureFieldServiceTier)
	if desired != "ultrafast" || effective != "" || f.DesiredDisposition != metadataDispositionRequested {
		t.Fatalf("pending tier = %+v %s %s", f, desired, effective)
	}
	read := httptest.NewRecorder()
	sess.HandleTurnOptionsRead(read, httptest.NewRequest(http.MethodGet, "/turn-options", nil))
	var options struct {
		Model       string `json:"model"`
		ServiceTier string `json:"serviceTier"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &options); err != nil {
		t.Fatal(err)
	}
	if options.Model != "gpt-6.1-sol" || options.ServiceTier != "ultrafast" {
		t.Fatalf("rejected switch changed intent: %s %s", options.Model, options.ServiceTier)
	}
}

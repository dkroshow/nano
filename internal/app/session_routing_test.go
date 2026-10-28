package app

// These tests protect session routing from metadata-read contention and from
// losing the stored hosted identity during resume.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// /fast enters through the ordinary turn-options endpoint. An open browser
// concurrently reads canonical metadata for the session registry, so that
// read must not monopolize control.sqlite and make this durable local write
// wait behind the whole registry traversal.
func TestCodexFastModeDoesNotWaitBehindMetadataReader(t *testing.T) {
	metadata := newTestControlMetadata(t)
	if err := metadata.ensureHostedThread("nht_fast", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}

	rows, err := metadata.readPool().Query(`SELECT hosted_thread_id FROM hosted_threads`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("metadata reader returned no hosted thread")
	}

	sess := &session{id: "sess-fast",
		provider:       "codex",
		hostedThreadID: "nht_fast",
		metadata:       metadata,
		subs:           map[chan wireEvent]struct{}{},
		lifecycleState: testLifecycleSnapshot("ready", false)}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		sess.handleTurnOptionsUpdate(rec, httptest.NewRequest(http.MethodPost,
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
	metadata := newTestControlMetadata(t)
	if err := metadata.ensureHostedThread("nht_fast_route", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}

	readDB := metadata.readPool()
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

	sess := &session{id: "sess-fast-route",
		provider:       "codex",
		hostedThreadID: "nht_fast_route",
		metadata:       metadata,
		subs:           map[chan wireEvent]struct{}{},
		lifecycleState: testLifecycleSnapshot("ready", false)}
	s := &server{
		sessionRegistry: sessionRegistry{sessions: map[string]*session{sess.id: sess}},
		controlMetadata: metadata,
	}
	registryDone := make(chan struct{})
	go func() {
		s.handleSessionsList(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
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
		applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
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

// The log keys on hostedThreadID, so a click-resume must resume the identity
// too: each provider's resolver adopts the attachment record's stored id
// instead of letting spawnSession mint a fresh one that would orphan the
// thread's log. A request that already names one (agent API, boot reattach)
// wins over the record.
func TestResumeResolversKeepStoredHostedIdentity(t *testing.T) {
	store := newTestStore(t)
	cwd := t.TempDir()
	metadata, executions := newTestHostedControl(t)
	for provider, hosted := range map[string]string{
		"claude": "nht_claude", "codex": "nht_codex", "grok": "nht_grok",
	} {
		store.remember(attachmentRecord{
			Provider: provider, NativeThreadID: "native-" + provider,
			HostedThreadID: hosted, Cwd: cwd, AttachedAt: "2026-08-06T10:00:00Z",
		})
		if _, err := executions.ensureSessionWithIdentity(hosted, provider, presentationShared, "", "", ""); err != nil {
			t.Fatal(err)
		}
		if err := metadata.claimNativeID(hosted, provider, "native-"+provider, false); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.appendEvent(metadataEvent{HostedThreadID: hosted, Field: "cwd", Value: cwd, Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive}); err != nil {
			t.Fatal(err)
		}
	}
	s := &server{defaults: sessionDefaults{cwd: cwd, claudeBin: "/fake/claude"}, attachments: store, controlMetadata: metadata, agentExecutions: executions}

	for provider, want := range map[string]string{
		"claude": "nht_claude", "codex": "nht_codex", "grok": "nht_grok",
	} {
		resolved, err := s.resolveSpawn(spawnRequest{providerRequest: providerRequest{Provider: provider, Mode: "resume", ThreadID: "native-" + provider}})
		if err != nil {
			t.Fatalf("resolve %s resume: %v", provider, err)
		}
		if resolved.HostedThreadID != want {
			t.Fatalf("%s resume hostedThreadID = %q, want the stored %q", provider, resolved.HostedThreadID, want)
		}
	}
	if _, err := executions.ensureSessionWithIdentity("nht_explicit", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := metadata.claimNativeID("nht_explicit", "claude", "native-explicit", false); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.appendEvent(metadataEvent{HostedThreadID: "nht_explicit", Field: "cwd", Value: cwd, Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive}); err != nil {
		t.Fatal(err)
	}

	resolved, err := s.resolveSpawn(spawnRequest{providerRequest: providerRequest{Provider: "claude", Mode: "resume", ThreadID: "native-explicit", HostedThreadID: "nht_explicit"}})
	if err != nil {
		t.Fatalf("resolve explicit resume: %v", err)
	}
	if resolved.HostedThreadID != "nht_explicit" {
		t.Fatalf("explicit hostedThreadID = %q, want the request's own to win", resolved.HostedThreadID)
	}
}

// Operator requires unsupported model/speed combinations to fail before changing
// intent. Boundary: the real HTTP turn-options handler and canonical store.
func TestUltrafastTurnOptionsValidateModelAndPreserveIntent(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.mu.Lock()
	fake.modelList = map[string]any{"data": []any{
		map[string]any{"model": "gpt-6-astra", "serviceTiers": []any{map[string]any{"id": "ultrafast"}}},
		map[string]any{"model": "gpt-6.1-sol", "serviceTiers": []any{map[string]any{"id": "ultrafast"}}},
		map[string]any{"model": "gpt-6-sol", "serviceTiers": []any{map[string]any{"id": "priority"}}},
	}}
	fake.mu.Unlock()
	d := fake.daemon()
	t.Cleanup(func() {
		d.mu.Lock()
		c := d.client
		d.mu.Unlock()
		if c != nil {
			c.Close()
		}
	})
	sess, store := causationTestSession(t, "nht_speed_controls")
	sess.codex = d
	sess.turnModel = "gpt-6-astra"
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"serviceTier":"ultrafast"}`, 200},
		{`{"model":"gpt-6.1-sol"}`, 200},
		{`{"model":"gpt-6-sol"}`, 400},
	} {
		rec := httptest.NewRecorder()
		sess.handleTurnOptionsUpdate(rec, httptest.NewRequest(http.MethodPost, "/turn-options", strings.NewReader(tc.body)))
		if rec.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.body, rec.Code, rec.Body)
		}
	}
	f, desired, effective := fieldStrings(t, store, sess.hostedThreadID, postureFieldServiceTier)
	if desired != "ultrafast" || effective != "" || f.DesiredDisposition != metadataDispositionRequested {
		t.Fatalf("pending tier = %+v %s %s", f, desired, effective)
	}
	model, _, tier := sess.turnOptions()
	if model != "gpt-6.1-sol" || tier != "ultrafast" {
		t.Fatalf("rejected switch changed intent: %s %s", model, tier)
	}
}

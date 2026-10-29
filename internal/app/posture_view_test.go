package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"reflect"
	"strings"
	"testing"

	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

// Item 3 of the live-posture plan: one posture struct serves every surface.
// These tests pin the projection itself (fold-first, live fallback), the
// surfaces that serve it (summary, /state, catalog memory row), resume
// hydration.

// The summary and /state serve the same folded posture — newest decision per
// field — and fall back to live session values where the log is silent.
func TestSummaryAndStateServeFoldedPosture(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread("nht_view", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, err := store.BeginTransportBinding("nht_view")
	if err != nil {
		t.Fatal(err)
	}
	sess := newSession(sessionConfig{ID: "posture-view", Provider: "claude", HostedThreadID: "nht_view", Metadata: store})
	if err := sess.InitializeBinding(g); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []metadataEvent{
		{Field: postureFieldModel, Value: "sonnet", Source: postureSourceCreation, Evidence: postureEvidenceSpawnRequest},
		{Field: postureFieldEffort, Value: "xhigh", Source: postureSourceCreation, Evidence: postureEvidenceSpawnRequest},
		{Field: postureFieldPermissionMode, Value: "acceptEdits", Source: postureSourceCreation, Evidence: postureEvidenceSpawnRequest},
		{Field: postureFieldModel, Value: "haiku", Source: postureSourceOperator, Evidence: postureEvidenceSetModel},
	} {
		entry.HostedThreadID = "nht_view"
		entry.Disposition = metadataDispositionRequested
		entry.Freshness = metadataFreshnessLive
		entry.AttachmentGeneration = g
		if _, err := store.AppendEvent(entry); err != nil {
			t.Fatal(err)
		}
	}

	want := sessionPosture{Model: "haiku", Effort: "xhigh", PermissionMode: "acceptEdits"}
	if got := sess.Summary().Posture; got != want {
		t.Fatalf("summary posture = %+v, want %+v", got, want)
	}

	w := httptest.NewRecorder()
	sess.HandleState(w, httptest.NewRequest(http.MethodGet, "/api/sessions/x/state", nil))
	var state struct {
		Posture sessionPosture `json:"posture"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.Posture != want {
		t.Fatalf("/state posture = %+v, want the summary's %+v", state.Posture, want)
	}

	// A thread with no recorded decisions serves live session evidence — the
	// legacy-session fallback.
	legacy := newProviderRuntimeFixture(t, sessionConfig{ID: "legacy-model", Provider: "claude", HostedThreadID: "nht_legacy"}, "legacy-native", "claude-opus-5").session
	if got := legacy.Summary().Posture.Model; got != "" {
		t.Fatalf("unavailable metadata fabricated model = %q", got)
	}
}

// The catalog memory row serves the same projection, which lights up the
// effort/priority/permission cells that were hardcoded unknown — and shows
// the creation-request model before the provider handshake reports one.
func TestMemoryThreadRowServesPostureCells(t *testing.T) {
	sum := sessionSummary{
		ID: "sess-1", Provider: "claude", Cwd: "/w",
		Posture: sessionPosture{Model: "opus", Effort: "high", PermissionMode: "plan", ServiceTier: "priority"},
	}
	row := sidebar.MemoryThreadRow(sum)
	cells := map[string]threadindex.Value{
		"model":      row.Metadata.Model,
		"effort":     row.Metadata.Effort,
		"priority":   row.Metadata.Priority,
		"permission": row.Metadata.Permission,
	}
	wants := map[string]string{"model": "opus", "effort": "high", "priority": "priority", "permission": "plan"}
	for cell, want := range wants {
		got := cells[cell]
		if got.State != "known" || got.Value == nil || *got.Value != want {
			t.Fatalf("%s cell = %+v, want known %q", cell, got, want)
		}
	}

	unknown := sidebar.MemoryThreadRow(sessionSummary{ID: "sess-2", Provider: "claude"})
	if unknown.Metadata.Effort.State != "unknown" {
		t.Fatalf("empty posture effort cell = %+v, want honest unknown", unknown.Metadata.Effort)
	}
}

func TestCanonicalResumeProjectionPreservesDesiredIntent(t *testing.T) {
	cases := []struct {
		name, desired, settlement, effective, want string
	}{
		{"pending", "priority", "", "", "priority"},
		{"confirmed", "priority", metadataDispositionConfirmed, "priority", "priority"},
		{"rejected", "priority", metadataDispositionRejected, "default", "priority"},
		{"cleared", "", metadataDispositionCleared, "priority", ""},
		{"effective-only", "", "", "priority", "priority"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			metadata := newTestControlMetadata(t)
			const hostedID = "nht_resume_projection"
			if err := metadata.EnsureHostedThread(hostedID, "claude", presentationShared, "", "", ""); err != nil {
				t.Fatal(err)
			}
			var desired metadataEvent
			var err error
			if test.settlement == metadataDispositionCleared {
				desired, err = metadata.AppendEvent(metadataEvent{HostedThreadID: hostedID, Field: postureFieldModel, Value: "", Disposition: metadataDispositionCleared, Source: postureSourceOperator, Freshness: metadataFreshnessLive, SourceEventKey: test.name + ":cleared"})
			} else if test.desired != "" {
				desired, err = metadata.AppendEvent(metadataEvent{HostedThreadID: hostedID, Field: postureFieldModel, Value: test.desired, Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, SourceEventKey: test.name + ":requested"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.settlement == metadataDispositionConfirmed || test.settlement == metadataDispositionRejected {
				if _, err := metadata.AppendEvent(metadataEvent{HostedThreadID: hostedID, Field: postureFieldModel, Value: test.desired, Disposition: test.settlement, Source: postureSourceProvider, Freshness: metadataFreshnessLive, CausationEventID: &desired.ID, SourceEventKey: test.name + ":settlement"}); err != nil {
					t.Fatal(err)
				}
			}
			if test.effective != "" && (test.settlement != metadataDispositionConfirmed || test.effective != test.desired) {
				if _, err := metadata.AppendEvent(metadataEvent{HostedThreadID: hostedID, Field: postureFieldModel, Value: test.effective, Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, SourceEventKey: test.name + ":effective"}); err != nil {
					t.Fatal(err)
				}
			}
			view, err := metadata.View(hostedID)
			if err != nil {
				t.Fatal(err)
			}
			view.Fields[postureFieldServiceTier] = view.Fields[postureFieldModel]
			fixture := newProviderRuntimeFixture(t, sessionConfig{ID: "canonical-resume", Provider: "codex", HostedThreadID: hostedID, Metadata: fixedMetadataView{sessionMetadata: metadata, view: view}}, "canonical-native", "")
			read := httptest.NewRecorder()
			fixture.session.HandleTurnOptionsRead(read, httptest.NewRequest(http.MethodGet, "/turn-options", nil))
			var options struct {
				Model       string `json:"model"`
				ServiceTier string `json:"serviceTier"`
			}
			if err := json.Unmarshal(read.Body.Bytes(), &options); err != nil {
				t.Fatal(err)
			}
			if options.Model != test.want || options.ServiceTier != test.want {
				t.Fatalf("Codex resume = %q/%q, want %q", options.Model, options.ServiceTier, test.want)
			}
			cwd := t.TempDir()
			if _, err := metadata.AppendEvent(metadataEvent{HostedThreadID: hostedID, Field: "cwd", Value: cwd, Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive}); err != nil {
				t.Fatal(err)
			}
			owner := &server{controlMetadata: metadata}
			req := hostedmodel.SpawnRequest{ProviderRequest: providerRequest{Provider: "claude", Mode: "resume", ThreadID: "canonical-native", Model: "stale", HostedThreadID: hostedID}}
			if err := metadata.ClaimNativeID(hostedID, "claude", "canonical-native", false); err != nil {
				t.Fatal(err)
			}
			args := fixturePreviewArgs(t, owner, req)[1:]

			if test.want == "" {
				args = claudeCanonicalResumeArgs(args, (hostedmodel.SpawnRequest{}).ProviderRequest)
				if strings.Contains(strings.Join(args, " "), "stale") {
					t.Fatalf("cleared Claude resume retained stale argv %#v", args)
				}
				return
			}
			if got := singletonValue(t, args, claudeModelFlag); got != test.want {
				t.Fatalf("Claude resume model = %q, want %q", got, test.want)
			}
		})
	}
}

// A resumed thread picks its durable name back up at spawn, so the boot/click
// summary carries it without waiting for the catalog overlay.
func TestResumeHydratesDurableName(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.Reply(call, map[string]any{
			"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID,
			"state": "running", "lastSeq": 1,
		})
		c.Line(1, "recv", claudeInitLine)
		c.Notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 1})
	}))
	s, store := claudeResumeServer(t, shim.WSURL())
	s.catalog = fixtureNamedCatalog(t, "claude", claudeResumeThreadID, "overnight refactor")
	store.Remember(rememberedClaudeSession(t, "/stored/claude"))

	sum := spawnedSummary(t, postClaudeResume(t, s, resumeBody(claudeResumeThreadID)))
	if sum.Name != "overnight refactor" {
		t.Fatalf("resumed summary name = %q, want the durable name hydrated at spawn", sum.Name)
	}
}

// The overlay through the real fallback rung: a set_model recorded before the
// shim lost the child survives the respawn — the exact scar the plan's
// "live set_model is never persisted" finding describes.
func TestClaudeResumeFallbackCarriesOperatorModelOverlay(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.ReplyError(call, shimCodeSessionNotFound, "session_not_found")
	}))
	s, store := claudeResumeServer(t, shim.WSURL())
	rec := rememberedClaudeSession(t, "/stored/claude")
	rec.HostedThreadID = "nht_overlay"
	store.Remember(rec)
	if _, err := fixtureEnsureSessionWithIdentity(s.agentExecutions, "nht_overlay", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.controlMetadata.ClaimNativeID("nht_overlay", "claude", claudeResumeThreadID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.controlMetadata.AppendEvent(metadataEvent{HostedThreadID: "nht_overlay", Field: "cwd", Value: rec.Cwd, Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive, SourceEventKey: "fixture:nht_overlay:cwd"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.controlMetadata.AppendEvent(metadataEvent{HostedThreadID: "nht_overlay", Field: postureFieldPermissionMode, Value: "bypassPermissions", Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive, SourceEventKey: "fixture:nht_overlay:permission"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.controlMetadata.AppendEvent(metadataEvent{HostedThreadID: "nht_overlay", Field: postureFieldModel, Value: "haiku", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Evidence: postureEvidenceSetModel, Freshness: metadataFreshnessLive}); err != nil {
		t.Fatal(err)
	}

	sum := spawnedSummary(t, postClaudeResume(t, s, resumeBody(claudeResumeThreadID)))
	if sum.HostedThreadID != "nht_overlay" {
		t.Fatalf("resumed hostedThreadId = %q, want the stored identity the log keys on", sum.HostedThreadID)
	}
	waitForResumedThreadID(t, s, sum.ID, "native-2")

	_, args, _ := startArgs(t, shim)
	want := []string{
		"-p", "--verbose", "--permission-mode", "bypassPermissions",
		claudeResumeFlag, claudeResumeThreadID, claudeModelFlag, "haiku",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("fallback args = %#v, want the overlay applied: %#v", args, want)
	}
	view, err := s.controlMetadata.View("nht_overlay")
	if err != nil || view.Fields[postureFieldModel].DesiredEventID == nil {
		t.Fatalf("canonical overlay intent missing after fallback: view=%#v err=%v", view, err)
	}
}

// fixedMetadataView supplies the same canonical view to a second provider's consumer.
// The real store still owns persistence and event validation.
type fixedMetadataView struct {
	sessionMetadata
	view hostedstore.MetadataView
}

func (m fixedMetadataView) View(string) (hostedstore.MetadataView, error) { return m.view, nil }

package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/config"
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

// Once an attachment names a hosted identity, its retired JSON posture fields
// are transport-history only.  A resume must take cwd/model/permission from
// control.sqlite even when the old record says something different.
func TestHostedResumeSeedsOnlyCanonicalMetadata(t *testing.T) {
	metadata := newTestControlMetadata(t)
	canonicalCwd := t.TempDir()
	legacyCwd := t.TempDir()
	if err := metadata.EnsureHostedThread("nht_resume_canonical", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := metadata.ClaimNativeID("nht_resume_canonical", "claude", "native-resume", false); err != nil {
		t.Fatal(err)
	}
	for _, event := range []metadataEvent{
		{HostedThreadID: "nht_resume_canonical", Field: "cwd", Value: canonicalCwd, Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive},
		{HostedThreadID: "nht_resume_canonical", Field: postureFieldModel, Value: "canonical-model", Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive},
		{HostedThreadID: "nht_resume_canonical", Field: postureFieldPermissionMode, Value: "acceptEdits", Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive},
	} {
		if _, err := metadata.AppendEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	attachments, err := hostedstore.NewAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	attachments.Remember(hostedstore.Attachment{
		Provider: "claude", NativeThreadID: "native-resume", HostedThreadID: "nht_resume_canonical",
		Cwd: legacyCwd, Model: "legacy-model", Bin: "/claude", Args: []string{"-p", claudeModelFlag, "legacy-model", "--permission-mode", "bypassPermissions"},
	})
	s := &server{defaults: config.SessionDefaults{CWD: legacyCwd, ClaudeBin: "/claude"}, attachments: attachments, controlMetadata: metadata}
	resolved, err := s.attachmentCoordinator().ResolveSpawn(hostedmodel.SpawnRequest{ProviderRequest: providerRequest{Provider: "claude", Mode: "resume", ThreadID: "native-resume"}})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Cwd != canonicalCwd || resolved.Model != "canonical-model" || resolved.PermissionMode != "acceptEdits" {
		t.Fatalf("resume = cwd %q model %q permission %q, want canonical values", resolved.Cwd, resolved.Model, resolved.PermissionMode)
	}
	if got := strings.Join(resolved.ClaudeArgs, " "); strings.Contains(got, "legacy-model") || strings.Contains(got, "bypassPermissions") {
		t.Fatalf("resume argv retained legacy posture: %q", got)
	}
}

func TestHostedResumeUsesDesiredIntentWhileMetadataAPIUsesHistoricalDisplay(t *testing.T) {
	metadata, executions := newTestHostedControl(t)
	const hostedID = "nht_historical_resume"
	canonicalCwd := t.TempDir()
	if err := metadata.EnsureHostedThread(hostedID, "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureEnsureSessionWithIdentity(executions, hostedID, "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := metadata.ClaimNativeID(hostedID, "claude", "native-historical", false); err != nil {
		t.Fatal(err)
	}
	for _, event := range []metadataEvent{
		// The old creation request remains an auditable desired choice. The
		// migration fold reports what the prior attachment actually ran.
		{HostedThreadID: hostedID, Field: postureFieldModel, Value: "opus", Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessHistorical, SourceEventKey: "historical-resume:model:desired"},
		{HostedThreadID: hostedID, Field: postureFieldModel, Value: "claude-opus-5", Disposition: metadataDispositionObserved, Source: metadataSourceMigration, Evidence: "legacy_fold_baseline", Freshness: metadataFreshnessHistorical, SourceEventKey: "historical-resume:model:effective"},
		{HostedThreadID: hostedID, Field: "cwd", Value: canonicalCwd, Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessHistorical, SourceEventKey: "historical-resume:cwd"},
		// A historical clear means a new attachment must not reapply the
		// attachment's stale effort flag.
		{HostedThreadID: hostedID, Field: postureFieldEffort, Value: "", Disposition: metadataDispositionCleared, Source: postureSourceOperator, Freshness: metadataFreshnessHistorical, SourceEventKey: "historical-resume:effort:clear"},
	} {
		if _, err := metadata.AppendEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	attachments, err := hostedstore.NewAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	attachments.Remember(hostedstore.Attachment{
		Provider: "claude", NativeThreadID: "native-historical", HostedThreadID: hostedID,
		Bin: "/claude", Args: []string{"-p", claudeModelFlag, "stale-model", "--effort", "low", "--permission-mode", "bypassPermissions"},
	})
	s := &server{defaults: config.SessionDefaults{CWD: t.TempDir(), ClaudeBin: "/default/claude"}, attachments: attachments, controlMetadata: metadata, agentExecutions: executions, Registry: newSessionRegistry()}

	// The public API is presentation state and must expose the observed fold.
	api := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/"+hostedID+"/metadata", nil))
	if api.Code != http.StatusOK {
		t.Fatalf("metadata status = %d: %s", api.Code, api.Body.String())
	}
	var body hostedstore.MetadataView
	if err := json.Unmarshal(api.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got := string(body.Fields[postureFieldModel].DisplayValue); got != `"claude-opus-5"` {
		t.Fatalf("API display model = %s, want observed fold", got)
	}

	resolved, err := s.attachmentCoordinator().ResolveSpawn(hostedmodel.SpawnRequest{ProviderRequest: providerRequest{Provider: "claude", Mode: "resume", ThreadID: "native-historical"}})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Model != "opus" || resolved.Cwd != canonicalCwd || resolved.Effort != "" {
		t.Fatalf("resume fields = model %q cwd %q effort %q, want desired intent and cleared effort", resolved.Model, resolved.Cwd, resolved.Effort)
	}
	if got := singletonValue(t, resolved.ClaudeArgs, claudeModelFlag); got != "opus" {
		t.Fatalf("Claude fallback argv model = %q, want desired opus: %#v", got, resolved.ClaudeArgs)
	}
	if containsArg(resolved.ClaudeArgs, "--effort") || containsArg(resolved.ClaudeArgs, "low") {
		t.Fatalf("Claude fallback argv retained historical clear's effort: %#v", resolved.ClaudeArgs)
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
			if err := owner.attachmentCoordinator().ApplyCanonicalResumeMetadata(&req); err != nil {
				t.Fatal(err)
			}
			resolved, err := owner.attachmentCoordinator().ResolveClaudeResume(req)
			if err != nil {
				t.Fatal(err)
			}
			args := resolved.ClaudeArgs
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

// namedThreadCatalog adds the optional name-reader surface to the fake.
type namedThreadCatalog struct {
	fakeThreadCatalog
	names map[string]string
}

func (c *namedThreadCatalog) ThreadNameFor(_ context.Context, provider, nativeID string) (string, bool) {
	name, ok := c.names[provider+":"+nativeID]
	return name, ok
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
	s.catalog = &namedThreadCatalog{
		names: map[string]string{"claude:" + claudeResumeThreadID: "overnight refactor"},
	}
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

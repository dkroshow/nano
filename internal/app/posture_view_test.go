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
)

// Item 3 of the live-posture plan: one posture struct serves every surface.
// These tests pin the projection itself (fold-first, live fallback), the
// surfaces that serve it (summary, /state, catalog memory row), resume
// hydration.

// The summary and /state serve the same folded posture — newest decision per
// field — and fall back to live session values where the log is silent.
func TestSummaryAndStateServeFoldedPosture(t *testing.T) {
	sess, _ := claudeTestSession(t)
	sess.hostedThreadID = "nht_view"
	store := newTestControlMetadata(t)
	if err := store.ensureHostedThread("nht_view", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, err := store.beginTransportBinding("nht_view")
	if err != nil {
		t.Fatal(err)
	}
	sess.metadata, sess.attachmentGeneration = store, g
	sess.recordPosture([]postureEntry{
		{Field: postureFieldModel, Value: "sonnet", Source: postureSourceCreation, Evidence: postureEvidenceSpawnRequest},
		{Field: postureFieldEffort, Value: "xhigh", Source: postureSourceCreation, Evidence: postureEvidenceSpawnRequest},
		{Field: postureFieldPermissionMode, Value: "acceptEdits", Source: postureSourceCreation, Evidence: postureEvidenceSpawnRequest},
		{Field: postureFieldModel, Value: "haiku", Source: postureSourceOperator, Evidence: postureEvidenceSetModel},
	}, metadataFreshnessLive)

	want := sessionPosture{Model: "haiku", Effort: "xhigh", PermissionMode: "acceptEdits"}
	if got := sess.summary().Posture; got != want {
		t.Fatalf("summary posture = %+v, want %+v", got, want)
	}

	w := httptest.NewRecorder()
	sess.handleState(w, httptest.NewRequest(http.MethodGet, "/api/sessions/x/state", nil))
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
	legacy, _ := claudeTestSession(t)
	legacy.hostedThreadID = "nht_legacy"
	legacy.model = "claude-opus-5"
	if got := legacy.summary().Posture.Model; got != "" {
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
	if err := metadata.ensureHostedThread("nht_resume_canonical", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := metadata.claimNativeID("nht_resume_canonical", "claude", "native-resume", false); err != nil {
		t.Fatal(err)
	}
	for _, event := range []metadataEvent{
		{HostedThreadID: "nht_resume_canonical", Field: "cwd", Value: canonicalCwd, Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive},
		{HostedThreadID: "nht_resume_canonical", Field: postureFieldModel, Value: "canonical-model", Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive},
		{HostedThreadID: "nht_resume_canonical", Field: postureFieldPermissionMode, Value: "acceptEdits", Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive},
	} {
		if _, err := metadata.appendEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	attachments, err := newAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	attachments.remember(attachmentRecord{
		Provider: "claude", NativeThreadID: "native-resume", HostedThreadID: "nht_resume_canonical",
		Cwd: legacyCwd, Model: "legacy-model", Bin: "/claude", Args: []string{"-p", claudeModelFlag, "legacy-model", "--permission-mode", "bypassPermissions"},
	})
	s := &server{defaults: sessionDefaults{cwd: legacyCwd, claudeBin: "/claude"}, attachments: attachments, controlMetadata: metadata}
	resolved, err := s.resolveSpawn(spawnRequest{Provider: "claude", Mode: "resume", ThreadID: "native-resume"})
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
	if err := metadata.ensureHostedThread(hostedID, "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := executions.ensureSessionWithIdentity(hostedID, "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := metadata.claimNativeID(hostedID, "claude", "native-historical", false); err != nil {
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
		if _, err := metadata.appendEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	attachments, err := newAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	attachments.remember(attachmentRecord{
		Provider: "claude", NativeThreadID: "native-historical", HostedThreadID: hostedID,
		Bin: "/claude", Args: []string{"-p", claudeModelFlag, "stale-model", "--effort", "low", "--permission-mode", "bypassPermissions"},
	})
	s := &server{defaults: sessionDefaults{cwd: t.TempDir(), claudeBin: "/default/claude"}, attachments: attachments, controlMetadata: metadata, agentExecutions: executions, sessionRegistry: sessionRegistry{sessions: map[string]*session{}}}

	// The public API is presentation state and must expose the observed fold.
	api := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/"+hostedID+"/metadata", nil))
	if api.Code != http.StatusOK {
		t.Fatalf("metadata status = %d: %s", api.Code, api.Body.String())
	}
	var body hostedMetadataView
	if err := json.Unmarshal(api.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got := string(body.Fields[postureFieldModel].DisplayValue); got != `"claude-opus-5"` {
		t.Fatalf("API display model = %s, want observed fold", got)
	}

	resolved, err := s.resolveSpawn(spawnRequest{Provider: "claude", Mode: "resume", ThreadID: "native-historical"})
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
	row := memoryThreadRow(sum)
	cells := map[string]catalogValue{
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

	unknown := memoryThreadRow(sessionSummary{ID: "sess-2", Provider: "claude"})
	if unknown.Metadata.Effort.State != "unknown" {
		t.Fatalf("empty posture effort cell = %+v, want honest unknown", unknown.Metadata.Effort)
	}
}

// A resumed codex session re-seeds its per-turn overrides from the fold, so a
// click-resume and a boot reattach converge. A pre-log thread (nil fold)
// falls back to the attachment record's stored tier — the boot special case
// retired into this one seam — while any recorded history makes the log the
// only truth.
func TestSeedCodexTurnOverridesFromFold(t *testing.T) {
	folded := map[string]postureEntry{
		postureFieldModel:       {Field: postureFieldModel, Value: "gpt-5.3-codex", Source: postureSourceOperator},
		postureFieldEffort:      {Field: postureFieldEffort, Value: "high", Source: postureSourceCreation},
		postureFieldServiceTier: {Field: postureFieldServiceTier, Value: "priority", Source: postureSourceOperator},
	}

	codex := &session{provider: "codex",
		lifecycleState: testLifecycleSnapshot("ready", false)}
	codex.seedCodexTurnOverrides(folded, "ignored-legacy")
	if codex.turnModel != "gpt-5.3-codex" || codex.turnEffort != "high" || codex.turnServiceTier != "priority" {
		t.Fatalf("codex overrides = %q/%q/%q, want the folded posture seeded",
			codex.turnModel, codex.turnEffort, codex.turnServiceTier)
	}

	legacy := &session{provider: "codex",
		lifecycleState: testLifecycleSnapshot("ready", false)}
	legacy.seedCodexTurnOverrides(nil, "priority")
	if legacy.turnServiceTier != "priority" {
		t.Fatalf("legacy tier = %q, want the pre-log record honored on a nil fold", legacy.turnServiceTier)
	}

	cleared := &session{provider: "codex",
		turnServiceTier: "stale",
		lifecycleState:  testLifecycleSnapshot("ready", false)}
	cleared.seedCodexTurnOverrides(map[string]postureEntry{}, "stale-record")
	if cleared.turnServiceTier != "stale" {
		t.Fatalf("cleared-history tier = %q, want the record ignored once any history exists", cleared.turnServiceTier)
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
			if err := metadata.ensureHostedThread(hostedID, "claude", presentationShared, "", "", ""); err != nil {
				t.Fatal(err)
			}
			var desired metadataEvent
			var err error
			if test.settlement == metadataDispositionCleared {
				desired, err = metadata.appendEvent(metadataEvent{HostedThreadID: hostedID, Field: postureFieldModel, Value: "", Disposition: metadataDispositionCleared, Source: postureSourceOperator, Freshness: metadataFreshnessLive, SourceEventKey: test.name + ":cleared"})
			} else if test.desired != "" {
				desired, err = metadata.appendEvent(metadataEvent{HostedThreadID: hostedID, Field: postureFieldModel, Value: test.desired, Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, SourceEventKey: test.name + ":requested"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.settlement == metadataDispositionConfirmed || test.settlement == metadataDispositionRejected {
				if _, err := metadata.appendEvent(metadataEvent{HostedThreadID: hostedID, Field: postureFieldModel, Value: test.desired, Disposition: test.settlement, Source: postureSourceProvider, Freshness: metadataFreshnessLive, CausationEventID: &desired.ID, SourceEventKey: test.name + ":settlement"}); err != nil {
					t.Fatal(err)
				}
			}
			if test.effective != "" && (test.settlement != metadataDispositionConfirmed || test.effective != test.desired) {
				if _, err := metadata.appendEvent(metadataEvent{HostedThreadID: hostedID, Field: postureFieldModel, Value: test.effective, Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, SourceEventKey: test.name + ":effective"}); err != nil {
					t.Fatal(err)
				}
			}
			view, err := metadata.view(hostedID)
			if err != nil {
				t.Fatal(err)
			}
			codex := &session{turnModel: "stale",
				turnServiceTier: "stale",
				lifecycleState:  testLifecycleSnapshot("ready", false)}
			view.Fields[postureFieldServiceTier] = view.Fields[postureFieldModel]
			codex.seedCodexCanonicalOverrides(view)
			if codex.turnModel != test.want || codex.turnServiceTier != test.want {
				t.Fatalf("Codex resume = %q/%q, want %q", codex.turnModel, codex.turnServiceTier, test.want)
			}
			args := applyClaudeResumeOverlay([]string{"-p", claudeModelFlag, "stale"}, metadata, hostedID)
			if test.want == "" {
				args = claudeCanonicalResumeArgs(args, spawnRequest{})
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
		c.reply(call, map[string]any{
			"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID,
			"state": "running", "lastSeq": 1,
		})
		c.line(1, "recv", claudeInitLine)
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 1})
	}))
	s, store := claudeResumeServer(t, shim.wsURL())
	s.catalog = &namedThreadCatalog{
		names: map[string]string{"claude:" + claudeResumeThreadID: "overnight refactor"},
	}
	store.remember(rememberedClaudeSession(t, "/stored/claude"))

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
		c.replyError(call, shimCodeSessionNotFound, "session_not_found")
	}))
	s, store := claudeResumeServer(t, shim.wsURL())
	rec := rememberedClaudeSession(t, "/stored/claude")
	rec.HostedThreadID = "nht_overlay"
	store.remember(rec)
	if _, err := s.agentExecutions.ensureSessionWithIdentity("nht_overlay", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.controlMetadata.claimNativeID("nht_overlay", "claude", claudeResumeThreadID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.controlMetadata.appendEvent(metadataEvent{HostedThreadID: "nht_overlay", Field: "cwd", Value: rec.Cwd, Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive, SourceEventKey: "fixture:nht_overlay:cwd"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.controlMetadata.appendEvent(metadataEvent{HostedThreadID: "nht_overlay", Field: postureFieldPermissionMode, Value: "bypassPermissions", Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive, SourceEventKey: "fixture:nht_overlay:permission"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.controlMetadata.appendEvent(metadataEvent{HostedThreadID: "nht_overlay", Field: postureFieldModel, Value: "haiku", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Evidence: postureEvidenceSetModel, Freshness: metadataFreshnessLive}); err != nil {
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
	view, err := s.controlMetadata.view("nht_overlay")
	if err != nil || view.Fields[postureFieldModel].DesiredEventID == nil {
		t.Fatalf("canonical overlay intent missing after fallback: view=%#v err=%v", view, err)
	}
}

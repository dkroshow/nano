package hosted

import (
	"encoding/json"
	"errors"
	"github.com/dkroshow/nano/internal/config"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/testsupport"
	"github.com/dkroshow/nano/internal/threads"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Catches: a Codex child started without a sandbox field getting a read-only
// sandbox, against the permissive default launch policy.
// Boundary: Nano's flag defaults, which the launchd job leaves unset, through
// the start resolver that fills an omitted sandbox and approval.
func TestCodexStartWithoutSandboxGetsPermissiveDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	s := newTestAttachmentOwner(AttachmentSources{Defaults: config.SessionDefaults{CWD: t.TempDir(), Sandbox: cfg.Sandbox, Approval: cfg.Approval}})
	req, err := s.resolveSpawn(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "codex", Mode: "start"}})
	if err != nil {
		t.Fatal(err)
	}
	if req.Sandbox != "danger-full-access" || req.Approval != "never" {
		t.Fatalf("omitted Codex posture resolved to sandbox=%q approval=%q", req.Sandbox, req.Approval)
	}
}

// The app-server's thread/start rejects sessionStartSource outside
// startup|clear as an unknown enum variant. Catching a bad value here, before
// the app-server sees it, keeps a caller's field mix-up (this value belongs
// on threadSource, a free-text tag) from minting a session that can never
// start — observed 2026-08-06: five background workers stuck exited, each
// carrying rpc error -32600 unknown variant `nano`.
func TestResolveSpawnRejectsInvalidSessionStartSource(t *testing.T) {
	cwd := t.TempDir()
	s := newTestAttachmentOwner(AttachmentSources{Defaults: config.SessionDefaults{CWD: cwd}})
	_, err := s.resolveSpawn(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "codex", Mode: "start", Cwd: cwd, SessionStartSource: "nano"}})
	if err == nil || !strings.Contains(err.Error(), "sessionStartSource") {
		t.Fatalf("resolveSpawn error = %v, want a sessionStartSource complaint", err)
	}
}

func TestResolveSpawnAcceptsKnownSessionStartSourceValues(t *testing.T) {
	cwd := t.TempDir()
	s := newTestAttachmentOwner(AttachmentSources{Defaults: config.SessionDefaults{CWD: cwd}})
	for _, value := range []string{"", "startup", "clear"} {
		if _, err := s.resolveSpawn(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "codex", Mode: "start", Cwd: cwd, SessionStartSource: value}}); err != nil {
			t.Fatalf("resolveSpawn(%q) error = %v", value, err)
		}
	}
}

func TestResolveSpawnDefaultsCodexServiceTierToStandard(t *testing.T) {
	cwd := t.TempDir()
	s := newTestAttachmentOwner(AttachmentSources{Defaults: config.SessionDefaults{CWD: cwd}})

	resolved, err := s.resolveSpawn(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "codex", Mode: "start", Cwd: cwd}})
	if err != nil {
		t.Fatalf("resolve codex start: %v", err)
	}
	if resolved.ServiceTier != "default" {
		t.Fatalf("default service tier = %q, want explicit Standard", resolved.ServiceTier)
	}

	resolved, err = s.resolveSpawn(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "codex", Mode: "start", Cwd: cwd, ServiceTier: "priority"}})
	if err != nil {
		t.Fatalf("resolve priority codex start: %v", err)
	}
	if resolved.ServiceTier != "priority" {
		t.Fatalf("explicit service tier = %q, want priority", resolved.ServiceTier)
	}
}

func TestResolveGrokSpawnAcceptsSupportedSessionPermissionPosture(t *testing.T) {
	cwd := t.TempDir()
	s := newTestAttachmentOwner(AttachmentSources{Defaults: config.SessionDefaults{CWD: cwd}})
	for _, mode := range []string{"", "auto", "bypassPermissions"} {
		resolved, err := s.resolveSpawn(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "grok", Mode: "start", Cwd: cwd, PermissionMode: mode}})
		if err != nil || resolved.PermissionMode != mode {
			t.Fatalf("resolveSpawn(%q) = mode %q, error %v", mode, resolved.PermissionMode, err)
		}
	}
	_, err := s.resolveSpawn(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "grok", Mode: "start", Cwd: cwd, PermissionMode: "acceptEdits"}})
	if err == nil || !strings.Contains(err.Error(), "Grok permissionMode") {
		t.Fatalf("unsupported resolveSpawn error = %v", err)
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
		store.Remember(hostedstore.Attachment{
			Provider: provider, NativeThreadID: "native-" + provider,
			HostedThreadID: hosted, Cwd: cwd, AttachedAt: "2026-08-06T10:00:00Z",
		})
		if _, err := fixtureEnsureSessionWithIdentity(executions, hosted, provider, threads.PresentationShared, "", "", ""); err != nil {
			t.Fatal(err)
		}
		if err := metadata.ClaimNativeID(hosted, provider, "native-"+provider, false); err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.AppendEvent(hostedstore.MetadataEvent{HostedThreadID: hosted, Field: "cwd", Value: cwd, Disposition: hostedstore.DispositionRequested, Source: hostedstore.PostureSourceCreation, Freshness: hostedstore.FreshnessLive}); err != nil {
			t.Fatal(err)
		}
	}
	s := newTestAttachmentOwner(AttachmentSources{Defaults: config.SessionDefaults{CWD: cwd, ClaudeBin: "/fake/claude"}, Attachments: store, Metadata: metadata, Executions: executions})

	for provider, want := range map[string]string{
		"claude": "nht_claude", "codex": "nht_codex", "grok": "nht_grok",
	} {
		resolved, err := s.resolveSpawn(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: provider, Mode: "resume", ThreadID: "native-" + provider}})
		if err != nil {
			t.Fatalf("resolve %s resume: %v", provider, err)
		}
		if resolved.HostedThreadID != want {
			t.Fatalf("%s resume hostedThreadID = %q, want the stored %q", provider, resolved.HostedThreadID, want)
		}
	}
	if _, err := fixtureEnsureSessionWithIdentity(executions, "nht_explicit", "claude", threads.PresentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := metadata.ClaimNativeID("nht_explicit", "claude", "native-explicit", false); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.AppendEvent(hostedstore.MetadataEvent{HostedThreadID: "nht_explicit", Field: "cwd", Value: cwd, Disposition: hostedstore.DispositionRequested, Source: hostedstore.PostureSourceCreation, Freshness: hostedstore.FreshnessLive}); err != nil {
		t.Fatal(err)
	}

	resolved, err := s.resolveSpawn(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "claude", Mode: "resume", ThreadID: "native-explicit", HostedThreadID: "nht_explicit"}})
	if err != nil {
		t.Fatalf("resolve explicit resume: %v", err)
	}
	if resolved.HostedThreadID != "nht_explicit" {
		t.Fatalf("explicit hostedThreadID = %q, want the request's own to win", resolved.HostedThreadID)
	}
}

// The provider transport builder requires an explicit cwd without an attachment.
// Public resume separately requires a canonical owner before reaching this seam.
func TestClaudeResumeTransportWithoutAttachment(t *testing.T) {
	s, _ := newResumeCoordinator(t)
	_, err := s.resolveClaudeResume(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "claude", Mode: "resume", ThreadID: claudeResumeThreadID}})
	if err == nil || !strings.Contains(err.Error(), "no attachment record") {
		t.Fatalf("err = %v, want a refusal to guess the cwd", err)
	}
	cwd := t.TempDir()
	req, err := s.resolveClaudeResume(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "claude", Mode: "resume", ThreadID: claudeResumeThreadID, Cwd: cwd}})
	if err != nil {
		t.Fatal(err)
	}
	args := sessions.ClaudeCommand(req.ProviderRequest)[1:]
	want := append(sessions.ClaudeArgs(sessions.ProviderRequest{AllowedTools: "Read,Grep"}), "--resume", claudeResumeThreadID)
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("record-less transport args = %#v, want %#v", args, want)
	}
	if req.Cwd != cwd || req.ClaudeBin != sessions.ResolveClaudeBin("/default/claude") {
		t.Fatalf("bin = %q cwd = %q, want explicit cwd and configured binary", req.ClaudeBin, req.Cwd)
	}
}

// Canonical posture passes into the provider builder after ownership resolves.
func TestClaudeResumeTransportWithoutAttachmentCarriesPosture(t *testing.T) {
	s, _ := newResumeCoordinator(t)
	req, err := s.resolveClaudeResume(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "claude", Mode: "resume", ThreadID: claudeResumeThreadID, Cwd: t.TempDir(),
		Model: "claude-opus-4-1", PermissionMode: "bypassPermissions", Effort: "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	args := sessions.ClaudeCommand(req.ProviderRequest)[1:]
	want := []string{
		"-p", "--verbose", "--input-format", "stream-json",
		"--output-format", "stream-json", "--include-partial-messages",
		"--permission-prompt-tool", "stdio",
		"--model", "claude-opus-4-1", "--effort", "high",
		"--permission-mode", "bypassPermissions",
		"--resume", claudeResumeThreadID,
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("posture argv = %#v, want %#v", args, want)
	}
}

// Once an attachment names a hosted identity, its retired JSON posture fields
// are transport-history only.  A resume must take cwd/model/permission from
// control.sqlite even when the old record says something different.
func TestHostedResumeSeedsOnlyCanonicalMetadata(t *testing.T) {
	metadata := newTestControlMetadata(t)
	canonicalCwd := t.TempDir()
	legacyCwd := t.TempDir()
	if err := metadata.EnsureHostedThread("nht_resume_canonical", "claude", threads.PresentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := metadata.ClaimNativeID("nht_resume_canonical", "claude", "native-resume", false); err != nil {
		t.Fatal(err)
	}
	for _, event := range []hostedstore.MetadataEvent{
		{HostedThreadID: "nht_resume_canonical", Field: "cwd", Value: canonicalCwd, Disposition: hostedstore.DispositionRequested, Source: hostedstore.PostureSourceCreation, Freshness: hostedstore.FreshnessLive},
		{HostedThreadID: "nht_resume_canonical", Field: hostedstore.PostureFieldModel, Value: "canonical-model", Disposition: hostedstore.DispositionRequested, Source: hostedstore.PostureSourceCreation, Freshness: hostedstore.FreshnessLive},
		{HostedThreadID: "nht_resume_canonical", Field: hostedstore.PostureFieldPermissionMode, Value: "acceptEdits", Disposition: hostedstore.DispositionRequested, Source: hostedstore.PostureSourceCreation, Freshness: hostedstore.FreshnessLive},
	} {
		if _, err := metadata.AppendEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	Attachments, err := hostedstore.NewAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	Attachments.Remember(hostedstore.Attachment{
		Provider: "claude", NativeThreadID: "native-resume", HostedThreadID: "nht_resume_canonical",
		Cwd: legacyCwd, Model: "legacy-model", Bin: "/claude", Args: []string{"-p", "--model", "legacy-model", "--permission-mode", "bypassPermissions"},
	})
	s := newTestAttachmentOwner(AttachmentSources{Defaults: config.SessionDefaults{CWD: legacyCwd, ClaudeBin: "/claude"}, Attachments: Attachments, Metadata: metadata})
	resolved, err := s.resolveSpawn(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "claude", Mode: "resume", ThreadID: "native-resume"}})
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
	if err := metadata.EnsureHostedThread(hostedID, "claude", threads.PresentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureEnsureSessionWithIdentity(executions, hostedID, "claude", threads.PresentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := metadata.ClaimNativeID(hostedID, "claude", "native-historical", false); err != nil {
		t.Fatal(err)
	}
	for _, event := range []hostedstore.MetadataEvent{
		// The old creation request remains an auditable desired choice. The
		// migration fold reports what the prior attachment actually ran.
		{HostedThreadID: hostedID, Field: hostedstore.PostureFieldModel, Value: "opus", Disposition: hostedstore.DispositionRequested, Source: hostedstore.PostureSourceCreation, Freshness: hostedstore.FreshnessHistorical, SourceEventKey: "historical-resume:model:desired"},
		{HostedThreadID: hostedID, Field: hostedstore.PostureFieldModel, Value: "claude-opus-5", Disposition: hostedstore.DispositionObserved, Source: hostedstore.SourceMigration, Evidence: "legacy_fold_baseline", Freshness: hostedstore.FreshnessHistorical, SourceEventKey: "historical-resume:model:effective"},
		{HostedThreadID: hostedID, Field: "cwd", Value: canonicalCwd, Disposition: hostedstore.DispositionRequested, Source: hostedstore.PostureSourceCreation, Freshness: hostedstore.FreshnessHistorical, SourceEventKey: "historical-resume:cwd"},
		// A historical clear means a new attachment must not reapply the
		// attachment's stale effort flag.
		{HostedThreadID: hostedID, Field: hostedstore.PostureFieldEffort, Value: "", Disposition: hostedstore.DispositionCleared, Source: hostedstore.PostureSourceOperator, Freshness: hostedstore.FreshnessHistorical, SourceEventKey: "historical-resume:effort:clear"},
	} {
		if _, err := metadata.AppendEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	Attachments, err := hostedstore.NewAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	Attachments.Remember(hostedstore.Attachment{
		Provider: "claude", NativeThreadID: "native-historical", HostedThreadID: hostedID,
		Bin: "/claude", Args: []string{"-p", "--model", "stale-model", "--effort", "low", "--permission-mode", "bypassPermissions"},
	})
	s := newTestAttachmentOwner(AttachmentSources{Defaults: config.SessionDefaults{CWD: t.TempDir(), ClaudeBin: "/default/claude"}, Attachments: Attachments, Metadata: metadata, Executions: executions, Registry: newOwnerRegistry()})

	// The public API is presentation state and must expose the observed fold.
	api := httptest.NewRecorder()
	(&ViewService{Metadata: metadata, Executions: executions, Sessions: s.Registry}).HandleMetadataRead(api, httptest.NewRequest(http.MethodGet, "/metadata", nil), threads.SessionRecord{ID: hostedID})
	if api.Code != http.StatusOK {
		t.Fatalf("metadata status = %d: %s", api.Code, api.Body.String())
	}
	var body hostedstore.MetadataView
	if err := json.Unmarshal(api.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got := string(body.Fields[hostedstore.PostureFieldModel].DisplayValue); got != `"claude-opus-5"` {
		t.Fatalf("API display model = %s, want observed fold", got)
	}

	resolved, err := s.resolveSpawn(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "claude", Mode: "resume", ThreadID: "native-historical"}})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Model != "opus" || resolved.Cwd != canonicalCwd || resolved.Effort != "" {
		t.Fatalf("resume fields = model %q cwd %q effort %q, want desired intent and cleared effort", resolved.Model, resolved.Cwd, resolved.Effort)
	}
	if got := testsupport.SingletonValue(t, resolved.ClaudeArgs, "--model"); got != "opus" {
		t.Fatalf("Claude fallback argv model = %q, want desired opus: %#v", got, resolved.ClaudeArgs)
	}
	if slices.Contains(resolved.ClaudeArgs, "--effort") || slices.Contains(resolved.ClaudeArgs, "low") {
		t.Fatalf("Claude fallback argv retained historical clear's effort: %#v", resolved.ClaudeArgs)
	}
}

func TestNativeRecoveryRetiredAliasCannotResume(t *testing.T) {
	control, executions, record := retiredRecoveryFixture(t)
	s := newTestAttachmentOwner(AttachmentSources{Metadata: control, Executions: executions})
	for _, hostedID := range []string{"", record.ID} {
		req := SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: "recovery-old", HostedThreadID: hostedID}}
		if err := s.resolveResumeHostedIdentity(&req); err == nil {
			t.Fatal("retired alias resumed")
		}
	}
	view, err := control.View(record.ID)
	if err != nil || view.NativeThreadID != "recovery-new" {
		t.Fatal("retired alias reclaimed current identity")
	}
}

func TestTransferNativeClaimConflictsWhenOwnerIsLiveAttached(t *testing.T) {
	_, store := newTestHostedControl(t)
	owner := seedHostedNative(t, store, "claude", revivedNativeID, "ready")
	challenger, err := fixtureEnsureSessionWithIdentity(store, "", "claude", threads.PresentationShared, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	registry := newOwnerRegistry()
	live := readyClaimSession(t, sessions.Config{ID: "live", Provider: "claude", HostedThreadID: owner.ID}, revivedNativeID)
	registry.RegisterSession(live)
	s := newTestAttachmentOwner(AttachmentSources{Metadata: store.Control(), Executions: store, Registry: registry})

	if err := s.TransferNativeClaim(challenger.ID, "claude", revivedNativeID); !errors.Is(err, errNativeClaimLive) {
		t.Fatalf("transfer err = %v, want errNativeClaimLive", err)
	}
	if got := activeOwner(t, store.Control(), "claude", revivedNativeID); got != owner.ID {
		t.Fatalf("active owner moved to %q", got)
	}
}

func TestResumeReusesExitedOwnerInsteadOfMinting(t *testing.T) {
	s, _ := newResumeCoordinator(t)
	old := seedHostedNative(t, s.Executions, "claude", claudeResumeThreadID, sessions.StatusExited)
	if _, err := s.Metadata.AppendEvent(hostedstore.MetadataEvent{
		HostedThreadID: old.ID, Field: "cwd", Value: s.Defaults.CWD,
		Disposition: hostedstore.DispositionRequested, Source: hostedstore.PostureSourceCreation, Freshness: hostedstore.FreshnessLive,
	}); err != nil {
		t.Fatal(err)
	}
	req := SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "claude", Mode: "resume", ThreadID: claudeResumeThreadID}}
	resolved, err := s.resolveSpawn(req)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.HostedThreadID != old.ID {
		t.Fatalf("resume hosted id = %q, want original %q", resolved.HostedThreadID, old.ID)
	}
	if activeOwner(t, s.Metadata, "claude", claudeResumeThreadID) != old.ID {
		t.Fatal("resume minted or transferred away from the original owner")
	}
}

func TestResumeHistoricalHostedIDRedirectsToCurrentOwner(t *testing.T) {
	s, _ := newResumeCoordinator(t)
	old := seedHostedNative(t, s.Executions, "claude", claudeResumeThreadID, sessions.StatusExited)
	current := seedHostedNative(t, s.Executions, "claude", "cs_tmp", threads.AgentSessionDetached)
	if err := s.Metadata.TransferNativeClaim(current.ID, "claude", claudeResumeThreadID, false); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if _, err := s.Metadata.AppendEvent(hostedstore.MetadataEvent{
		HostedThreadID: current.ID, Field: "cwd", Value: cwd,
		Disposition: hostedstore.DispositionRequested, Source: hostedstore.PostureSourceCreation, Freshness: hostedstore.FreshnessLive,
	}); err != nil {
		t.Fatal(err)
	}
	req := SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "claude", Mode: "resume", ThreadID: claudeResumeThreadID, HostedThreadID: old.ID}}
	resolved, err := s.resolveSpawn(req)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.HostedThreadID != current.ID {
		t.Fatalf("redirected hosted id = %q, want current %q", resolved.HostedThreadID, current.ID)
	}
	if activeOwner(t, s.Metadata, "claude", claudeResumeThreadID) != current.ID {
		t.Fatal("historical resume transferred the claim back")
	}
}

func TestInheritMissingCwdFromPredecessor(t *testing.T) {
	control, store := newTestHostedControl(t)
	old := seedHostedNative(t, store, "claude", revivedNativeID, sessions.StatusExited)
	cwd := t.TempDir()
	if _, err := control.AppendEvent(hostedstore.MetadataEvent{
		HostedThreadID: old.ID, Field: "cwd", Value: cwd,
		Disposition: hostedstore.DispositionRequested, Source: hostedstore.PostureSourceCreation, Freshness: hostedstore.FreshnessLive,
	}); err != nil {
		t.Fatal(err)
	}
	current := seedHostedNative(t, store, "claude", "cs_tmp", threads.AgentSessionDetached)
	s := newTestAttachmentOwner(AttachmentSources{Metadata: control, Executions: store, Registry: newOwnerRegistry()})
	if err := s.repairCanonicalNativeIdentity("claude", revivedNativeID, current.ID); err != nil {
		t.Fatal(err)
	}
	if activeOwner(t, control, "claude", revivedNativeID) != current.ID {
		t.Fatal("repair did not transfer the claim")
	}
	view, err := control.View(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotCwd, ok := hostedstore.ResumeString(view.Fields["cwd"])
	if !ok || gotCwd != cwd {
		t.Fatalf("inherited cwd = %q ok=%v", gotCwd, ok)
	}

}

func TestResumeRefusesMissingCanonicalOwner(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {
		for _, named := range []bool{false, true} {
			t.Run(provider+map[bool]string{false: "/native", true: "/hosted"}[named], func(t *testing.T) {
				control, store := newTestHostedControl(t)
				old := seedHostedNative(t, store, provider, revivedNativeID, sessions.StatusExited)
				if _, err := control.CanonicalDatabase().Exec(`UPDATE hosted_thread_native_ids SET retired_at='retired' WHERE hosted_thread_id=?`, old.ID); err != nil {
					t.Fatal(err)
				}
				Attachments := newTestStore(t)
				Attachments.Remember(hostedstore.Attachment{Provider: provider, NativeThreadID: revivedNativeID, HostedThreadID: old.ID})
				app := newTestAttachmentOwner(AttachmentSources{Metadata: control, Executions: store, Attachments: Attachments})
				req := SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: provider, Mode: "resume", ThreadID: revivedNativeID}}
				if named {
					req.HostedThreadID = old.ID
				}
				before := req
				if err := app.resolveResumeHostedIdentity(&req); err == nil {
					t.Fatal("resume reconstructed ownership from retired history")
				}
				if req.HostedThreadID != before.HostedThreadID || activeOwner(t, control, provider, revivedNativeID) != "" {
					t.Fatal("refused resume mutated request or ownership")
				}
				if rec, ok := store.SessionForNative(provider, revivedNativeID); ok {
					t.Fatalf("current-owner lookup selected retired record %+v", rec)
				}
			})
		}
	}
}

func TestResumeRefusesUnownedNativeBeforeProviderStart(t *testing.T) {
	app, _ := newResumeCoordinator(t)
	before := ownerSessionCount(app.Executions)
	_, err := app.spawn(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "claude", Mode: "resume", ThreadID: revivedNativeID, Cwd: t.TempDir()}})
	if err == nil {
		t.Fatal("unowned native resume created a hosted record")
	}
	if ownerSessionCount(app.Executions) != before || len(app.Registry.SnapshotSessions()) != 0 {
		t.Fatal("unowned resume created session state before refusal")
	}
}

func TestResumeRefusesWithoutCanonicalStore(t *testing.T) {
	app := newTestAttachmentOwner(AttachmentSources{})
	req := SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: revivedNativeID}}
	if err := app.resolveResumeHostedIdentity(&req); err == nil {
		t.Fatal("resume accepted without canonical ownership store")
	}
}

func TestResumeRefusesConflictingCanonicalOwners(t *testing.T) {
	control, store := newTestHostedControl(t)
	first := seedHostedNative(t, store, "claude", revivedNativeID, sessions.StatusExited)
	second := seedHostedNative(t, store, "claude", "other-native", "ready")
	// Only the disposable fixture drops this index to simulate corrupt ownership.
	if _, err := control.CanonicalDatabase().Exec(`DROP INDEX hosted_thread_active_native_id`); err != nil {
		t.Fatal(err)
	}
	if _, err := control.CanonicalDatabase().Exec(`UPDATE hosted_thread_native_ids SET native_thread_id=? WHERE hosted_thread_id=?`, revivedNativeID, second.ID); err != nil {
		t.Fatal(err)
	}
	app := newTestAttachmentOwner(AttachmentSources{Metadata: control, Executions: store})
	for _, named := range []string{"", first.ID, second.ID} {
		req := SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: "claude", Mode: "resume", ThreadID: revivedNativeID, HostedThreadID: named}}
		if err := app.resolveResumeHostedIdentity(&req); err == nil {
			t.Fatal("conflicting owners selected by row order")
		}
		if req.HostedThreadID != named {
			t.Fatal("conflicting request changed hosted identity")
		}
	}
	if rec, ok := store.SessionForNative("claude", revivedNativeID); ok {
		t.Fatalf("current-owner lookup selected conflicted record %+v", rec)
	}
}

// retiredRecoveryFixture represents a journal and identity pair left by a prior build.
func retiredRecoveryFixture(t *testing.T) (*hostedstore.MetadataStore, *hostedstore.ExecutionStore, threads.SessionRecord) {
	t.Helper()
	control, executions := newTestHostedControl(t)
	record := seedHostedNative(t, executions, "codex", "recovery-old", "ready")
	if err := executions.BindSession(record.ID, "recovery-session", "recovery-new", "ready", threads.PresentationShared); err != nil {
		t.Fatal(err)
	}
	if _, err := control.CanonicalDatabase().Exec(`UPDATE hosted_thread_native_ids SET retired_at='2026-01-01T00:00:00Z' WHERE hosted_thread_id=? AND native_thread_id='recovery-old'`, record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := control.CanonicalDatabase().Exec(`INSERT INTO hosted_native_recoveries(hosted_thread_id,client_recovery_id,old_native_id,new_native_id,state,record_json) VALUES(?,'recovery-key','recovery-old','recovery-new','activated','{}')`, record.ID); err != nil {
		t.Fatal(err)
	}
	return control, executions, record
}

func seedHostedNative(t *testing.T, store *hostedstore.ExecutionStore, provider, nativeID, status string) threads.SessionRecord {
	t.Helper()
	record, err := fixtureEnsureSessionWithIdentity(store, "", provider, threads.PresentationShared, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := hostedstore.SkipOwnedNativeClaim(store.Control().ClaimNativeID(record.ID, provider, nativeID, false)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSession(record.ID, "nano-"+record.ID, nativeID, status, ""); err != nil {
		t.Fatal(err)
	}
	updated, ok := store.Session(record.ID)
	if !ok {
		t.Fatal("missing hosted record")
	}
	return updated
}

func activeOwner(t *testing.T, control *hostedstore.MetadataStore, provider, nativeID string) string {
	t.Helper()
	owner, err := control.ActiveNativeOwner(provider, nativeID)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

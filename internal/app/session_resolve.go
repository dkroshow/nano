package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	hostedvalues "github.com/dkroshow/nano/internal/hosted"
)

func newSessionID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// resolveSpawn fills flag defaults and absolutizes cwd — the single place
// spawn parameters get resolved, shared by spawnSession and the preview
// endpoint so the previewed JSON matches what a real spawn would send. Resume
// intentionally avoids applying start defaults for either provider; otherwise
// nano would silently override the posture the resumed session already has.
func (s *server) resolveSpawn(req spawnRequest) (spawnRequest, error) {
	req.Provider = strings.TrimSpace(req.Provider)
	if req.Mode == "" {
		req.Mode = "start"
	}
	presentationMode, err := normalizePresentationMode(req.PresentationMode)
	if err != nil {
		return req, err
	}
	req.PresentationMode = presentationMode
	sidebarPlacement, err := normalizeSidebarPlacement(req.SidebarPlacement)
	if err != nil {
		return req, err
	}
	req.SidebarPlacement = sidebarPlacement
	switch req.Mode {
	case "start", "resume":
	default:
		return req, fmt.Errorf("unknown spawn mode %q", req.Mode)
	}
	if req.Mode == "resume" {
		// A Claude fallback may have to rebuild the complete child command from
		// canonical hosted state alone. The immutable identity block is part of
		// that transport command, so the resume resolver needs the same server
		// origin a fresh start receives below.
		req.apiOrigin = s.defaults.apiOrigin
		req.ThreadID = strings.TrimSpace(req.ThreadID)
		if req.ThreadID == "" {
			return req, fmt.Errorf("threadId is required to resume a thread")
		}
		// Resume routes only through the canonical current native owner. Neither
		// attachment records nor historical activity may mint or reclaim one.
		if err := s.resolveResumeHostedIdentity(&req); err != nil {
			return req, err
		}
		// A hosted attachment is only a transport locator.  Its old cwd/model/
		// tier fields are migration input, never resume authority: once a durable
		// hosted identity exists, the canonical control row supplies every
		// provider setting that can be replayed.
		if req.HostedThreadID != "" {
			if err := s.inheritResumePosture(req.HostedThreadID, req.Provider, req.ThreadID); err != nil {
				return req, err
			}
			if err := s.applyCanonicalResumeMetadata(&req); err != nil {
				return req, err
			}
		}
		switch req.Provider {
		case "codex":
			return s.resolveCodexResume(req)
		case "claude":
			return s.resolveClaudeResume(req)
		case "grok":
			return s.resolveGrokResume(req)
		}
		return req, fmt.Errorf("resume is not supported for provider %q", req.Provider)
	}

	// The app-server's thread/start rejects anything outside this pair as an
	// unknown enum variant — a caller reaching for a free-text origin tag
	// wants threadSource, not this field. Catching it here means a bad value
	// fails the request instead of minting a session that can never start
	// (observed 2026-08-06: five background workers stuck exited, each
	// carrying rpc error -32600 unknown variant `nano`).
	if req.SessionStartSource != "" && req.SessionStartSource != "startup" && req.SessionStartSource != "clear" {
		return req, fmt.Errorf("sessionStartSource must be %q or %q, got %q", "startup", "clear", req.SessionStartSource)
	}

	req.apiOrigin = s.defaults.apiOrigin
	if req.Cwd == "" {
		req.Cwd = s.defaults.cwd
	}
	absCwd, err := filepath.Abs(req.Cwd)
	if err != nil {
		return req, err
	}
	req.Cwd = absCwd
	if req.Model == "" && req.Provider != "grok" {
		req.Model = s.defaults.model
	}
	switch req.Provider {
	case "claude":
		req.ClaudeBin = s.defaults.claudeBin
		req.ClaudeShimWS = s.defaults.claudeShimWS
		if req.AllowedTools == "" && req.PermissionMode != "bypassPermissions" {
			req.AllowedTools = s.defaults.allowedTools
		}
	case "codex":
		if req.Sandbox == "" {
			req.Sandbox = s.defaults.sandbox
		}
		if req.Approval == "" {
			req.Approval = s.defaults.approval
		}
		// Standard is Nano's deliberate default. A raw app-server omission can
		// inherit an operator config that is already Priority, which makes an
		// unspecified worker's speed posture depend on ambient state. Priority
		// is opt-in: callers that want it must send serviceTier=priority.
		if req.ServiceTier == "" {
			req.ServiceTier = "default"
		}
		if err := s.codex.validateUltrafast(req.Model, req.ServiceTier); err != nil {
			return req, err
		}
	case "grok":
		req.GrokBin = s.defaults.grokBin
		req.GrokLeaderSocket = s.defaults.grokLeaderSocket
		switch req.PermissionMode {
		case "", "auto", "bypassPermissions":
		default:
			return req, fmt.Errorf("Grok permissionMode must be %q or %q", "auto", "bypassPermissions")
		}
	default:
		return req, fmt.Errorf("unknown provider %q", req.Provider)
	}
	return req, nil
}

// applyCanonicalResumeMetadata overlays the recoverable provider settings for
// an already-hosted thread.  The JSON attachment remains useful for transport
// handles (Claude argv, shim ids and replay anchors), but it must never be
// consulted for model, effort, permission, service tier or cwd once a hosted
// identity has been established.
func (s *server) applyCanonicalResumeMetadata(req *spawnRequest) error {
	if req == nil || req.HostedThreadID == "" {
		return nil
	}
	if s.controlMetadata == nil || s.controlMetadataErr != "" {
		return errHostedControlUnavailable
	}
	view, err := s.controlMetadata.view(req.HostedThreadID)
	if err != nil {
		return fmt.Errorf("read canonical metadata for hosted thread %s: %w", req.HostedThreadID, err)
	}
	externalPosture, err := s.controlMetadata.wasExternalResumeRegistration(req.HostedThreadID)
	if err != nil {
		return fmt.Errorf("read external Resume origin for hosted thread %s: %w", req.HostedThreadID, err)
	}
	req.externalResumePosture = externalPosture
	// Do not let an API caller's resume body or a retired attachment value fill
	// a missing canonical fact. Resume deliberately uses desired intent rather
	// than the display projection: an imported historical creation/operator
	// request remains what a new attachment should retry, even when the UI
	// displays a later observed provider value. Unknown is explicit state, not
	// permission to inherit today's server defaults.
	req.Cwd, req.Model, req.Effort, req.PermissionMode, req.ServiceTier = "", "", "", "", ""
	for _, field := range []struct {
		name string
		set  func(string)
	}{
		{postureFieldModel, func(value string) { req.Model = value }},
		{postureFieldEffort, func(value string) { req.Effort = value }},
		{postureFieldPermissionMode, func(value string) { req.PermissionMode = value }},
		{postureFieldServiceTier, func(value string) { req.ServiceTier = value }},
		{"cwd", func(value string) { req.Cwd = value }},
	} {
		if value, ok := hostedvalues.ResumeString(view.Fields[field.name]); ok {
			field.set(value)
		}
	}
	return nil
}

// resolveGrokResume restores only the attachment's identity and cwd. The
// native leader owns model and permission posture; session/load is the source
// of truth and Nano does not layer current start defaults over it.
func (s *server) resolveGrokResume(req spawnRequest) (spawnRequest, error) {
	cwd := strings.TrimSpace(req.Cwd)
	if cwd == "" {
		return req, fmt.Errorf("no attachment record for Grok session %s and no cwd supplied", req.ThreadID)
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return req, err
	}
	resumeModel := req.Model
	resumePermissionMode := req.PermissionMode
	req.Cwd = absCwd
	req.Model = ""
	req.Effort = ""
	// session/load, not Nano, restores Grok's native permission posture. Keep
	// the canonical value on the local attachment so Nano can still enforce
	// and audit a thread-scoped bypass request after reconnecting.
	req.PermissionMode = resumePermissionMode
	req.GrokBin = s.defaults.grokBin
	req.GrokLeaderSocket = s.defaults.grokLeaderSocket
	// For a hosted thread this is the canonical desired/effective model loaded
	// by applyCanonicalResumeMetadata.  A non-hosted historical attachment may
	// still supply an explicit request value, but it cannot become authority for
	// a hosted session.
	req.grokResumeModel = resumeModel
	return req, nil
}

// resolveCodexResume keeps a codex resume free of start defaults: the
// app-server already holds the thread's posture, so every start-only field is
// blanked rather than re-sent. The service tier is the exception: Codex resumes
// at its own config's tier, so every resume handshake sends the thread's.
func (s *server) resolveCodexResume(req spawnRequest) (spawnRequest, error) {
	if req.Cwd == "" {
		if req.HostedThreadID != "" {
			return req, fmt.Errorf("canonical cwd is unknown for hosted Codex thread %s", req.HostedThreadID)
		}
		req.Cwd = s.defaults.cwd
	}
	absCwd, err := filepath.Abs(req.Cwd)
	if err != nil {
		return req, err
	}
	req.Cwd = absCwd
	req.Model = ""
	req.Effort = ""
	req.ServiceTier = ""
	req.Sandbox = ""
	req.Approval = ""
	req.Personality = ""
	req.ApprovalsReviewer = ""
	req.Ephemeral = false
	req.BaseInstructions = ""
	req.DeveloperInstructions = ""
	req.ModelProvider = ""
	req.Config = nil
	req.ThreadSource = ""
	req.SessionStartSource = ""
	req.ServiceName = ""
	return req, nil
}

// resolveClaudeResume resolves a claude resume from evidence, not defaults. The
// attachment record left behind by the session being resumed carries recovery
// handles (binary + argv). Cwd and provider settings for hosted threads come
// only from canonical control.sqlite metadata.
//
// Without an attachment, the canonical hosted owner still supplies the cwd
// and posture before this provider builder runs. A missing transport record
// never authorizes public resume to mint a new owner or guess its settings.
func (s *server) resolveClaudeResume(req spawnRequest) (spawnRequest, error) {
	req.ClaudeShimWS = s.defaults.claudeShimWS
	req.ClaudeShimSpools = s.defaults.claudeShimSpools
	if nativeThreadIDProvisional("claude", req.ThreadID) && req.HostedThreadID != "" {
		// An interrupted re-key can leave the handle's record beside the one it
		// wrote for the resolved native. The native's record is the current
		// one, with the anchor of any later turn: resume it. spawnSession
		// retires the handle's record the re-key did not get to forget.
		native, err := s.controlStore().resolvedNative(context.Background(), req.HostedThreadID, "claude", req.ThreadID)
		if err != nil {
			return req, err
		}
		if current, ok := s.attachmentFor("claude", native); native != "" && ok && current.HostedThreadID == req.HostedThreadID {
			req.retiredClaudeHandle, req.ThreadID = req.ThreadID, native
		}
	}
	rec, _ := s.attachmentFor("claude", req.ThreadID)
	cwd := strings.TrimSpace(req.Cwd)
	if cwd == "" {
		if req.HostedThreadID != "" {
			return req, fmt.Errorf("canonical cwd is unknown for hosted Claude thread %s", req.HostedThreadID)
		}
		return req, fmt.Errorf("no attachment record for thread %s and no cwd supplied", req.ThreadID)
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return req, err
	}
	// The cwd must exist for the `--resume` respawn, but not for the shim-attach
	// rung above it, which needs no cwd at all and is the only rung that keeps a
	// mid-flight turn alive. The check therefore lives on that rung
	// (requireResumeCwd, claude.go), not here.
	req.Cwd = absCwd
	req.ClaudeBin = rec.Bin
	req.ClaudeArgs = rec.Args
	req.ClaudeAnchor = rec
	if req.ClaudeBin == "" {
		req.ClaudeBin = resolveClaudeBin(s.defaults.claudeBin)
	}
	if len(req.ClaudeArgs) == 0 {
		// No stored transport command exists, so rebuild one from the request's
		// recorded fields. Existing hosted records retain the legacy allowlist
		// fallback. A first registration must keep an unknown provider allowlist
		// unknown instead of applying a new-session default.
		if !req.externalResumePosture && req.AllowedTools == "" && req.PermissionMode != "bypassPermissions" {
			req.AllowedTools = s.defaults.allowedTools
		}
		req.ClaudeArgs = claudeArgs(req.providerRequest)
	}
	// A stored command supplies the stream transport baseline. A record-less
	// historical resume rebuilds that baseline above. Only after either source
	// exists may canonical hosted posture and identity replace stale values.
	if req.HostedThreadID != "" {
		req.ClaudeArgs = claudeCanonicalResumeArgs(req.ClaudeArgs, req.providerRequest)
	}
	return req, nil
}

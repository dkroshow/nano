package app

import (
	"context"
	"encoding/json"

	"github.com/dkroshow/nano/internal/hosted"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"

	"github.com/dkroshow/nano/internal/threadindex"
)

type externalResumeFixture struct {
	index                                   *threadCatalogService
	store                                   *history.Store
	root                                    history.SourceRoot
	path, provider, native, cwd, permission string
}

func externalResumeCatalog(t *testing.T, provider, nativeID, cwd string) *externalResumeFixture {
	t.Helper()
	f := &externalResumeFixture{provider: provider, native: nativeID, cwd: cwd, permission: "never"}
	kind := history.RootCodexSessions
	root := t.TempDir()
	f.path = filepath.Join(root, "rollout-"+nativeID+".jsonl")
	if provider == "claude" {
		kind = history.RootClaudeProjects
		f.path = filepath.Join(root, "-workspace", nativeID+".jsonl")
	}
	if provider == "grok" {
		kind = history.RootGrokSessions
		f.path = filepath.Join(root, nativeID, "summary.json")
	}
	f.root = history.SourceRoot{Provider: history.Provider(provider), Kind: kind, Path: root}
	f.write(t, nativeID)
	var err error
	f.store, err = history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	f.index, err = newThreadCatalogService(context.Background(), f.store, threadCatalogServiceOptions{Roots: []history.SourceRoot{f.root}, DisableWatcher: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.index.Close)
	f.refresh(t)
	return f
}
func (f *externalResumeFixture) write(t *testing.T, native string) {
	t.Helper()
	var rows []map[string]any
	switch f.provider {
	case "codex":
		rows = []map[string]any{
			{"type": "session_meta", "payload": map[string]any{"id": native, "cwd": f.cwd}},
			{"type": "turn_context", "payload": map[string]any{"cwd": f.cwd, "model": "gpt-external", "effort": "high", "service_tier": "priority", "approval_policy": f.permission}},
		}
	case "claude":
		rows = []map[string]any{
			{"type": "user", "sessionId": native, "cwd": f.cwd, "effort": "high", "permissionMode": f.permission, "timestamp": "2026-10-01T00:00:00Z", "message": map[string]any{"role": "user", "content": "Resume fixture"}},
			{"type": "assistant", "sessionId": native, "timestamp": "2026-10-01T00:00:01Z", "message": map[string]any{"role": "assistant", "model": "gpt-external", "content": []any{map[string]any{"type": "text", "text": "Saved answer"}}}},
		}
	case "grok":
		rows = []map[string]any{{"info": map[string]any{"id": native, "cwd": f.cwd}, "created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:00:01Z", "current_model_id": "gpt-external", "reasoning_effort": "high", "sandbox_profile": f.permission}}
	}
	var out []byte
	for _, row := range rows {
		data, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		out = append(append(out, data...), '\n')
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, out, 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *externalResumeFixture) refresh(t *testing.T) {
	t.Helper()
	f.index.RequestRefresh()
	waitForCatalog(t, func() bool {
		page, err := f.index.ListThreads(context.Background(), threadindex.ListQuery{IncludeSubagents: true, IncludeGuardians: true, IncludeProgrammatic: true, Archived: "include", Limit: 50})
		return err == nil && len(page.Items) == 1
	})
}
func (f *externalResumeFixture) setPermission(t *testing.T, value string) {
	t.Helper()
	f.permission = value
	f.write(t, f.native)
	f.index.FileChanged(f.path)
	waitForCatalog(t, func() bool {
		page, err := f.index.ListThreads(context.Background(), threadindex.ListQuery{IncludeSubagents: true, IncludeGuardians: true, IncludeProgrammatic: true, Archived: "include", Limit: 50})
		return err == nil && len(page.Items) == 1 && knownCatalogValue(page.Items[0].Metadata.Permission) == value
	})
}
func (f *externalResumeFixture) filenameIdentity(t *testing.T) {
	t.Helper()
	roots, err := f.store.ListRoots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sources, err := f.store.LoadSourcesByRoot(context.Background(), roots[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	source := sources[f.path]
	projection := *source.Projection
	projection.IdentityEvidence = history.EvidenceFilenameFallback
	_, err = f.store.ApplySource(context.Background(), history.SourceProjection{Source: source.Source, Status: source.Status, ParserVersion: source.ParserVersion, IndexedAt: time.Now(), Result: &history.ProjectionResult{Projection: projection, CompleteOffset: source.CompleteOffset, CompleteLines: source.CompleteLines}})
	if err != nil {
		t.Fatal(err)
	}
}

func postExternalResume(s *server, provider, nativeID string) *httptest.ResponseRecorder {
	body := `{"provider":"` + provider + `","mode":"resume","threadId":"` + nativeID + `"}`
	rec := httptest.NewRecorder()
	s.handleSessionCreate(rec, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body)))
	return rec
}

func countHostedRecords(t *testing.T, s *server) int {
	t.Helper()
	var count int
	if err := s.controlMetadata.CanonicalDatabase().QueryRow(`SELECT count(*) FROM hosted_threads`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestExternalCodexResumeRegistersOnceAndPreservesHistoryIdentity(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const nativeID = "0199a213-abcd-7000-8000-external001"
	s.catalog = externalResumeCatalog(t, "codex", nativeID, s.defaults.CWD).index

	rec := postExternalResume(s, "codex", nativeID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var summary sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.HostedThreadID == "" || summary.ThreadID != nativeID {
		t.Fatalf("summary = %#v", summary)
	}
	if got := activeOwner(t, s.controlMetadata, "codex", nativeID); got != summary.HostedThreadID {
		t.Fatalf("active owner = %q, want %q", got, summary.HostedThreadID)
	}
	if got := countHostedRecords(t, s); got != 1 {
		t.Fatalf("hosted records = %d, want 1", got)
	}
	ready := waitForSessionStatus(t, s, summary.ID, "ready")
	if ready.ThreadID != nativeID || fake.ResumeCount() != 1 {
		t.Fatalf("ready = %#v resumes=%d", ready, fake.ResumeCount())
	}
	for field, want := range map[string]string{
		"cwd": s.defaults.CWD, postureFieldModel: "gpt-external", postureFieldEffort: "high",
		postureFieldServiceTier: "priority", postureFieldPermissionMode: "never",
	} {
		var encoded string
		if err := s.controlMetadata.CanonicalDatabase().QueryRow(`SELECT value_json FROM thread_metadata_events WHERE hosted_thread_id=? AND field=? AND source_event_key=?`, summary.HostedThreadID, field, "external-resume:codex:"+nativeID+":"+field).Scan(&encoded); err != nil {
			t.Fatalf("read imported field %s: %v", field, err)
		}
		var got string
		if err := json.Unmarshal([]byte(encoded), &got); err != nil || got != want {
			t.Fatalf("imported field %s = %q err=%v, want %q", field, got, err, want)
		}
	}
}

func TestExternalResumePreviewIsReadOnly(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const nativeID = "0199a213-abcd-7000-8000-external002"
	s.catalog = externalResumeCatalog(t, "codex", nativeID, s.defaults.CWD).index

	rec := httptest.NewRecorder()
	body := `{"provider":"codex","mode":"resume","threadId":"` + nativeID + `"}`
	s.handleSpawnPreview(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/preview", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := countHostedRecords(t, s); got != 0 {
		t.Fatalf("preview created %d hosted records", got)
	}
	if got := activeOwner(t, s.controlMetadata, "codex", nativeID); got != "" {
		t.Fatalf("preview claimed native identity for %q", got)
	}
}

func TestConcurrentExternalResumeCreatesOneRegistrationAndOneAttachment(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const nativeID = "0199a213-abcd-7000-8000-external003"
	s.catalog = externalResumeCatalog(t, "codex", nativeID, s.defaults.CWD).index

	recs := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := range recs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recs[i] = postExternalResume(s, "codex", nativeID)
		}(i)
	}
	wg.Wait()
	wins, conflicts := 0, 0
	var winner sessionSummary
	for _, rec := range recs {
		switch rec.Code {
		case http.StatusOK:
			wins++
			if err := json.Unmarshal(rec.Body.Bytes(), &winner); err != nil {
				t.Fatal(err)
			}
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
		}
	}
	if wins != 1 || conflicts != 1 || countHostedRecords(t, s) != 1 {
		t.Fatalf("wins=%d conflicts=%d hosted=%d", wins, conflicts, countHostedRecords(t, s))
	}
	waitForSessionStatus(t, s, winner.ID, "ready")
	if fake.ResumeCount() != 1 {
		t.Fatalf("provider resumes = %d, want 1", fake.ResumeCount())
	}
}

func TestExternalResumeProviderResolversPreserveKnownAndUnknownPosture(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	s.defaults.AllowedTools = "Read,Write"

	for _, provider := range []string{"claude", "grok"} {
		t.Run(provider, func(t *testing.T) {
			nativeID := "external-" + provider
			catalog := externalResumeCatalog(t, provider, nativeID, s.defaults.CWD)
			// Unknown permission must remain unknown; Nano must not substitute
			// its new-session allowlist or a broader permission mode.
			catalog.setPermission(t, "")
			s.catalog = catalog.index
			if s.attachmentsControl != nil {
				s.attachmentsControl.Catalog = catalog.index.Service
			}
			resolved, err := s.attachmentCoordinator().ResolveSpawn(hosted.SpawnRequest{ProviderRequest: providerRequest{Provider: provider, Mode: "resume", ThreadID: nativeID}})
			if err != nil {
				t.Fatal(err)
			}
			if !resolved.ExternalResumePosture || resolved.Cwd != s.defaults.CWD || resolved.PermissionMode != "" {
				t.Fatalf("resolved = %#v", resolved)
			}
			switch provider {
			case "claude":
				joined := strings.Join(resolved.ClaudeArgs, " ")
				if !strings.Contains(joined, "--model gpt-external") || !strings.Contains(joined, "--effort high") {
					t.Fatalf("Claude args lost recorded posture: %q", joined)
				}
				if strings.Contains(joined, "--allowedTools") || strings.Contains(joined, "Read,Write") {
					t.Fatalf("Claude args leaked new-session defaults: %q", joined)
				}
			case "grok":
				if resolved.Model != "" || resolved.Effort != "" || resolved.GrokResumeModel != "gpt-external" {
					t.Fatalf("Grok resume posture = %#v", resolved)
				}
			}
		})
	}
}

func TestExternalResumeProviderFailureKeepsOneRetryableIdentity(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.SetResumeErrorSequence("temporary resume failure", "")
	s := newResumeTestServer(t, fake.WSURL())
	const nativeID = "0199a213-abcd-7000-8000-external006"
	s.catalog = externalResumeCatalog(t, "codex", nativeID, s.defaults.CWD).index

	first := postExternalResume(s, "codex", nativeID)
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	var failed sessionSummary
	if err := json.Unmarshal(first.Body.Bytes(), &failed); err != nil {
		t.Fatal(err)
	}
	failed = waitForSessionStatus(t, s, failed.ID, sessionStatusExited)
	if !strings.Contains(failed.Error, "temporary resume failure") {
		t.Fatalf("failed summary = %#v", failed)
	}

	second := postExternalResume(s, "codex", nativeID)
	if second.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", second.Code, second.Body.String())
	}
	var retried sessionSummary
	if err := json.Unmarshal(second.Body.Bytes(), &retried); err != nil {
		t.Fatal(err)
	}
	if retried.HostedThreadID != failed.HostedThreadID || countHostedRecords(t, s) != 1 {
		t.Fatalf("retry minted a new identity: failed=%#v retried=%#v records=%d", failed, retried, countHostedRecords(t, s))
	}
	waitForSessionStatus(t, s, retried.ID, "ready")
	if fake.ResumeCount() != 2 {
		t.Fatalf("resume attempts=%d, want 2", fake.ResumeCount())
	}
}

func TestExternalClaudeRetryKeepsUnknownAllowlist(t *testing.T) {
	s, _ := claudeResumeServer(t, "")
	s.defaults.ClaudeBin = filepath.Join(t.TempDir(), "missing-claude")
	const nativeID = "external-claude-retry"
	catalog := externalResumeCatalog(t, "claude", nativeID, s.defaults.CWD)
	catalog.setPermission(t, "")
	s.catalog = catalog.index
	if s.attachmentsControl != nil {
		s.attachmentsControl.Catalog = catalog.index.Service
	}

	firstResolved, err := s.attachmentCoordinator().ResolveSpawn(hosted.SpawnRequest{ProviderRequest: providerRequest{Provider: "claude", Mode: "resume", ThreadID: nativeID}})
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(firstResolved.ClaudeArgs, " "); strings.Contains(joined, "--allowed-tools") {
		t.Fatalf("first external Resume introduced an allowlist: %q", joined)
	}
	first := spawnedSummary(t, postExternalResume(s, "claude", nativeID))
	waitForSessionStatus(t, s, first.ID, sessionStatusExited)

	retry, err := s.attachmentCoordinator().ResolveSpawn(hosted.SpawnRequest{ProviderRequest: providerRequest{Provider: "claude", Mode: "resume", ThreadID: nativeID}})
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(retry.ClaudeArgs, " "); strings.Contains(joined, "--allowed-tools") {
		t.Fatalf("retry introduced server default tools absent from the original provider posture: %q", joined)
	}
	second := spawnedSummary(t, postExternalResume(s, "claude", nativeID))
	waitForSessionStatus(t, s, second.ID, sessionStatusExited)
	if second.HostedThreadID != first.HostedThreadID || countHostedRecords(t, s) != 1 {
		t.Fatalf("retry changed identity: first=%#v second=%#v records=%d", first, second, countHostedRecords(t, s))
	}
}

func TestConcurrentExternalClaudeResumePreclaimsBeforeReturn(t *testing.T) {
	releaseAttach := make(chan struct{})
	shim := newFakeShim(t, func(c *shimConn) {
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			switch call.Method {
			case shimMethodAttach:
				<-releaseAttach
				c.ReplyError(call, shimCodeSessionNotFound, "not found")
			case shimMethodStart:
				c.Reply(call, map[string]any{"shimSessionId": "unexpected-second-writer"})
			default:
				c.Reply(call, nil)
			}
		}
	})
	s, _ := claudeResumeServer(t, shim.WSURL())
	const nativeID = "external-claude-concurrent"
	s.catalog = externalResumeCatalog(t, "claude", nativeID, s.defaults.CWD).index

	recorders := make([]*httptest.ResponseRecorder, 2)
	var requests sync.WaitGroup
	for i := range recorders {
		requests.Add(1)
		go func(i int) {
			defer requests.Done()
			recorders[i] = postExternalResume(s, "claude", nativeID)
		}(i)
	}
	requests.Wait()
	close(releaseAttach)

	wins, conflicts := 0, 0
	var winner sessionSummary
	for _, rec := range recorders {
		switch rec.Code {
		case http.StatusOK:
			wins++
			winner = spawnedSummary(t, rec)
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("concurrent Resume status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("concurrent results: wins=%d conflicts=%d", wins, conflicts)
	}
	if winner.ThreadID != nativeID {
		t.Fatalf("winning Resume returned before reserving native identity: threadId=%q", winner.ThreadID)
	}
	waitForSessionStatus(t, s, winner.ID, sessionStatusExited)
	if attaches := len(shim.CallsOf(shimMethodAttach)); attaches != 1 {
		t.Fatalf("provider attach attempts=%d, want exactly one", attaches)
	}
	if starts := len(shim.CallsOf(shimMethodStart)); starts != 0 {
		t.Fatalf("exclusive attach failure started %d replacement Claude writers", starts)
	}
}

func TestConcurrentExternalGrokResumePreclaimsBeforeReturn(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	s := newResumeTestServer(t, "")
	bin := filepath.Join(t.TempDir(), "blocked-grok")
	if err := os.WriteFile(bin, []byte(`#!/bin/sh
IFS= read -r request || exit 1
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
IFS= read -r request || exit 1
while :; do sleep 1; done
`), 0o700); err != nil {
		t.Fatal(err)
	}
	s.defaults.GrokBin = bin
	const nativeID = "external-grok-concurrent"
	s.catalog = externalResumeCatalog(t, "grok", nativeID, s.defaults.CWD).index

	first := spawnedSummary(t, postExternalResume(s, "grok", nativeID))
	if first.ThreadID != nativeID {
		t.Fatalf("first Grok Resume returned before reserving native identity: threadId=%q", first.ThreadID)
	}
	second := postExternalResume(s, "grok", nativeID)
	if second.Code != http.StatusConflict {
		t.Fatalf("second Grok Resume status=%d body=%s, want conflict", second.Code, second.Body.String())
	}
	firstSession := s.LookupSession(first.ID)
	if firstSession == nil {
		t.Fatal("first Grok session disappeared during blocked handshake")
	}
	firstSession.Close()
}

func TestExternalClaudeResumeFailsWithoutExclusiveShimAttachment(t *testing.T) {
	shim := newFakeShim(t, func(c *shimConn) {
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			switch call.Method {
			case shimMethodAttach:
				c.ReplyError(call, shimCodeSessionNotFound, "external process is not shim-owned")
			case shimMethodStart:
				c.Reply(call, map[string]any{"shimSessionId": "unsafe-replacement"})
			default:
				c.Reply(call, nil)
			}
		}
	})
	s, _ := claudeResumeServer(t, shim.WSURL())
	const nativeID = "external-claude-exclusive"
	s.catalog = externalResumeCatalog(t, "claude", nativeID, s.defaults.CWD).index

	first := spawnedSummary(t, postExternalResume(s, "claude", nativeID))
	failed := waitForSessionStatus(t, s, first.ID, sessionStatusExited)
	if !strings.Contains(strings.ToLower(failed.Error), "exclusive") {
		t.Fatalf("failure=%q, want an exclusive-attachment explanation", failed.Error)
	}
	if starts := len(shim.CallsOf(shimMethodStart)); starts != 0 {
		t.Fatalf("unsafe fallback started %d replacement Claude writers", starts)
	}
	if got := countHostedRecords(t, s); got != 1 {
		t.Fatalf("failed exclusive attach created %d hosted records, want one retryable identity", got)
	}
}

func TestExternalClaudeResumeUsesExclusiveShimAttachment(t *testing.T) {
	// The newly minted hosted id has no spool file yet, which only a shim
	// session that has recorded nothing (lastSeq 0) positively explains.
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.Reply(call, map[string]any{
			"shimSessionId": "shim-external", "nativeSessionId": "native-1",
			"state": "running", "lastSeq": 0,
		})
		c.Notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-external", "lastSeq": 0})
	}))
	s, _ := claudeResumeServer(t, shim.WSURL())
	s.catalog = externalResumeCatalog(t, "claude", "native-1", s.defaults.CWD).index

	first := spawnedSummary(t, postExternalResume(s, "claude", "native-1"))
	ready := waitForSessionStatus(t, s, first.ID, "ready")
	if ready.ThreadID != "native-1" {
		t.Fatalf("ready identity=%q, want native-1", ready.ThreadID)
	}
	if starts := len(shim.CallsOf(shimMethodStart)); starts != 0 {
		t.Fatalf("exclusive attach unexpectedly started %d replacement children", starts)
	}
}

func TestExternalResumeRejectsUnsafeCatalogEvidenceWithoutRegistration(t *testing.T) {
	for _, tc := range []struct {
		name, provider, want string
		mutate               func(*testing.T, *externalResumeFixture)
	}{
		{"missing source", "codex", "source", func(t *testing.T, f *externalResumeFixture) {
			if err := os.Remove(f.path); err != nil {
				t.Fatal(err)
			}
		}},
		{"filename identity", "codex", "identity", func(t *testing.T, f *externalResumeFixture) { f.filenameIdentity(t) }},
		{"provider mismatch", "claude", "provider", func(*testing.T, *externalResumeFixture) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAppServer(t)
			s := newResumeTestServer(t, fake.WSURL())
			const nativeID = "0199a213-abcd-7000-8000-external004"
			catalog := externalResumeCatalog(t, tc.provider, nativeID, s.defaults.CWD)
			tc.mutate(t, catalog)
			s.catalog = catalog.index
			if s.attachmentsControl != nil {
				s.attachmentsControl.Catalog = catalog.index.Service
			}
			rec := postExternalResume(s, "codex", nativeID)
			if rec.Code == http.StatusOK || !strings.Contains(strings.ToLower(rec.Body.String()), tc.want) {
				t.Fatalf("status=%d body=%s, want failure containing %q", rec.Code, rec.Body.String(), tc.want)
			}
			if got := countHostedRecords(t, s); got != 0 {
				t.Fatalf("unsafe evidence created %d hosted records", got)
			}
		})
	}
}

func TestExternalResumeRejectsStaleCatalogWhenCurrentSourceChanged(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const nativeID = "0199a213-abcd-7000-8000-external007"
	catalog := externalResumeCatalog(t, "codex", nativeID, s.defaults.CWD)
	s.catalog = catalog.index
	if s.attachmentsControl != nil {
		s.attachmentsControl.Catalog = catalog.index.Service
	}
	// Replace the provider file without refreshing its saved catalog identity.
	catalog.write(t, "a-different-native-thread")
	rec := postExternalResume(s, "codex", nativeID)
	if rec.Code == http.StatusOK || !strings.Contains(strings.ToLower(rec.Body.String()), "current provider source") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := countHostedRecords(t, s); got != 0 {
		t.Fatalf("stale catalog created %d hosted records", got)
	}
}

func TestExternalResumeRejectsUnknownNativeIDWithoutRegistration(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	empty, err := history.Open(filepath.Join(t.TempDir(), "empty.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = empty.Close() })
	s.catalog = newStoredCatalog(t, empty)
	rec := postExternalResume(s, "codex", "0199a213-abcd-7000-8000-unknown")
	if rec.Code == http.StatusOK || !strings.Contains(strings.ToLower(rec.Body.String()), "not found") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := countHostedRecords(t, s); got != 0 {
		t.Fatalf("unknown native id created %d hosted records", got)
	}
}

func TestExternalResumeRejectsRecoveryHeldNativeIDWithoutRegistration(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const nativeID = "0199a213-abcd-7000-8000-recovery"
	s.catalog = externalResumeCatalog(t, "codex", nativeID, s.defaults.CWD).index
	hosted := seedHostedNative(t, s.agentExecutions, "codex", "recovery-old", sessionStatusExited)
	if _, err := s.controlMetadata.CanonicalDatabase().Exec(`INSERT INTO hosted_native_recoveries(hosted_thread_id,client_recovery_id,old_native_id,new_native_id,state,record_json) VALUES(?,?,?,?,?,?)`, hosted.ID, "external-resume-test", "recovery-old", nativeID, "created", `{}`); err != nil {
		t.Fatal(err)
	}
	rec := postExternalResume(s, "codex", nativeID)
	if rec.Code == http.StatusOK || !strings.Contains(strings.ToLower(rec.Body.String()), "recovery") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := countHostedRecords(t, s); got != 1 {
		t.Fatalf("recovery-held native id created a registration: %d hosted records", got)
	}
}

func TestExternalResumeRejectsRetiredHostedHistory(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const nativeID = "0199a213-abcd-7000-8000-external005"
	s.catalog = externalResumeCatalog(t, "codex", nativeID, s.defaults.CWD).index
	old := seedHostedNative(t, s.agentExecutions, "codex", nativeID, sessionStatusExited)
	if _, err := s.controlMetadata.CanonicalDatabase().Exec(`UPDATE hosted_thread_native_ids SET retired_at='retired' WHERE hosted_thread_id=? AND native_thread_id=?`, old.ID, nativeID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.controlMetadata.CanonicalDatabase().Exec(`UPDATE hosted_threads SET current_native_id='' WHERE hosted_thread_id=?`, old.ID); err != nil {
		t.Fatal(err)
	}
	rec := postExternalResume(s, "codex", nativeID)
	if rec.Code == http.StatusOK || !strings.Contains(strings.ToLower(rec.Body.String()), "previously hosted") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := countHostedRecords(t, s); got != 1 {
		t.Fatalf("retired history was duplicated: %d records", got)
	}
}

// Every boot registration checks its thread for the external-resume marker
// while holding the one control-database writer. The check must find the
// marker without scanning every metadata event: with LIKE it read all of them,
// about 141,000 on the live store, once per registered thread.
func TestExternalResumeMarkerCheckDoesNotScanEveryMetadataEvent(t *testing.T) {
	if raceEnabled {
		t.Skip("a timing bound: -race slows every SQLite step about twentyfold, and the retry tests cover the marker's answers")
	}
	control := newTestControlMetadata(t)
	for _, id := range []string{"nht_marked", "nht_plain"} {
		if err := control.EnsureHostedThread(id, "codex", presentationShared, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := control.CanonicalDatabase().Exec(`INSERT INTO thread_metadata_events(hosted_thread_id,field,disposition,source,evidence_freshness,source_event_key,observed_at,committed_at)
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<100000)
		SELECT 'nht_plain','cwd',?,?,?,'other:'||i,?,? FROM n`, metadataDispositionConfirmed, postureSourceOperator, metadataFreshnessLive, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := control.CanonicalDatabase().Exec(`INSERT INTO thread_metadata_events(hosted_thread_id,field,disposition,source,evidence_freshness,source_event_key,observed_at,committed_at)
		VALUES('nht_marked','cwd',?,?,?,'external-resume:codex:native:cwd',?,?)`, metadataDispositionConfirmed, postureSourceOperator, metadataFreshnessLive, now, now); err != nil {
		t.Fatal(err)
	}

	const checks = 20
	started := time.Now()
	for range checks {
		marked, err := control.WasExternalResumeRegistration("nht_marked")
		if err != nil || !marked {
			t.Fatalf("marked thread = %v, %v; want its external-resume marker found", marked, err)
		}
		plain, err := control.WasExternalResumeRegistration("nht_plain")
		if err != nil || plain {
			t.Fatalf("plain thread = %v, %v; want no marker among its 100,000 other events", plain, err)
		}
	}
	took := time.Since(started)
	// A scan of every event takes several milliseconds per check here.
	if took > 50*time.Millisecond {
		t.Fatalf("%d marker checks over 100,001 events took %s; want an index search", 2*checks, took)
	}
	t.Logf("%d marker checks over 100,001 events in %s", 2*checks, took)
}

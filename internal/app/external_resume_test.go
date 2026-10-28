package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/threadindex"

	"github.com/dkroshow/nano/internal/history"
)

type externalResumeVerifyingCatalog struct {
	*fakeThreadCatalog
	err error
}

func (c *externalResumeVerifyingCatalog) VerifyExternalResumeSource(context.Context, string, string, string) (history.ThreadProjection, error) {
	return history.ThreadProjection{}, c.err
}

func externalResumeCatalog(t *testing.T, provider, nativeID, cwd string) *fakeThreadCatalog {
	t.Helper()
	source := filepath.Join(t.TempDir(), "native.jsonl")
	if err := os.WriteFile(source, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	known := func(value string) threadindex.Value {
		evidence := string(history.EvidenceNativeDurable)
		return threadindex.Value{State: string(history.MetadataKnown), Value: &value, Evidence: &evidence}
	}
	row := catalogThread{Thread: threadindex.Thread{ID: "nth_external", Provider: provider, NativeID: nativeID, Kind: "main", Metadata: threadindex.Metadata{
		CWD: known(cwd), Model: known("gpt-external"), Effort: known("high"),
		Priority: known("priority"), Permission: known("never"),
	}, Source: threadindex.SourceSummary{Status: string(history.SourcePresent)}}}
	detail := row
	detail.Sources = &[]threadindex.SourceDetail{{
		RootKind: "codex_sessions", Path: source, Status: string(history.SourcePresent),
		IdentityEvidence: string(history.EvidenceNativeDurable),
	}}
	return &fakeThreadCatalog{
		thread:  detail,
		rescued: map[string]catalogThread{nativeID: row},
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
	s.catalog = externalResumeCatalog(t, "codex", nativeID, s.defaults.cwd)

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
		"cwd": s.defaults.cwd, postureFieldModel: "gpt-external", postureFieldEffort: "high",
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
	s.catalog = externalResumeCatalog(t, "codex", nativeID, s.defaults.cwd)

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
	s.catalog = externalResumeCatalog(t, "codex", nativeID, s.defaults.cwd)

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
	s.defaults.allowedTools = "Read,Write"

	for _, provider := range []string{"claude", "grok"} {
		t.Run(provider, func(t *testing.T) {
			nativeID := "external-" + provider
			catalog := externalResumeCatalog(t, provider, nativeID, s.defaults.cwd)
			// Unknown permission must remain unknown; Nano must not substitute
			// its new-session allowlist or a broader permission mode.
			catalog.thread.Metadata.Permission = unknownCatalogValue()
			s.catalog = catalog
			resolved, err := s.resolveSpawn(spawnRequest{ProviderRequest: providerRequest{Provider: provider, Mode: "resume", ThreadID: nativeID}})
			if err != nil {
				t.Fatal(err)
			}
			if !resolved.externalResume || resolved.Cwd != s.defaults.cwd || resolved.PermissionMode != "" {
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
	s.catalog = externalResumeCatalog(t, "codex", nativeID, s.defaults.cwd)

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
	s.defaults.claudeBin = filepath.Join(t.TempDir(), "missing-claude")
	const nativeID = "external-claude-retry"
	catalog := externalResumeCatalog(t, "claude", nativeID, s.defaults.cwd)
	catalog.thread.Metadata.Permission = unknownCatalogValue()
	s.catalog = catalog

	firstResolved, err := s.resolveSpawn(spawnRequest{ProviderRequest: providerRequest{Provider: "claude", Mode: "resume", ThreadID: nativeID}})
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(firstResolved.ClaudeArgs, " "); strings.Contains(joined, "--allowed-tools") {
		t.Fatalf("first external Resume introduced an allowlist: %q", joined)
	}
	first := spawnedSummary(t, postExternalResume(s, "claude", nativeID))
	waitForSessionStatus(t, s, first.ID, sessionStatusExited)

	retry, err := s.resolveSpawn(spawnRequest{ProviderRequest: providerRequest{Provider: "claude", Mode: "resume", ThreadID: nativeID}})
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
	s.catalog = externalResumeCatalog(t, "claude", nativeID, s.defaults.cwd)

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
	s.defaults.grokBin = bin
	const nativeID = "external-grok-concurrent"
	s.catalog = externalResumeCatalog(t, "grok", nativeID, s.defaults.cwd)

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
	s.catalog = externalResumeCatalog(t, "claude", nativeID, s.defaults.cwd)

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
	s.catalog = externalResumeCatalog(t, "claude", "native-1", s.defaults.cwd)

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
		name   string
		mutate func(*fakeThreadCatalog)
		want   string
	}{
		{"missing source", func(c *fakeThreadCatalog) { (*c.thread.Sources)[0].Status = string(history.SourceMissing) }, "source"},
		{"filename identity", func(c *fakeThreadCatalog) {
			(*c.thread.Sources)[0].IdentityEvidence = string(history.EvidenceFilenameFallback)
		}, "identity"},
		{"provider mismatch", func(c *fakeThreadCatalog) { c.thread.Provider = "claude"; c.rescued[c.thread.NativeID] = c.thread }, "provider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAppServer(t)
			s := newResumeTestServer(t, fake.WSURL())
			const nativeID = "0199a213-abcd-7000-8000-external004"
			catalog := externalResumeCatalog(t, "codex", nativeID, s.defaults.cwd)
			tc.mutate(catalog)
			s.catalog = catalog
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
	s.catalog = &externalResumeVerifyingCatalog{
		fakeThreadCatalog: externalResumeCatalog(t, "codex", nativeID, s.defaults.cwd),
		err:               errors.New("current source identity changed"),
	}
	rec := postExternalResume(s, "codex", nativeID)
	if rec.Code == http.StatusOK || !strings.Contains(strings.ToLower(rec.Body.String()), "current source") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := countHostedRecords(t, s); got != 0 {
		t.Fatalf("stale catalog created %d hosted records", got)
	}
}

func TestExternalResumeRejectsUnknownNativeIDWithoutRegistration(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	s.catalog = &fakeThreadCatalog{rescued: map[string]catalogThread{}}
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
	s.catalog = externalResumeCatalog(t, "codex", nativeID, s.defaults.cwd)
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
	s.catalog = externalResumeCatalog(t, "codex", nativeID, s.defaults.cwd)
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

package app

import (
	"errors"
	"testing"
	"time"
)

func TestNativeRecoveryRetiredAliasCannotResume(t *testing.T) {
	control, executions, record := retiredRecoveryFixture(t)
	s := &server{controlMetadata: control, agentExecutions: executions}
	for _, hosted := range []string{"", record.ID} {
		req := spawnRequest{Provider: "codex", Mode: "resume", ThreadID: "recovery-old", HostedThreadID: hosted}
		if err := s.resolveResumeHostedIdentity(&req); err == nil {
			t.Fatal("retired alias resumed")
		}
	}
	view, err := control.view(record.ID)
	if err != nil || view.NativeThreadID != "recovery-new" {
		t.Fatal("retired alias reclaimed current identity")
	}
}

// retiredRecoveryFixture represents a journal and identity pair left by a prior build.
func retiredRecoveryFixture(t *testing.T) (*controlMetadataStore, *agentExecutionStore, agentSessionRecord) {
	t.Helper()
	control, executions := newTestHostedControl(t)
	record := seedHostedNative(t, executions, "codex", "recovery-old", "ready")
	if err := executions.bindSession(record.ID, "recovery-session", "recovery-new", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	if _, err := control.db.Exec(`UPDATE hosted_thread_native_ids SET retired_at='2026-01-01T00:00:00Z' WHERE hosted_thread_id=? AND native_thread_id='recovery-old'`, record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := control.db.Exec(`INSERT INTO hosted_native_recoveries(hosted_thread_id,client_recovery_id,old_native_id,new_native_id,state,record_json) VALUES(?,'recovery-key','recovery-old','recovery-new','activated','{}')`, record.ID); err != nil {
		t.Fatal(err)
	}
	return control, executions, record
}

const revivedNativeID = "f2000067-f9db-51d0-919d-079cc251e891"

func seedHostedNative(t *testing.T, store *agentExecutionStore, provider, nativeID, status string) agentSessionRecord {
	t.Helper()
	record, err := store.ensureSessionWithIdentity("", provider, presentationShared, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := skipOwnedNativeClaim(store.control.claimNativeID(record.ID, provider, nativeID, false)); err != nil {
		t.Fatal(err)
	}
	if err := store.updateSession(record.ID, "nano-"+record.ID, nativeID, status, ""); err != nil {
		t.Fatal(err)
	}
	updated, ok := store.session(record.ID)
	if !ok {
		t.Fatal("missing hosted record")
	}
	return updated
}

func activeOwner(t *testing.T, control *controlMetadataStore, provider, nativeID string) string {
	t.Helper()
	owner, err := control.activeNativeOwner(provider, nativeID)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func TestClaimNativeIDDoesNotStealFromExitedOwner(t *testing.T) {
	control, store := newTestHostedControl(t)
	old := seedHostedNative(t, store, "claude", revivedNativeID, sessionStatusExited)
	newRec, err := store.ensureSessionWithIdentity("", "claude", presentationShared, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := control.claimNativeID(newRec.ID, "claude", revivedNativeID, false); !errors.Is(err, errNativeClaimOwned) {
		t.Fatalf("claimNativeID err = %v, want errNativeClaimOwned", err)
	}
	if got := activeOwner(t, control, "claude", revivedNativeID); got != old.ID {
		t.Fatalf("active owner = %q, want original %q", got, old.ID)
	}
}

func TestTransferNativeClaimMovesExitedOwnerAtomically(t *testing.T) {
	control, store := newTestHostedControl(t)
	old := seedHostedNative(t, store, "claude", revivedNativeID, sessionStatusExited)
	newRec := seedHostedNative(t, store, "claude", "cs_other", agentSessionDetached)
	if err := control.transferNativeClaim(newRec.ID, "claude", revivedNativeID, false); err != nil {
		t.Fatal(err)
	}
	if got := activeOwner(t, control, "claude", revivedNativeID); got != newRec.ID {
		t.Fatalf("active owner = %q, want %q", got, newRec.ID)
	}
	var oldRetired string
	if err := control.db.QueryRow(`SELECT COALESCE(retired_at,'') FROM hosted_thread_native_ids WHERE hosted_thread_id=? AND native_thread_id=?`, old.ID, revivedNativeID).Scan(&oldRetired); err != nil || oldRetired == "" {
		t.Fatalf("old claim retired_at = %q err=%v", oldRetired, err)
	}
	var oldCurrent string
	if err := control.db.QueryRow(`SELECT current_native_id FROM hosted_threads WHERE hosted_thread_id=?`, old.ID).Scan(&oldCurrent); err != nil || oldCurrent != revivedNativeID {
		t.Fatalf("old current_native_id = %q err=%v", oldCurrent, err)
	}
}

func TestTransferNativeClaimConflictsWhenOwnerIsLiveAttached(t *testing.T) {
	_, store := newTestHostedControl(t)
	owner := seedHostedNative(t, store, "claude", revivedNativeID, "ready")
	challenger, err := store.ensureSessionWithIdentity("", "claude", presentationShared, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{controlMetadata: store.control, agentExecutions: store, sessions: map[string]*session{
		"live": {id: "live", provider: "claude", hostedThreadID: owner.ID, threadID: revivedNativeID, lifecycleState: testLifecycleSnapshot("ready", false)},
	}}
	if err := s.transferNativeClaim(challenger.ID, "claude", revivedNativeID); !errors.Is(err, errNativeClaimLive) {
		t.Fatalf("transfer err = %v, want errNativeClaimLive", err)
	}
	if got := activeOwner(t, store.control, "claude", revivedNativeID); got != owner.ID {
		t.Fatalf("active owner moved to %q", got)
	}
}

func TestNonOwnerStatusUpdateWhileOwnerDetachedDoesNotMoveClaim(t *testing.T) {
	control, store := newTestHostedControl(t)
	owner := seedHostedNative(t, store, "claude", revivedNativeID, agentSessionDetached)
	other := seedHostedNative(t, store, "claude", "cs_other", "ready")
	sess := &session{hostedThreadID: other.ID,
		provider:        "claude",
		threadID:        revivedNativeID,
		metadata:        control,
		agentExecutions: store,
		lifecycleState:  testLifecycleSnapshot("ready", false)}
	sess.updateHostedThread(agentSessionDetached, "")
	if err := skipOwnedNativeClaim(store.bindSession(other.ID, "nano-other", revivedNativeID, agentSessionDetached, presentationShared)); err != nil {
		t.Fatal(err)
	}
	if got := activeOwner(t, control, "claude", revivedNativeID); got != owner.ID {
		t.Fatalf("active owner = %q, want %q", got, owner.ID)
	}
	updated, ok := store.session(other.ID)
	if !ok || updated.Status != agentSessionDetached {
		t.Fatalf("non-owner status = %+v", updated)
	}
}

func TestResumeReusesExitedOwnerInsteadOfMinting(t *testing.T) {
	s, _ := claudeResumeServer(t, "")
	old := seedHostedNative(t, s.agentExecutions, "claude", claudeResumeThreadID, sessionStatusExited)
	if _, err := s.controlMetadata.appendEvent(metadataEvent{
		HostedThreadID: old.ID, Field: "cwd", Value: s.defaults.cwd,
		Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive,
	}); err != nil {
		t.Fatal(err)
	}
	req := spawnRequest{Provider: "claude", Mode: "resume", ThreadID: claudeResumeThreadID}
	resolved, err := s.resolveSpawn(req)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.HostedThreadID != old.ID {
		t.Fatalf("resume hosted id = %q, want original %q", resolved.HostedThreadID, old.ID)
	}
	if activeOwner(t, s.controlMetadata, "claude", claudeResumeThreadID) != old.ID {
		t.Fatal("resume minted or transferred away from the original owner")
	}
}

func TestResumeHistoricalHostedIDRedirectsToCurrentOwner(t *testing.T) {
	s, _ := claudeResumeServer(t, "")
	old := seedHostedNative(t, s.agentExecutions, "claude", claudeResumeThreadID, sessionStatusExited)
	current := seedHostedNative(t, s.agentExecutions, "claude", "cs_tmp", agentSessionDetached)
	if err := s.controlMetadata.transferNativeClaim(current.ID, "claude", claudeResumeThreadID, false); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if _, err := s.controlMetadata.appendEvent(metadataEvent{
		HostedThreadID: current.ID, Field: "cwd", Value: cwd,
		Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive,
	}); err != nil {
		t.Fatal(err)
	}
	req := spawnRequest{Provider: "claude", Mode: "resume", ThreadID: claudeResumeThreadID, HostedThreadID: old.ID}
	resolved, err := s.resolveSpawn(req)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.HostedThreadID != current.ID {
		t.Fatalf("redirected hosted id = %q, want current %q", resolved.HostedThreadID, current.ID)
	}
	if activeOwner(t, s.controlMetadata, "claude", claudeResumeThreadID) != current.ID {
		t.Fatal("historical resume transferred the claim back")
	}
}

func TestInheritMissingCwdFromPredecessor(t *testing.T) {
	control, store := newTestHostedControl(t)
	old := seedHostedNative(t, store, "claude", revivedNativeID, sessionStatusExited)
	cwd := t.TempDir()
	if _, err := control.appendEvent(metadataEvent{
		HostedThreadID: old.ID, Field: "cwd", Value: cwd,
		Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive,
	}); err != nil {
		t.Fatal(err)
	}
	current := seedHostedNative(t, store, "claude", "cs_tmp", agentSessionDetached)
	s := &server{controlMetadata: control, agentExecutions: store, sessions: map[string]*session{}}
	if err := s.repairCanonicalNativeIdentity("claude", revivedNativeID, current.ID); err != nil {
		t.Fatal(err)
	}
	if activeOwner(t, control, "claude", revivedNativeID) != current.ID {
		t.Fatal("repair did not transfer the claim")
	}
	view, err := control.view(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotCwd, ok := canonicalResumeString(view.Fields["cwd"])
	if !ok || gotCwd != cwd {
		t.Fatalf("inherited cwd = %q ok=%v", gotCwd, ok)
	}

}

func TestBootReattachInheritsCanonicalCwdWithoutJSONCwd(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.reply(call, map[string]any{
			"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID,
			"state": "running", "turnActive": false, "lastSeq": 1,
		})
		c.line(1, "recv", claudeInitLine)
		c.notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 1})
	}))
	s, attachments := bootReattachServer(t, shim.wsURL(), "")
	predecessor := seedHostedNative(t, s.agentExecutions, "claude", claudeResumeThreadID, sessionStatusExited)
	cwd := t.TempDir()
	if _, err := s.controlMetadata.appendEvent(metadataEvent{
		HostedThreadID: predecessor.ID, Field: "cwd", Value: cwd,
		Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive,
	}); err != nil {
		t.Fatal(err)
	}
	current := seedHostedNative(t, s.agentExecutions, "claude", "cs_tmp", agentSessionDetached)
	if err := s.repairCanonicalNativeIdentity("claude", claudeResumeThreadID, current.ID); err != nil {
		t.Fatal(err)
	}
	attachments.remember(attachmentRecord{
		Provider: "claude", NativeThreadID: claudeResumeThreadID, HostedThreadID: current.ID,
		Bin: "/stored/claude", Args: storedPosture, AttachedAt: "2026-08-25T13:02:50Z",
	})
	ensureClaudeTestSpool(t, s, "claude", current.ID)
	s.reattachOnBoot()
	s.mu.Lock()
	var sess *session
	for _, candidate := range s.sessions {
		sess = candidate
	}
	s.mu.Unlock()
	if sess == nil {
		t.Fatal("boot left the revived record detached")
	}
	sum := waitForSessionStatus(t, s, sess.id, "ready")
	if sum.Cwd != cwd || sum.HostedThreadID != current.ID {
		t.Fatalf("boot summary = %#v, want inherited cwd %q on %s", sum, cwd, current.ID)
	}
}

func TestNativeResolversSelectCurrentOwner(t *testing.T) {
	row := catalogThread{ID: "nth_pair", Provider: "claude", NativeID: identityNativeOld, Name: namedCatalog("keyboard owner")}
	s, store := newIdentityServer(t, []catalogThread{row})
	old := createIdentityHosted(t, store, "claude", identityNativeOld)
	current, err := store.ensureSessionWithIdentity("", "claude", presentationShared, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.control.transferNativeClaim(current.ID, "claude", identityNativeOld, false); err != nil {
		t.Fatal(err)
	}
	if err := store.updateSession(current.ID, "nano-current", identityNativeOld, agentSessionDetached, ""); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	if rec, ok := store.sessions[old.ID]; ok {
		rec.UpdatedAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
		store.sessions[old.ID] = rec
	}
	store.mu.Unlock()

	status, body, raw := resolveIdentity(t, s, identityNativeOld)
	if status != 200 || body.HostedThreadID != current.ID || body.MatchCount != 1 {
		t.Fatalf("native identity = %d %#v (%s)", status, body, raw)
	}
	status, body, raw = resolveIdentity(t, s, old.ID)
	if status != 200 || body.HostedThreadID != old.ID {
		t.Fatalf("historical hosted identity = %d %#v (%s)", status, body, raw)
	}
	if rec, ok := store.sessionForNative("claude", identityNativeOld); !ok || rec.ID != current.ID {
		t.Fatalf("sessionForNative = %+v ok=%v", rec, ok)
	}

	refStatus, refBody := resolveReference(t, s, identityNativeOld)
	if refStatus != 200 || refBody.State != "resolved" || refBody.Target == nil || refBody.Target.HostedThreadID != current.ID {
		t.Fatalf("thread-references = %d %#v", refStatus, refBody)
	}
}

func TestNativeResolversSingleRetiredClaimantResolves(t *testing.T) {
	s, store := newIdentityServer(t, nil)
	record := createIdentityHosted(t, store, "claude", identityNativeOld)
	if _, err := store.control.db.Exec(`UPDATE hosted_thread_native_ids SET retired_at=? WHERE hosted_thread_id=?`, time.Now().UTC().Format(time.RFC3339Nano), record.ID); err != nil {
		t.Fatal(err)
	}
	status, body, raw := resolveIdentity(t, s, identityNativeOld)
	if status != 200 || body.HostedThreadID != record.ID || body.MatchCount != 1 {
		t.Fatalf("retired identity = %d %#v (%s)", status, body, raw)
	}
	refStatus, refBody := resolveReference(t, s, identityNativeOld)
	if refStatus != 200 || refBody.State != "resolved" || refBody.Target == nil || refBody.Target.HostedThreadID != record.ID {
		t.Fatalf("retired thread-references = %d %#v", refStatus, refBody)
	}
}

func TestNativeResolversMultipleRetiredClaimantsStayAmbiguous(t *testing.T) {
	s, store := newIdentityServer(t, nil)
	first := createIdentityHosted(t, store, "claude", identityNativeOld)
	second, err := store.ensureSessionWithIdentity("", "claude", presentationShared, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.control.transferNativeClaim(second.ID, "claude", identityNativeOld, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.control.db.Exec(`UPDATE hosted_thread_native_ids SET retired_at=? WHERE native_thread_id=?`, now, identityNativeOld); err != nil {
		t.Fatal(err)
	}
	_ = first
	status, body, raw := resolveIdentity(t, s, identityNativeOld)
	if status != 200 || body.MatchCount != 2 {
		t.Fatalf("ambiguous identity = %d %#v (%s)", status, body, raw)
	}
	refStatus, refBody := resolveReference(t, s, identityNativeOld)
	if refStatus != 200 || refBody.State != "unresolved" || refBody.Reason != "ambiguous" {
		t.Fatalf("ambiguous thread-references = %d %#v", refStatus, refBody)
	}
}

func TestResumeRefusesMissingCanonicalOwner(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {
		for _, named := range []bool{false, true} {
			t.Run(provider+map[bool]string{false: "/native", true: "/hosted"}[named], func(t *testing.T) {
				control, store := newTestHostedControl(t)
				old := seedHostedNative(t, store, provider, revivedNativeID, sessionStatusExited)
				if _, err := control.db.Exec(`UPDATE hosted_thread_native_ids SET retired_at='retired' WHERE hosted_thread_id=?`, old.ID); err != nil {
					t.Fatal(err)
				}
				app := &server{controlMetadata: control, agentExecutions: store,
					attachments: &attachmentStore{byKey: map[attachmentKey]attachmentRecord{
						{provider: provider, nativeThreadID: revivedNativeID}: {Provider: provider, NativeThreadID: revivedNativeID, HostedThreadID: old.ID},
					}},
				}
				req := spawnRequest{Provider: provider, Mode: "resume", ThreadID: revivedNativeID}
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
				if rec, ok := store.sessionForNative(provider, revivedNativeID); ok {
					t.Fatalf("current-owner lookup selected retired record %+v", rec)
				}
			})
		}
	}
}

func TestResumeRefusesUnownedNativeBeforeProviderStart(t *testing.T) {
	app, _ := claudeResumeServer(t, "")
	before := len(app.agentExecutions.sessions)
	_, err := app.spawnSession(spawnRequest{Provider: "claude", Mode: "resume", ThreadID: revivedNativeID, Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("unowned native resume created a hosted record")
	}
	if len(app.agentExecutions.sessions) != before || len(app.sessions) != 0 {
		t.Fatal("unowned resume created session state before refusal")
	}
}

func TestResumeRefusesWithoutCanonicalStore(t *testing.T) {
	app := &server{}
	req := spawnRequest{Provider: "codex", Mode: "resume", ThreadID: revivedNativeID}
	if err := app.resolveResumeHostedIdentity(&req); err == nil {
		t.Fatal("resume accepted without canonical ownership store")
	}
}

func TestResumeRefusesConflictingCanonicalOwners(t *testing.T) {
	control, store := newTestHostedControl(t)
	first := seedHostedNative(t, store, "claude", revivedNativeID, sessionStatusExited)
	second := seedHostedNative(t, store, "claude", "other-native", "ready")
	// Only the disposable fixture drops this index to simulate corrupt ownership.
	if _, err := control.db.Exec(`DROP INDEX hosted_thread_active_native_id`); err != nil {
		t.Fatal(err)
	}
	if _, err := control.db.Exec(`UPDATE hosted_thread_native_ids SET native_thread_id=? WHERE hosted_thread_id=?`, revivedNativeID, second.ID); err != nil {
		t.Fatal(err)
	}
	app := &server{controlMetadata: control, agentExecutions: store}
	for _, named := range []string{"", first.ID, second.ID} {
		req := spawnRequest{Provider: "claude", Mode: "resume", ThreadID: revivedNativeID, HostedThreadID: named}
		if err := app.resolveResumeHostedIdentity(&req); err == nil {
			t.Fatal("conflicting owners selected by row order")
		}
		if req.HostedThreadID != named {
			t.Fatal("conflicting request changed hosted identity")
		}
	}
	if rec, ok := store.sessionForNative("claude", revivedNativeID); ok {
		t.Fatalf("current-owner lookup selected conflicted record %+v", rec)
	}
}

// Explicit fixture setup replaces the retired request-time claim reconstruction.
func seedCanonicalResumeOwner(t *testing.T, app *server, provider, nativeID, cwd string) string {
	t.Helper()
	if owner := activeOwner(t, app.controlMetadata, provider, nativeID); owner != "" {
		return owner
	}
	hostedID := "nht_resume_fixture_" + agentHash([]byte(provider + "\x00" + nativeID))[:16]
	if _, err := app.agentExecutions.ensureSessionWithIdentity(hostedID, provider, presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := app.controlMetadata.appendEvent(metadataEvent{
		HostedThreadID: hostedID, Field: "cwd", Value: cwd, SourceEventKey: "fixture-cwd:" + hostedID,
		Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.controlMetadata.claimNativeID(hostedID, provider, nativeID, nativeThreadIDProvisional(provider, nativeID)); err != nil {
		t.Fatal(err)
	}
	return hostedID
}

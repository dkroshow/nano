package app

import (
	"errors"
	"testing"
	"time"

	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
	"github.com/dkroshow/nano/internal/threads"
)

// retiredRecoveryFixture represents a journal and identity pair left by a prior build.
func retiredRecoveryFixture(t *testing.T) (*hostedstore.MetadataStore, *hostedstore.ExecutionStore, threads.SessionRecord) {
	t.Helper()
	control, executions := newTestHostedControl(t)
	record := seedHostedNative(t, executions, "codex", "recovery-old", "ready")
	if err := executions.BindSession(record.ID, "recovery-session", "recovery-new", "ready", presentationShared); err != nil {
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

const revivedNativeID = "f2000067-f9db-51d0-919d-079cc251e891"

func seedHostedNative(t *testing.T, store *hostedstore.ExecutionStore, provider, nativeID, status string) threads.SessionRecord {
	t.Helper()
	record, err := fixtureEnsureSessionWithIdentity(store, "", provider, presentationShared, "", "", "")
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

func TestClaimNativeIDDoesNotStealFromExitedOwner(t *testing.T) {
	control, store := newTestHostedControl(t)
	old := seedHostedNative(t, store, "claude", revivedNativeID, sessionStatusExited)
	newRec, err := fixtureEnsureSessionWithIdentity(store, "", "claude", presentationShared, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := control.ClaimNativeID(newRec.ID, "claude", revivedNativeID, false); !errors.Is(err, hostedmodel.ErrNativeClaimOwned) {
		t.Fatalf("claimNativeID err = %v, want hosted.ErrNativeClaimOwned", err)
	}
	if got := activeOwner(t, control, "claude", revivedNativeID); got != old.ID {
		t.Fatalf("active owner = %q, want original %q", got, old.ID)
	}
}

func TestTransferNativeClaimMovesExitedOwnerAtomically(t *testing.T) {
	control, store := newTestHostedControl(t)
	old := seedHostedNative(t, store, "claude", revivedNativeID, sessionStatusExited)
	newRec := seedHostedNative(t, store, "claude", "cs_other", agentSessionDetached)
	if err := control.TransferNativeClaim(newRec.ID, "claude", revivedNativeID, false); err != nil {
		t.Fatal(err)
	}
	if got := activeOwner(t, control, "claude", revivedNativeID); got != newRec.ID {
		t.Fatalf("active owner = %q, want %q", got, newRec.ID)
	}
	var oldRetired string
	if err := control.CanonicalDatabase().QueryRow(`SELECT COALESCE(retired_at,'') FROM hosted_thread_native_ids WHERE hosted_thread_id=? AND native_thread_id=?`, old.ID, revivedNativeID).Scan(&oldRetired); err != nil || oldRetired == "" {
		t.Fatalf("old claim retired_at = %q err=%v", oldRetired, err)
	}
	var oldCurrent string
	if err := control.CanonicalDatabase().QueryRow(`SELECT current_native_id FROM hosted_threads WHERE hosted_thread_id=?`, old.ID).Scan(&oldCurrent); err != nil || oldCurrent != revivedNativeID {
		t.Fatalf("old current_native_id = %q err=%v", oldCurrent, err)
	}
}

func TestNonOwnerStatusUpdateWhileOwnerDetachedDoesNotMoveClaim(t *testing.T) {
	control, store := newTestHostedControl(t)
	owner := seedHostedNative(t, store, "claude", revivedNativeID, agentSessionDetached)
	other := seedHostedNative(t, store, "claude", "cs_other", "ready")
	sess := configuredFixtureSession(t, sessionConfig{HostedThreadID: other.ID, Provider: "claude", Metadata: control, AgentExecutions: store}, revivedNativeID, "ready", "", false)
	sess.Close()
	if err := hostedstore.SkipOwnedNativeClaim(store.BindSession(other.ID, "nano-other", revivedNativeID, agentSessionDetached, presentationShared)); err != nil {
		t.Fatal(err)
	}
	if got := activeOwner(t, control, "claude", revivedNativeID); got != owner.ID {
		t.Fatalf("active owner = %q, want %q", got, owner.ID)
	}
	updated, ok := store.Session(other.ID)
	if !ok || updated.Status != agentSessionDetached {
		t.Fatalf("non-owner status = %+v", updated)
	}
}

func TestBootReattachInheritsCanonicalCwdWithoutJSONCwd(t *testing.T) {
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.Reply(call, map[string]any{
			"shimSessionId": "shim-1", "nativeSessionId": claudeResumeThreadID,
			"state": "running", "turnActive": false, "lastSeq": 1,
		})
		c.Line(1, "recv", claudeInitLine)
		c.Notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 1})
	}))
	s, attachments := bootReattachServer(t, shim.WSURL(), "")
	predecessor := seedHostedNative(t, s.agentExecutions, "claude", claudeResumeThreadID, sessionStatusExited)
	cwd := t.TempDir()
	if _, err := s.controlMetadata.AppendEvent(metadataEvent{
		HostedThreadID: predecessor.ID, Field: "cwd", Value: cwd,
		Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive,
	}); err != nil {
		t.Fatal(err)
	}
	current := seedHostedNative(t, s.agentExecutions, "claude", "cs_tmp", agentSessionDetached)
	if err := s.controlMetadata.TransferNativeClaim(current.ID, "claude", claudeResumeThreadID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.controlMetadata.InheritMissingDesiredPosture(current.ID, predecessor.ID); err != nil {
		t.Fatal(err)
	}
	attachments.Remember(hostedstore.Attachment{
		Provider: "claude", NativeThreadID: claudeResumeThreadID, HostedThreadID: current.ID,
		Bin: "/stored/claude", Args: storedPosture, AttachedAt: "2026-08-25T13:02:50Z",
	})
	ensureClaudeTestSpool(t, s, "claude", current.ID)
	openTurnsForBoot(t, s)
	fixtureOwners(s).attachmentCoordinator().ReattachOnBoot()
	var sess *session
	for _, candidate := range s.SnapshotSessions() {
		sess = candidate
	}
	if sess == nil {
		t.Fatal("boot left the revived record detached")
	}
	sum := waitForSessionStatus(t, s, sess.ID(), "ready")
	if sum.Cwd != cwd || sum.HostedThreadID != current.ID {
		t.Fatalf("boot summary = %#v, want inherited cwd %q on %s", sum, cwd, current.ID)
	}
}

func TestNativeResolversSelectCurrentOwner(t *testing.T) {
	row := sidebar.Thread{Thread: threadindex.Thread{ID: "nth_pair", Provider: "claude", NativeID: identityNativeOld, Name: namedCatalog("keyboard owner")}}
	s, store := newIdentityServer(t, []sidebar.Thread{row})
	old := createIdentityHosted(t, store, "claude", identityNativeOld)
	current, err := fixtureEnsureSessionWithIdentity(store, "", "claude", presentationShared, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Control().TransferNativeClaim(current.ID, "claude", identityNativeOld, false); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSession(current.ID, "nano-current", identityNativeOld, agentSessionDetached, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Control().CanonicalDatabase().Exec(`UPDATE hosted_execution_sessions SET updated_at=? WHERE hosted_thread_id=?`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), old.ID); err != nil {
		t.Fatal(err)
	}
	s.agentExecutions, err = newControlAgentExecutionStore(store.Control(), "resolver-read")
	if err != nil {
		t.Fatal(err)
	}

	status, body, raw := resolveIdentity(t, s, identityNativeOld)
	if status != 200 || body.HostedThreadID != current.ID || body.MatchCount != 1 {
		t.Fatalf("native identity = %d %#v (%s)", status, body, raw)
	}
	status, body, raw = resolveIdentity(t, s, old.ID)
	if status != 200 || body.HostedThreadID != old.ID {
		t.Fatalf("historical hosted identity = %d %#v (%s)", status, body, raw)
	}
	if rec, ok := store.SessionForNative("claude", identityNativeOld); !ok || rec.ID != current.ID {
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
	if _, err := store.Control().CanonicalDatabase().Exec(`UPDATE hosted_thread_native_ids SET retired_at=? WHERE hosted_thread_id=?`, time.Now().UTC().Format(time.RFC3339Nano), record.ID); err != nil {
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
	second, err := fixtureEnsureSessionWithIdentity(store, "", "claude", presentationShared, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Control().TransferNativeClaim(second.ID, "claude", identityNativeOld, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.Control().CanonicalDatabase().Exec(`UPDATE hosted_thread_native_ids SET retired_at=? WHERE native_thread_id=?`, now, identityNativeOld); err != nil {
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

// Explicit fixture setup replaces the retired request-time claim reconstruction.
func seedCanonicalResumeOwner(t *testing.T, app *server, provider, nativeID, cwd string) string {
	t.Helper()
	if owner := activeOwner(t, app.controlMetadata, provider, nativeID); owner != "" {
		return owner
	}
	hostedID := "nht_resume_fixture_" + hostedstore.AgentHash([]byte(provider + "\x00" + nativeID))[:16]
	if _, err := fixtureEnsureSessionWithIdentity(app.agentExecutions, hostedID, provider, presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := app.controlMetadata.AppendEvent(metadataEvent{
		HostedThreadID: hostedID, Field: "cwd", Value: cwd, SourceEventKey: "fixture-cwd:" + hostedID,
		Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.controlMetadata.ClaimNativeID(hostedID, provider, nativeID, nativeThreadIDProvisional(provider, nativeID)); err != nil {
		t.Fatal(err)
	}
	return hostedID
}

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	featuremail "github.com/dkroshow/nano/internal/features/mail"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
)

func newTestAgentMessageStore(t *testing.T) *featuremail.Store {
	t.Helper()
	mail, _, err := featuremail.OpenStore(filepath.Join(t.TempDir(), "mail.sqlite"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mail.Close() })
	return mail
}

func TestHostedMetadataAPIReadsCanonicalCurrentState(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread("nht_api", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	generation, err := store.BeginTransportBinding("nht_api")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(hostedstore.MetadataEvent{HostedThreadID: "nht_api", Field: postureFieldServiceTier, Value: "priority", Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive, AttachmentGeneration: generation}); err != nil {
		t.Fatal(err)
	}
	executions := newTestAgentExecutionStore(t, "test")
	if _, err := fixtureEnsureSession(executions, "nht_api", "codex", presentationShared); err != nil {
		t.Fatal(err)
	}
	server := &server{Registry: newSessionRegistry(), agentExecutions: executions, controlMetadata: store}
	rec := httptest.NewRecorder()
	applicationHandler(fixtureOwners(server)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_api/metadata", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata status = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !containsAll(body, `"serviceTier"`, `"priority"`, `"requested"`) {
		t.Fatalf("metadata body = %s", body)
	}
	changes := httptest.NewRecorder()
	applicationHandler(fixtureOwners(server)).ServeHTTP(changes, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_api/metadata/changes?afterRevision=0", nil))
	if changes.Code != http.StatusOK || !containsAll(changes.Body.String(), `"hostedThreadId":"nht_api"`, `"serviceTier"`) {
		t.Fatalf("metadata changes = %d %s", changes.Code, changes.Body.String())
	}
	events := httptest.NewRecorder()
	applicationHandler(fixtureOwners(server)).ServeHTTP(events, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_api/metadata/events?after=0&limit=1", nil))
	if events.Code != http.StatusOK || !containsAll(events.Body.String(), `"events"`, `"nextCursor"`, `"serviceTier"`) {
		t.Fatalf("metadata events = %d %s", events.Code, events.Body.String())
	}
}

func TestHostedMetadataAPIBaseMissingCanonicalRecordReturnsNotFound(t *testing.T) {
	metadata := newTestControlMetadata(t)
	// This deliberately uses the legacy in-memory execution fixture only to
	// exercise a durable execution record whose canonical metadata row was
	// removed. Production control-mode records cannot get into this state
	// because their foreign key owns the hosted identity.
	executions := newTestAgentExecutionStore(t, "test")
	if _, err := fixtureEnsureSession(executions, "nht_missing_metadata", "codex", presentationShared); err != nil {
		t.Fatal(err)
	}
	server := &server{Registry: newSessionRegistry(), agentExecutions: executions, controlMetadata: metadata}
	rec := httptest.NewRecorder()
	applicationHandler(fixtureOwners(server)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_missing_metadata/metadata", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("base metadata missing canonical row = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHostedMetadataEventsAPIUsesPublicFilteredCursorContract(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread("nht_event_api", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	generation, err := store.BeginTransportBinding("nht_event_api")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []hostedstore.MetadataEvent{
		{HostedThreadID: "nht_event_api", Field: postureFieldModel, Value: "old", Disposition: metadataDispositionObserved, Source: metadataSourceMigration, Freshness: metadataFreshnessHistorical, SourceEventKey: "old-model"},
		{HostedThreadID: "nht_event_api", Field: postureFieldServiceTier, Value: "priority", Disposition: metadataDispositionRequested, Source: postureSourceOperator, Freshness: metadataFreshnessLive, AttachmentGeneration: generation, SourceEventKey: "priority"},
		{HostedThreadID: "nht_event_api", Field: postureFieldServiceTier, Value: "priority", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation, SourceEventKey: "priority-confirmed"},
	} {
		if _, err := store.AppendEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	executions, err := newControlAgentExecutionStore(store, "run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureEnsureSession(executions, "nht_event_api", "codex", presentationShared); err != nil {
		t.Fatal(err)
	}
	server := &server{Registry: newSessionRegistry(), agentExecutions: executions, controlMetadata: store}
	rec := httptest.NewRecorder()
	applicationHandler(fixtureOwners(server)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_event_api/metadata/events?after=0&limit=1&field=serviceTier&freshness=live", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("events status = %d: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Events []struct {
			ID             int64           `json:"id"`
			HostedThreadID string          `json:"hostedThreadId"`
			Field          string          `json:"field"`
			Value          json.RawMessage `json:"value"`
			SourceEventKey json.RawMessage `json:"sourceEventKey"`
		} `json:"events"`
		NextCursor int64 `json:"nextCursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].HostedThreadID != "nht_event_api" || page.Events[0].Field != postureFieldServiceTier || len(page.Events[0].SourceEventKey) != 0 || page.NextCursor != page.Events[0].ID {
		t.Fatalf("public event page = %+v", page)
	}
	bad := httptest.NewRecorder()
	applicationHandler(fixtureOwners(server)).ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_event_api/metadata/events?field=not-a-field", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid filter = %d %s", bad.Code, bad.Body.String())
	}
	missing := httptest.NewRecorder()
	applicationHandler(fixtureOwners(server)).ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_missing/metadata/events", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown events = %d %s", missing.Code, missing.Body.String())
	}
}

func TestHostedMetadataAPIDegradedModeDoesNotServePartialCurrent(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread("nht_metadata_degraded", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	executions := newTestAgentExecutionStore(t, "test")
	if _, err := fixtureEnsureSession(executions, "nht_metadata_degraded", "codex", presentationShared); err != nil {
		t.Fatal(err)
	}
	server := &server{Registry: newSessionRegistry(), agentExecutions: executions, controlMetadata: store, controlMetadataErr: "legacy import checksum failed"}
	rec := httptest.NewRecorder()
	applicationHandler(fixtureOwners(server)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_metadata_degraded/metadata", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "legacy import checksum failed") {
		t.Fatalf("degraded metadata response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestExecutionControlDegradedModeRejectsHostedMutations(t *testing.T) {
	server := &server{Registry: newSessionRegistry(), executionControlErr: "control.sqlite is corrupt"}
	rec := httptest.NewRecorder()
	applicationHandler(fixtureOwners(server)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_unavailable", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "control.sqlite is corrupt") {
		t.Fatalf("execution degraded response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSessionsRouteDegradedModeKeepsReadsButRejectsNewHostedSession(t *testing.T) {
	server := &server{Registry: newSessionRegistry(), executionControlErr: "control.sqlite is corrupt"}
	get := httptest.NewRecorder()
	server.sidebarOwner().SessionsHandler(server.defaults)(get, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("degraded GET status = %d", get.Code)
	}
	post := httptest.NewRecorder()
	fixtureOwners(server).hostedController().HandleLegacyCreate(post, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"provider":"codex","mode":"start"}`)))
	if post.Code != http.StatusServiceUnavailable || !strings.Contains(post.Body.String(), "control.sqlite is corrupt") {
		t.Fatalf("degraded POST = %d %s", post.Code, post.Body.String())
	}
}

func TestRecordPostureFailureSeparatesDegradedIngestionFromReadAvailability(t *testing.T) {
	store := newTestControlMetadata(t)
	notified := 0
	sess := newSession(sessions.Config{HostedThreadID: "nht_missing", ID: "nano-1", Metadata: store, RuntimeNotify: func() { notified++ }})
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	sess.RecordCreationPosture(sessions.ProviderRequest{ServiceTier: "priority"})
	if notified != 1 {
		t.Fatalf("runtime notifications = %d", notified)
	}
	if !sess.Summary().MetadataUnavailable {
		t.Fatal("missing canonical record was not unavailable in summary")
	}
	if !sess.Summary().MetadataDegraded {
		t.Fatal("failed metadata ingestion was not degraded in summary")
	}
}

func TestIngestionFailureKeepsLastCommittedCanonicalStateVisible(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread("nht_degraded_readable", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	generation, err := store.BeginTransportBinding("nht_degraded_readable")
	if err != nil {
		t.Fatal(err)
	}
	metadata := &failingMetadataAppend{MetadataStore: store}
	sess := newSession(sessions.Config{HostedThreadID: "nht_degraded_readable", Provider: "codex", ID: "nano-1", Metadata: metadata})
	if err := sess.InitializeBinding(generation); err != nil {
		t.Fatal(err)
	}
	sess.RecordCreationPosture(sessions.ProviderRequest{ServiceTier: "priority"})
	// A rejected newer append must not hide the healthy committed view.
	metadata.fail = true
	sess.RecordCreationPosture(sessions.ProviderRequest{ServiceTier: "bad"})

	summary := sess.Summary()
	if summary.MetadataUnavailable || !summary.MetadataDegraded || summary.Metadata == nil {
		t.Fatalf("degraded readable summary = %+v", summary)
	}
	if summary.Posture.ServiceTier != "priority" {
		t.Fatalf("last committed posture hidden: %+v", summary.Posture)
	}
}

func TestPostureStoreCommitPrecedesRuntimePublication(t *testing.T) {
	store := newTestControlMetadata(t)
	const hosted = "nht_runtime_commit"
	if err := store.EnsureHostedThread(hosted, "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginTransportBinding(hosted); err != nil {
		t.Fatal(err)
	}
	metadata := &atomicFailMetadataAppend{Metadata: store}
	var notified atomic.Int32
	ready := make(chan struct{})
	var readyOnce sync.Once
	var sess *sessions.Session
	f := newProviderRuntimeFixture(t, sessions.Config{ID: "nano-1", Provider: "codex", HostedThreadID: hosted, Metadata: metadata, RuntimeNotify: func() {
		notified.Add(1)
		if sess != nil && sess.Summary().Status == "ready" {
			readyOnce.Do(func() { close(ready) })
		}
	}}, "native-notify", "", func(f *providerRuntimeFixture) { sess = f.session })
	<-ready
	before := notified.Load()
	sess.RecordCreationPosture(sessions.ProviderRequest{ServiceTier: "priority"})
	if got := notified.Load() - before; got != 1 {
		t.Fatalf("runtime notifications = %d, want one committed mutation", got)
	}
	view, err := store.View(hosted)
	if err != nil || view.Fields[postureFieldServiceTier].DesiredDisposition != metadataDispositionRequested {
		t.Fatalf("runtime notification preceded committed current state: view=%+v err=%v", view, err)
	}
	evidence := []byte(`{"method":"thread/settings/updated","params":{"threadId":"native-notify","threadSettings":{"serviceTier":"priority"}}}`)
	f.emitAndDrain(t, evidence)
	if got := notified.Load() - before; got != 2 {
		t.Fatalf("provider runtime notifications = %d, want post-commit notification", got)
	}
	view, err = store.View(hosted)
	if err != nil || view.Fields[postureFieldServiceTier].EffectiveDisposition != metadataDispositionConfirmed {
		t.Fatalf("provider notification preceded committed effective state: view=%+v err=%v", view, err)
	}
	f.emitAndDrain(t, evidence)
	if got := notified.Load() - before; got != 2 {
		t.Fatalf("duplicate provider evidence notified %d times, want 2", got)
	}
	metadata.fail.Store(true)
	f.emitAndDrain(t, []byte(`{"method":"thread/settings/updated","params":{"threadId":"native-notify","threadSettings":{"serviceTier":"default"}}}`))
	if got := notified.Load() - before; got != 3 || !sess.Summary().MetadataDegraded {
		t.Fatalf("failed provider observation did not publish degraded state: notifications=%d degraded=%v", got, sess.Summary().MetadataDegraded)
	}
}

type atomicFailMetadataAppend struct {
	sessions.Metadata
	fail atomic.Bool
}

func (m *atomicFailMetadataAppend) AppendEvent(event hostedstore.MetadataEvent) (hostedstore.MetadataEvent, error) {
	if m.fail.Load() {
		return hostedstore.MetadataEvent{}, errors.New("injected metadata append failure")
	}
	return m.Metadata.AppendEvent(event)
}

func TestMetadataChangesAPIReportsGapsAndDegradedState(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread("nht_hint", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	g, _ := store.BeginTransportBinding("nht_hint")
	if _, err := store.AppendEvent(hostedstore.MetadataEvent{HostedThreadID: "nht_hint", Field: postureFieldModel, Value: "sol", Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: g}); err != nil {
		t.Fatal(err)
	}
	exec := newTestAgentExecutionStore(t, "test")
	if _, err := fixtureEnsureSession(exec, "nht_hint", "codex", presentationShared); err != nil {
		t.Fatal(err)
	}
	server := &server{Registry: newSessionRegistry(), agentExecutions: exec, controlMetadata: store}
	good := httptest.NewRecorder()
	applicationHandler(fixtureOwners(server)).ServeHTTP(good, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_hint/metadata/changes?afterRevision=2", nil))
	if good.Code != http.StatusOK || !containsAll(good.Body.String(), `"currentRevision":3`, `"refetchRequired":false`) {
		t.Fatalf("incremental hints %d %s", good.Code, good.Body.String())
	}
	bad := httptest.NewRecorder()
	applicationHandler(fixtureOwners(server)).ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_hint/metadata/changes?afterRevision=nope", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("malformed cursor=%d", bad.Code)
	}
	unknown := httptest.NewRecorder()
	applicationHandler(fixtureOwners(server)).ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_unknown/metadata/changes?afterRevision=0", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown hint thread=%d", unknown.Code)
	}
	var databasePath string
	if err := store.CanonicalDatabase().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&databasePath); err != nil {
		t.Fatal(err)
	}
	restarted, err := fixtureOpenMetadata(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	server.controlMetadata = restarted
	gap := httptest.NewRecorder()
	applicationHandler(fixtureOwners(server)).ServeHTTP(gap, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_hint/metadata/changes?afterRevision=0", nil))
	if gap.Code != http.StatusOK || !strings.Contains(gap.Body.String(), `"refetchRequired":true`) {
		t.Fatalf("restart gap=%d %s", gap.Code, gap.Body.String())
	}
	server.controlMetadataErr = "degraded"
	degraded := httptest.NewRecorder()
	applicationHandler(fixtureOwners(server)).ServeHTTP(degraded, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_hint/metadata/changes?afterRevision=0", nil))
	if degraded.Code != http.StatusServiceUnavailable {
		t.Fatalf("degraded hints=%d", degraded.Code)
	}
}

func TestHostedMetadataNameUsesThreadNameProvenance(t *testing.T) {
	for _, provenance := range []history.ThreadNameProvenance{history.ThreadNameManual, history.ThreadNameHistoricalImport} {
		t.Run(string(provenance), func(t *testing.T) {
			catalog, saved := fixtureIdentityCatalogStore(t, nil)
			if _, err := saved.SyncThreadNames(context.Background(), []history.ThreadNameRecord{{Provider: history.ProviderCodex, NativeThreadID: "native-name", Name: "durable name", Provenance: provenance}}); err != nil {
				t.Fatal(err)
			}
			server := &server{catalog: catalog}
			view := fixtureOwners(server).hostedViews().DecorateMetadata(hostedstore.MetadataView{Provider: "codex", NativeThreadID: "native-name"}, hostedstore.AttachmentView{State: "ready", Provenance: "live_session"})
			if view.Name.Value != "durable name" || view.Name.Provenance != string(provenance) {
				t.Fatalf("hosted name provenance = %+v", view.Name)
			}
		})
	}
}

// The session list carries each field's displayed value and the revision,
// not the full canonical view (85% of its bytes, read by every page). Catches
// the full view, or its name and native-id history, coming back into the list.
func TestSessionListCarriesDisplayedMetadataOnly(t *testing.T) {
	metadata := newTestControlMetadata(t)
	if err := metadata.EnsureHostedThread("nht_listed", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	sess := configuredFixtureSession(t, sessions.Config{ID: "session-listed", Provider: "codex", HostedThreadID: "nht_listed", CreatedAt: time.Now(), Metadata: metadata, RuntimeNotify: func() {}}, "native-listed", "ready", "")
	sess.RecordCreationPosture(sessions.ProviderRequest{ServiceTier: "priority"})
	server := testServerWithSessions(&server{Registry: newSessionRegistry(), controlMetadata: metadata}, map[string]*sessions.Session{sess.ID(): sess})
	rec := httptest.NewRecorder()
	server.sidebarOwner().SessionsHandler(server.defaults)(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	var body struct {
		Sessions []struct {
			Metadata map[string]json.RawMessage `json:"metadata"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Sessions) != 1 {
		t.Fatalf("sessions = %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	listed := body.Sessions[0].Metadata
	if len(listed) != 2 || listed["revision"] == nil || string(listed["fields"]) != `{"serviceTier":{"displayValue":"priority"}}` {
		t.Fatalf("listed metadata = %v", listed)
	}
}

func TestHostedMetadataAttachmentStateIsReadTimeJoin(t *testing.T) {
	store := newTestControlMetadata(t)
	if err := store.EnsureHostedThread("nht_attachment_api", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	exec, err := newControlAgentExecutionStore(store, "run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureEnsureSessionWithIdentity(exec, "nht_attachment_api", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := exec.UpdateSession("nht_attachment_api", "", "", agentSessionDetached, ""); err != nil {
		t.Fatal(err)
	}
	s := &server{Registry: newSessionRegistry(), agentExecutions: exec, controlMetadata: store}
	before, _ := store.View("nht_attachment_api")
	rec := httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_attachment_api/metadata", nil))
	if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"attachment":{"state":"detached","provenance":"execution_record"`) {
		t.Fatalf("detached metadata = %d %s", rec.Code, rec.Body.String())
	}
	live := newSession(runtimeFixtureConfig(t, sessions.Config{ID: "live", Provider: "codex", HostedThreadID: "nht_attachment_api", Projection: newLiveProjection("codex")}))
	if err := live.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	live.ReserveNativeID("native-attachment-view")
	t.Cleanup(live.Close)

	s.RegisterSession(live)
	rec = httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/nht_attachment_api/metadata", nil))
	if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"state":"starting"`, `"provenance":"live_session"`) {
		t.Fatalf("starting metadata = %d %s", rec.Code, rec.Body.String())
	}
	fake := newFakeAppServer(t)
	fake.SetResumeResult(map[string]any{"thread": map[string]any{"id": live.ThreadID()}})
	if err := live.BeginProviderSpawn(sessions.ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: live.ThreadID()}, fake.daemon(), func() {}); err != nil {
		t.Fatal(err)
	}
	waitForClaudeStatus(t, live, "ready")
	if got := fixtureOwners(s).hostedViews().AttachmentState("nht_attachment_api"); got.State != "ready" || got.Provenance != "live_session" {
		t.Fatalf("ready attachment = %+v", got)
	}
	s.RemoveSession(live.ID())
	if err := exec.UpdateSession("nht_attachment_api", "", "", sessionStatusExited, "provider failed"); err != nil {
		t.Fatal(err)
	}
	if got := fixtureOwners(s).hostedViews().AttachmentState("nht_attachment_api"); got.State != sessionStatusExited || got.Error != "provider failed" {
		t.Fatalf("exited attachment = %+v", got)
	}
	after, _ := store.View("nht_attachment_api")
	if before.Revision != after.Revision {
		t.Fatalf("attachment read advanced metadata revision %d→%d", before.Revision, after.Revision)
	}
	if got := fixtureOwners(s).hostedViews().AttachmentState("missing"); got.State != "unavailable" {
		t.Fatalf("unavailable attachment = %+v", got)
	}
}

func assertCurrentValue(t *testing.T, store *hostedstore.MetadataStore, hostedID, field, wantValue, wantDisposition string) {
	t.Helper()
	var value, disposition string
	if err := store.CanonicalDatabase().QueryRow(`SELECT effective_value_json,effective_disposition FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, hostedID, field).Scan(&value, &disposition); err != nil {
		t.Fatal(err)
	}
	if value != wantValue || disposition != wantDisposition {
		t.Fatalf("effective = %s/%s, want %s/%s", value, disposition, wantValue, wantDisposition)
	}
}

func assertNoEffective(t *testing.T, store *hostedstore.MetadataStore, hostedID, field string) {
	t.Helper()
	var value sql.NullString
	if err := store.CanonicalDatabase().QueryRow(`SELECT effective_value_json FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, hostedID, field).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value.Valid {
		t.Fatalf("unexpected effective value %s", value.String)
	}
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}

// failingMetadataAppend injects a write failure without changing canonical reads.
type failingMetadataAppend struct {
	*hostedstore.MetadataStore
	fail bool
}

func (m *failingMetadataAppend) AppendEvent(event hostedstore.MetadataEvent) (hostedstore.MetadataEvent, error) {
	if m.fail {
		return hostedstore.MetadataEvent{}, errors.New("injected append failure")
	}
	return m.MetadataStore.AppendEvent(event)
}

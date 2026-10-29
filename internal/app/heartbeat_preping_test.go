package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	featureheartbeat "github.com/dkroshow/nano/internal/features/heartbeat"
	"github.com/dkroshow/nano/internal/interact"
)

func TestHeartbeatPrePingRunnerContract(t *testing.T) {
	runner := featureheartbeat.NewPrePingRunner("", "http://127.0.0.1:1")
	input := featureheartbeat.PrePingInput{Version: 1, DueEpochMinute: 42, TargetHostedThreadID: "nht_target", Prompt: "original", Settings: json.RawMessage(`{"ready":true}`)}
	for _, tc := range []struct {
		name, source, decision, prompt, reason string
		wantErr                                bool
	}{
		{"skip", `process.stdout.write(JSON.stringify({decision:"skip",reason:"nothing_due"}))`, "skip", "", "nothing_due", false},
		{"wake replaces prompt", `process.stdout.write(JSON.stringify({decision:"wake",prompt:"replacement",reason:"work_due"}))`, "wake", "replacement", "work_due", false},
		{"wake defaults prompt", `process.stdout.write(JSON.stringify({decision:"wake"}))`, "wake", "original", "", false},
		{"explicit failure", `process.stdout.write(JSON.stringify({decision:"fail",reason:"read_failed"}))`, "fail", "", "read_failed", false},
		{"unknown field", `process.stdout.write(JSON.stringify({decision:"skip",secret:"leak"}))`, "", "", "invalid_output", true},
		{"unknown decision", `process.stdout.write(JSON.stringify({decision:"maybe"}))`, "", "", "unknown_decision", true},
		{"mixed output", `process.stdout.write('{"decision":"wake"}\nextra')`, "", "", "invalid_output", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, audit, err := runner(context.Background(), featureheartbeat.ScriptRevision{ScriptID: "hbs_test", Revision: 3, Source: tc.source}, input)
			if (err != nil) != tc.wantErr || result.Decision != tc.decision || result.Prompt != tc.prompt || audit.Reason != tc.reason {
				t.Fatalf("result=%#v audit=%#v err=%v", result, audit, err)
			}
			if audit.ScriptID != "hbs_test" || audit.Revision != 3 || audit.DurationMS < 0 {
				t.Fatalf("audit identity = %#v", audit)
			}
		})
	}
}

func TestHeartbeatPrePingRunnerTimeoutAndDedupeLimits(t *testing.T) {
	runner := featureheartbeat.NewPrePingRunner("", "")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, audit, err := runner(ctx, featureheartbeat.ScriptRevision{ScriptID: "hbs_timeout", Revision: 1, Source: `setInterval(() => {}, 1000)`}, featureheartbeat.PrePingInput{Version: 1, Prompt: "x", Settings: json.RawMessage(`{}`)})
	if err == nil || audit.Result != featureheartbeat.PrePingFail || audit.Reason != "timeout" {
		t.Fatalf("timeout audit=%#v err=%v", audit, err)
	}

	source := `process.stdout.write(JSON.stringify({decision:"wake",dedupeKey:"x".repeat(4097)}))`
	_, audit, err = runner(context.Background(), featureheartbeat.ScriptRevision{ScriptID: "hbs_large", Revision: 1, Source: source}, featureheartbeat.PrePingInput{Version: 1, Prompt: "x", Settings: json.RawMessage(`{}`)})
	if err == nil || audit.Result != featureheartbeat.PrePingFail || audit.Reason != "dedupe_key_too_large" {
		t.Fatalf("dedupe audit=%#v err=%v", audit, err)
	}
}

func TestInboxMatchTemplatePeeksWithoutReading(t *testing.T) {
	requests := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("peek") != "true" || r.URL.Query().Get("status") != "unread" || r.URL.Query().Get("actorThreadRef") != "nht_target" {
			t.Fatalf("unsafe inbox query = %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"items":[{"seq":1,"status":"unread","sender":{"hostedThreadId":"nht_sender"},"refs":[{"kind":"issue.ref","id":"sample-task"}]}]}`))
	}))
	defer api.Close()
	template, err := featureheartbeat.ScriptTemplateByID("inbox_match")
	if err != nil {
		t.Fatal(err)
	}
	runner := featureheartbeat.NewPrePingRunner("", api.URL)
	input := featureheartbeat.PrePingInput{Version: 1, TargetHostedThreadID: "nht_target", Prompt: "review", Settings: json.RawMessage(`{"senderHostedThreadId":"nht_sender","refKind":"issue.ref","refId":"sample-task"}`)}
	result, audit, err := runner(context.Background(), featureheartbeat.ScriptRevision{ScriptID: "hbs_inbox", Revision: 1, Source: template.Source}, input)
	if err != nil || result.Decision != featureheartbeat.PrePingWake || audit.Reason != "inbox_match" || requests != 1 {
		t.Fatalf("result=%#v audit=%#v requests=%d err=%v", result, audit, requests, err)
	}
}

func TestHeartbeatScriptAPIIsReadOnly(t *testing.T) {
	store := openTestHeartbeatStore(t)
	s := &server{}
	s.heartbeat = featureheartbeat.NewService(store, interact.NewHTTPClient(s.defaults.APIOrigin, nil), true, nil)
	script, _, err := store.CreateScript(featureheartbeat.ScriptRevisionInput{Name: "copy", Source: "process.exit(0)", Actor: featureheartbeat.Actor{Kind: "operator"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/v1/heartbeat/scripts", bytes.NewBufferString(`{"source":"process.exit(0)"}`)),
		httptest.NewRequest(http.MethodPost, "/api/v1/heartbeat/scripts/"+script.ID+"/revisions", bytes.NewBufferString(`{}`)),
	} {
		recorder := httptest.NewRecorder()
		applicationHandler(fixtureOwners(s)).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s = %d %s", request.URL.Path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestHeartbeatAttachmentAPIAcceptsTemplateNotScriptSource(t *testing.T) {
	s, _, _, _, target, _, _ := stage0Server(t)
	store := openTestHeartbeatStore(t)
	s.heartbeat = featureheartbeat.NewService(store, interact.NewHTTPClient(s.defaults.APIOrigin, nil), true, nil)
	body, _ := json.Marshal(map[string]any{
		"targetHostedThreadId": target.ID, "cadenceMinutes": 1,
		"prePing":   map[string]any{"templateId": "inbox_match"},
		"authority": "Operator approved", "actor": map[string]any{"kind": "operator", "name": "Operator"},
	})
	recorder := httptest.NewRecorder()
	fixtureOwners(s).agentController().HandleHeartbeatAttachmentCreate(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/heartbeat/attachments", bytes.NewReader(body)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("attach = %d %s", recorder.Code, recorder.Body.String())
	}
	var attached featureheartbeat.Attachment
	if err := json.Unmarshal(recorder.Body.Bytes(), &attached); err != nil || attached.PrePing == nil || attached.Prompt == "" {
		t.Fatalf("attachment = %#v, %v", attached, err)
	}

	bad := bytes.NewBufferString(`{"targetHostedThreadId":"` + target.ID + `","cadenceMinutes":1,"prompt":"x","prePing":{"scriptId":"hbs_x","revision":1},"actor":{"kind":"operator"}}`)
	recorder = httptest.NewRecorder()
	fixtureOwners(s).agentController().HandleHeartbeatAttachmentCreate(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/heartbeat/attachments", bad))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("raw script reference = %d %s", recorder.Code, recorder.Body.String())
	}
}

// fakeInboxAPI serves unread inbox pages the way Nano does: ordered by seq,
// paged by since and limit. It counts requests so settings errors can prove
// they fail before any read.
type fakeInboxAPI struct {
	unread   []map[string]any
	endless  bool
	status   int
	requests int
}

func (f *fakeInboxAPI) serve(t *testing.T) *httptest.Server {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests++
		query := r.URL.Query()
		if query.Get("peek") != "true" || query.Get("status") != "unread" || query.Get("actorThreadRef") != "nht_target" {
			t.Errorf("unsafe inbox query = %q", r.URL.RawQuery)
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		since, _ := strconv.ParseInt(query.Get("since"), 10, 64)
		limit, _ := strconv.Atoi(query.Get("limit"))
		items := []map[string]any{}
		for seq := since + 1; f.endless && len(items) < limit; seq++ {
			items = append(items, unreadInboxItem(seq, "nht_other", ""))
		}
		for _, item := range f.unread {
			if item["seq"].(int64) > since && len(items) < limit {
				items = append(items, item)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))
	t.Cleanup(api.Close)
	return api
}

func unreadInboxItem(seq int64, sender, refID string) map[string]any {
	refs := []map[string]string{}
	if refID != "" {
		refs = append(refs, map[string]string{"kind": "issue.ref", "id": refID})
	}
	return map[string]any{"seq": seq, "status": "unread", "sender": map[string]string{"hostedThreadId": sender}, "refs": refs}
}

func runInboxMatchTemplate(t *testing.T, apiURL, settings string) (featureheartbeat.PrePingResult, featureheartbeat.PrePingAudit) {
	t.Helper()
	template, err := featureheartbeat.ScriptTemplateByID("inbox_match")
	if err != nil {
		t.Fatal(err)
	}
	input := featureheartbeat.PrePingInput{Version: 1, TargetHostedThreadID: "nht_target", Prompt: "review", Settings: json.RawMessage(settings)}
	result, audit, _ := featureheartbeat.NewPrePingRunner("", apiURL)(context.Background(), featureheartbeat.ScriptRevision{ScriptID: "hbs_inbox", Revision: 1, Source: template.Source}, input)
	return result, audit
}

// A migrated inbox_match copy with empty settings previously failed as
// inbox_read_failed. Boundary: the repository template run by the real
// pre-ping runner against an HTTP inbox.
func TestInboxMatchTemplateEmptySettingsMatchesAnyUnread(t *testing.T) {
	inbox := &fakeInboxAPI{unread: []map[string]any{unreadInboxItem(7, "nht_anyone", "")}}
	api := inbox.serve(t)
	if result, audit := runInboxMatchTemplate(t, api.URL, `{}`); result.Decision != featureheartbeat.PrePingWake || audit.Reason != "inbox_match" {
		t.Fatalf("empty settings with unread mail: result=%#v audit=%#v", result, audit)
	}
	inbox.unread = nil
	if result, audit := runInboxMatchTemplate(t, api.URL, `{}`); result.Decision != featureheartbeat.PrePingSkip || audit.Reason != "inbox_no_match" {
		t.Fatalf("empty settings with empty inbox: result=%#v audit=%#v", result, audit)
	}
}

// The template previously read only the oldest 100 unread messages, so
// a filtered match further back in a large inbox never woke the target.
// Boundary: the template's paging against an HTTP inbox that caps pages at 100.
func TestInboxMatchTemplateReadsPastTheFirstPage(t *testing.T) {
	inbox := &fakeInboxAPI{}
	for seq := int64(1); seq <= 250; seq++ {
		inbox.unread = append(inbox.unread, unreadInboxItem(seq, "nht_other", ""))
	}
	inbox.unread = append(inbox.unread, unreadInboxItem(251, "nht_sender", "sample-task"))
	api := inbox.serve(t)
	result, audit := runInboxMatchTemplate(t, api.URL, `{"senderHostedThreadId":"nht_sender"}`)
	if result.Decision != featureheartbeat.PrePingWake || audit.Reason != "inbox_match" || inbox.requests != 3 {
		t.Fatalf("match on page 3: result=%#v audit=%#v requests=%d", result, audit, inbox.requests)
	}
}

// A configuration error was reported as inbox_read_failed,
// which sent diagnosis toward the inbox API instead of the settings. Boundary:
// the template's failure reason, which the attachment's pause reason quotes.
func TestInboxMatchTemplateSeparatesSettingsErrorsFromReadErrors(t *testing.T) {
	inbox := &fakeInboxAPI{}
	api := inbox.serve(t)
	for _, settings := range []string{`{"refKind":"issue.ref"}`, `{"refId":"sample-task"}`, `{"sender":"nht_sender"}`, `{"senderHostedThreadId":7}`} {
		if result, audit := runInboxMatchTemplate(t, api.URL, settings); result.Decision != featureheartbeat.PrePingFail || audit.Reason != "settings_invalid" {
			t.Fatalf("settings %s: result=%#v audit=%#v", settings, result, audit)
		}
	}
	if inbox.requests != 0 {
		t.Fatalf("settings errors read the inbox %d times", inbox.requests)
	}
	inbox.status = http.StatusServiceUnavailable
	if result, audit := runInboxMatchTemplate(t, api.URL, `{}`); result.Decision != featureheartbeat.PrePingFail || audit.Reason != "inbox_read_failed" {
		t.Fatalf("unavailable inbox: result=%#v audit=%#v", result, audit)
	}
}

// An attachment carrying an
// inbox_match copy with empty settings must wake once for unread mail and stay
// quiet, without pausing, when the inbox is empty. Boundary: the heartbeat
// service's due-minute evaluation with the real runner and template copy.
func TestInboxMatchAttachmentWithEmptySettingsWakesOnlyForUnreadMail(t *testing.T) {
	client := &heartbeatTestClient{lifecycle: interact.LifecycleState{Availability: "available", AttachmentState: "live"}}
	h, rec := heartbeatTestService(t, client)
	now := time.Now().UTC()
	template, err := featureheartbeat.ScriptTemplateByID("inbox_match")
	if err != nil {
		t.Fatal(err)
	}
	_, revision, err := h.Store.CreateScript(featureheartbeat.ScriptRevisionInput{Name: template.Name + " attachment copy", Source: template.Source, DefaultSettings: json.RawMessage(`{}`), TemplateID: template.ID, TemplateRevision: template.Revision, Actor: featureheartbeat.Actor{Kind: "operator"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	rec, err = h.Store.UpdateAttachment(rec.ID, featureheartbeat.AttachmentPatch{PrePingSet: true, PrePing: &featureheartbeat.PrePingReference{ScriptID: revision.ScriptID, Revision: 1, Settings: json.RawMessage(`{}`)}}, "add inbox template", featureheartbeat.Actor{Kind: "operator"}, now)
	if err != nil {
		t.Fatal(err)
	}
	inbox := &fakeInboxAPI{}
	h.PrePing = featureheartbeat.NewPrePingRunner("", inbox.serve(t).URL)

	h.EvaluateDue(context.Background(), rec, 400, now)
	inbox.unread = []map[string]any{unreadInboxItem(1, "nht_anyone", "")}
	h.EvaluateDue(context.Background(), rec, 401, now.Add(time.Minute))
	if len(client.prompts) != 1 {
		t.Fatalf("prompts = %d, want exactly one wake for the unread message", len(client.prompts))
	}
	loaded, err := h.Store.Attachment(rec.ID)
	if err != nil || loaded.State != featureheartbeat.StateActive {
		t.Fatalf("attachment = %#v, %v", loaded, err)
	}
	rows, err := h.Store.ListWakeLog(rec.ID, 10)
	if err != nil || len(rows) != 2 || rows[0].PrePingResult != featureheartbeat.PrePingWake || rows[1].PrePingResult != featureheartbeat.PrePingSkip || rows[1].PrePingReason != "inbox_no_match" {
		t.Fatalf("wake log = %#v, %v", rows, err)
	}
}

// Paging must stop at a fixed bound and report it, rather than run
// long or silently ignore older mail. Boundary: the template's page loop
// against an inbox that never returns a short page.
func TestInboxMatchTemplateReportsTheScanLimit(t *testing.T) {
	inbox := &fakeInboxAPI{endless: true}
	api := inbox.serve(t)
	result, audit := runInboxMatchTemplate(t, api.URL, `{"senderHostedThreadId":"nht_sender"}`)
	if result.Decision != featureheartbeat.PrePingFail || audit.Reason != "inbox_scan_limit" || inbox.requests != 50 {
		t.Fatalf("endless inbox: result=%#v audit=%#v requests=%d", result, audit, inbox.requests)
	}
}

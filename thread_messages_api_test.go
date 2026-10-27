package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/history"
)

// fakeMessagesCatalog serves transcript pages keyed by backward cursor, so a
// test can exercise the tail walk over more than one page.
type fakeMessagesCatalog struct {
	fakeThreadCatalog
	pages     map[string]transcriptResponse
	queries   []transcriptListQuery
	threadID  string
	resolved  string
	resolve   func(string) (string, error)
	err       error
	bodies    map[string]history.TranscriptRetainedBody
	bodyLoads []string
	bodyErr   error
}

func (f *fakeMessagesCatalog) LoadTranscriptBody(_ context.Context, _, itemID string) (history.TranscriptRetainedBody, error) {
	f.bodyLoads = append(f.bodyLoads, itemID)
	if f.bodyErr != nil {
		return history.TranscriptRetainedBody{}, f.bodyErr
	}
	if body, ok := f.bodies[itemID]; ok {
		return body, nil
	}
	return history.TranscriptRetainedBody{}, history.ErrTranscriptBodyUnavailable
}

func TestThreadMessagesPrefersNewestBodiesAndKeepsUnavailablePreview(t *testing.T) {
	page := transcriptResponse{ThreadID: "nth_one", Items: []transcriptItem{
		{ID: "older", Type: "agent_message", Text: "older preview", ContentRef: "old", OriginalBytes: 150 << 10},
		{ID: "newer", Type: "agent_message", Text: "newer preview", ContentRef: "new", OriginalBytes: 150 << 10},
	}}
	fake := &fakeMessagesCatalog{pages: map[string]transcriptResponse{"": page}, bodies: map[string]history.TranscriptRetainedBody{
		"newer": {ItemID: "newer", Text: strings.Repeat("n", 150<<10), StoredBytes: 150 << 10},
	}}
	messages, err := collectThreadMessages(context.Background(), fake, "nth_one", threadMessagesQuery{Tail: 2, Roles: map[string]bool{"agent": true}})
	if err != nil || len(messages) != 2 || messages[1].Complete != true || messages[0].Complete != false || strings.Join(fake.bodyLoads, ",") != "newer" {
		t.Fatalf("messages=%#v loads=%v err=%v", messages, fake.bodyLoads, err)
	}
}

func (f *fakeMessagesCatalog) LoadTranscript(_ context.Context, id string, query transcriptListQuery) (transcriptResponse, error) {
	f.threadID = id
	f.queries = append(f.queries, query)
	return f.pages[query.Before], f.err
}

func (f *fakeMessagesCatalog) ResolveThreadReference(_ context.Context, reference string) (string, error) {
	f.resolved = reference
	if f.resolve == nil {
		return reference, nil
	}
	return f.resolve(reference)
}

func stamp(value string) *string { return &value }

// realShapedTranscript is a transcript page the way the projection actually
// emits one: user and agent messages interleaved with reasoning, a tool call
// and its result, and a subagent aside.
func realShapedTranscript() transcriptResponse {
	items := []transcriptItem{
		{ID: "nit_1", ProjectedSeq: 1, TurnID: "ntr_1", Type: "user_message", Category: "messages",
			Role: "user", Text: "please investigate the failing deploy", CreatedAt: stamp("2026-07-31T12:00:00Z")},
		{ID: "nit_2", ProjectedSeq: 2, TurnID: "ntr_1", Type: "reasoning", Category: "reasoning",
			Text: "considering the bundle hash", CreatedAt: stamp("2026-07-31T12:00:05Z")},
		{ID: "nit_3", ProjectedSeq: 3, TurnID: "ntr_1", Type: "tool_call", Category: "toolCall",
			ToolName: "Bash", Text: "lsof -nP -iTCP:7690", CreatedAt: stamp("2026-07-31T12:00:06Z")},
		{ID: "nit_4", ProjectedSeq: 4, TurnID: "ntr_1", Type: "tool_result", Category: "toolResult",
			Text: "nanod 4821", CreatedAt: stamp("2026-07-31T12:00:07Z")},
		{ID: "nit_5", ProjectedSeq: 5, TurnID: "ntr_1", Type: "agent_message", Category: "subagent",
			Role: "assistant", Sidechain: true, Text: "subagent aside", CreatedAt: stamp("2026-07-31T12:00:08Z")},
		{ID: "nit_6", ProjectedSeq: 6, TurnID: "ntr_1", Type: "agent_message", Category: "messages",
			Role: "assistant", Text: "The listener is stale.\n", IsFinalAnswer: true, CreatedAt: stamp("2026-07-31T12:00:09Z")},
		{ID: "nit_7", ProjectedSeq: 7, TurnID: "ntr_2", Type: "user_message", Category: "messages",
			Role: "user", Text: "restart it", CreatedAt: stamp("2026-07-31T12:01:00Z")},
		{ID: "nit_8", ProjectedSeq: 8, TurnID: "ntr_2", Type: "agent_message", Category: "messages",
			Role: "assistant", Text: "Restarted. Want me to verify the hash?", IsFinalAnswer: true,
			CreatedAt: stamp("2026-07-31T12:01:30Z")},
	}
	return transcriptResponse{
		ThreadID: "nth_one", View: defaultThreadTranscriptView, Items: items,
		Turns: []transcriptTurn{{ID: "ntr_1", Ordinal: 1, StartSeq: 1, EndSeq: 6}, {ID: "ntr_2", Ordinal: 2, StartSeq: 7, EndSeq: 8}},
		Page:  transcriptPageInfo{StartSeq: 1, EndSeq: 8, LatestSeq: 8, SnapshotHeadSeq: 8},
		Projection: transcriptProjection{Key: "ntp_one", State: "ready", SourceStatus: "present",
			EvidenceAvailable: true, ItemCount: 8},
	}
}

func messagesRequest(t *testing.T, s *server, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	mux := http.NewServeMux()
	mountRoutes(mux, applicationRoutes(s))
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

// TestThreadMessagesTextIsChronologicalUserAndAgentOnly is the endpoint's whole
// promise: plain text, in order, carrying only what the user and agent said.
func TestThreadMessagesTextIsChronologicalUserAndAgentOnly(t *testing.T) {
	fake := &fakeMessagesCatalog{pages: map[string]transcriptResponse{"": realShapedTranscript()}}
	recorder := messagesRequest(t, &server{catalog: fake}, "/api/v1/threads/nth_one/messages")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	body := recorder.Body.String()
	want := strings.Join([]string{
		"user [2026-07-31T12:00:00Z]\nplease investigate the failing deploy",
		"agent [2026-07-31T12:00:08Z]\nsubagent aside",
		"agent [2026-07-31T12:00:09Z]\nThe listener is stale.",
		"user [2026-07-31T12:01:00Z]\nrestart it",
		"agent [2026-07-31T12:01:30Z]\nRestarted. Want me to verify the hash?",
	}, "\n\n") + "\n\n"
	if body != want {
		t.Fatalf("text body =\n%s\nwant\n%s", body, want)
	}
	for _, forbidden := range []string{"considering the bundle hash", "lsof -nP", "nanod 4821",
		`"role"`, "projectedSeq", "{"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("text body leaked %q:\n%s", forbidden, body)
		}
	}
	// The transcript is asked for the message-shaped categories only.
	if len(fake.queries) != 1 || fake.threadID != "nth_one" ||
		strings.Join(fake.queries[0].HiddenCategories, ",") != "reasoning,toolCall,toolResult,system" ||
		!fake.queries[0].HiddenSpecified || fake.queries[0].View != defaultThreadTranscriptView {
		t.Fatalf("transcript query = %#v", fake.queries)
	}
}

func TestThreadMessagesTailRolesAndJSONFormat(t *testing.T) {
	fake := &fakeMessagesCatalog{pages: map[string]transcriptResponse{"": realShapedTranscript()}}
	s := &server{catalog: fake}

	recorder := messagesRequest(t, s, "/api/v1/threads/nth_one/messages?tail=2&format=json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response threadMessagesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ThreadID != "nth_one" || response.Count != 2 || len(response.Messages) != 2 {
		t.Fatalf("json response = %#v", response)
	}
	if response.Messages[0].Role != threadMessageRoleUser || response.Messages[0].Text != "restart it" ||
		response.Messages[1].Role != threadMessageRoleAgent {
		t.Fatalf("tail = %#v", response.Messages)
	}

	recorder = messagesRequest(t, s, "/api/v1/threads/nth_one/messages?roles=user&format=json")
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Count != 2 {
		t.Fatalf("user-only tail = %#v", response.Messages)
	}
	for _, message := range response.Messages {
		if message.Role != threadMessageRoleUser {
			t.Fatalf("role filter leaked %q", message.Role)
		}
	}
}

func TestThreadMessagesUsesFittingRetainedBodyAndLabelsIncompletePreview(t *testing.T) {
	page := realShapedTranscript()
	page.Items = []transcriptItem{
		{ID: "nit_small", Type: "user_message", Text: "small", ContentRef: "ref-small", OriginalBytes: 20},
		{ID: "nit_large", Type: "agent_message", Text: "preview", ContentRef: "ref-large", OriginalBytes: maxThreadTranscriptBytes + 1, ContentTruncated: true},
	}
	fake := &fakeMessagesCatalog{pages: map[string]transcriptResponse{"": page}, bodies: map[string]history.TranscriptRetainedBody{
		"nit_small": {ItemID: "nit_small", ContentRef: "ref-small", Text: "canonical retained text", OriginalBytes: 20, StoredBytes: 20},
	}}
	rec := messagesRequest(t, &server{catalog: fake}, "/api/v1/threads/nth_one/messages?format=json")
	var response threadMessagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Messages[0].Complete || response.Messages[0].Text != "canonical retained text" {
		t.Fatalf("fitting body = %#v", response.Messages[0])
	}
	if response.Messages[1].Complete || response.Messages[1].ContentRef != "ref-large" || response.Messages[1].RetainedTruncated {
		t.Fatalf("preview metadata = %#v", response.Messages[1])
	}
	if strings.Join(fake.bodyLoads, ",") != "nit_small" {
		t.Fatalf("known over-budget body was needlessly loaded: %v", fake.bodyLoads)
	}
	rec = messagesRequest(t, &server{catalog: fake}, "/api/v1/threads/nth_one/messages")
	if !strings.Contains(rec.Body.String(), "[inline_preview incomplete item=nit_large contentRef=ref-large") {
		t.Fatalf("silent preview: %s", rec.Body.String())
	}
}

func TestThreadMessagesResponseStaysBoundedWhenBodiesFitIndividually(t *testing.T) {
	items := make([]transcriptItem, 0, 4)
	bodies := map[string]history.TranscriptRetainedBody{}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("nit_%d", i)
		items = append(items, transcriptItem{ID: id, Type: "agent_message", Text: "preview", ContentRef: "ref" + id, OriginalBytes: 100000})
		bodies[id] = history.TranscriptRetainedBody{ItemID: id, ContentRef: "ref" + id, Text: strings.Repeat("x", 100000), StoredBytes: 100000}
	}
	fake := &fakeMessagesCatalog{pages: map[string]transcriptResponse{"": {ThreadID: "nth_one", Items: items}}, bodies: bodies}
	rec := messagesRequest(t, &server{catalog: fake}, "/api/v1/threads/nth_one/messages?format=json")
	if rec.Code != http.StatusOK || rec.Body.Len() > maxThreadTranscriptBytes {
		t.Fatalf("bounded response = %d bytes=%d", rec.Code, rec.Body.Len())
	}
	var response threadMessagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 4 || response.Messages[0].Complete || response.Messages[1].Complete ||
		!response.Messages[2].Complete || !response.Messages[3].Complete {
		t.Fatalf("newest fitting bodies were not preferred: %#v", response.Messages)
	}
	if got := strings.Join(fake.bodyLoads, ","); got != "nit_3,nit_2" {
		t.Fatalf("body load order=%q; want newest fitting messages only", got)
	}
}

func TestThreadMessagesLoadsASingleOrdinaryHundredKiBBody(t *testing.T) {
	const bodyBytes = 100 << 10
	fake := &fakeMessagesCatalog{pages: map[string]transcriptResponse{"": {
		ThreadID: "nth_one", Items: []transcriptItem{{ID: "ordinary", Type: "agent_message", Text: "preview", ContentRef: "ordinary-body", OriginalBytes: bodyBytes}},
	}}, bodies: map[string]history.TranscriptRetainedBody{
		"ordinary": {ItemID: "ordinary", ContentRef: "ordinary-body", Text: strings.Repeat("a", bodyBytes), StoredBytes: bodyBytes, OriginalBytes: bodyBytes},
	}}
	recorder := messagesRequest(t, &server{catalog: fake}, "/api/v1/threads/nth_one/messages?format=json")
	var response threadMessagesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Body.Len() > maxThreadTranscriptBytes || len(response.Messages) != 1 || !response.Messages[0].Complete || len(response.Messages[0].Text) != bodyBytes {
		t.Fatalf("ordinary retained body was not complete: bytes=%d message=%#v", recorder.Body.Len(), response.Messages)
	}
	if got := strings.Join(fake.bodyLoads, ","); got != "ordinary" {
		t.Fatalf("body loads=%q", got)
	}
}

func TestThreadMessagesFallsBackWhenActualJSONEscapingDoesNotFit(t *testing.T) {
	const originalBytes = 200 << 10
	fake := &fakeMessagesCatalog{pages: map[string]transcriptResponse{"": {
		ThreadID: "nth_one", Items: []transcriptItem{{ID: "escaped-body", Type: "user_message", Text: "preview", ContentRef: "escaped-ref", OriginalBytes: originalBytes}},
	}}, bodies: map[string]history.TranscriptRetainedBody{
		"escaped-body": {ItemID: "escaped-body", ContentRef: "escaped-ref", Text: strings.Repeat("\\\"\nX", originalBytes/4), StoredBytes: originalBytes, OriginalBytes: originalBytes},
	}}
	recorder := messagesRequest(t, &server{catalog: fake}, "/api/v1/threads/nth_one/messages?format=json")
	var response threadMessagesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	message := response.Messages[0]
	if recorder.Body.Len() > maxThreadTranscriptBytes || message.Complete || message.ContentState != "inline_preview" || message.Text != "preview" {
		t.Fatalf("escaped body did not fall back honestly: bytes=%d message=%#v", recorder.Body.Len(), message)
	}
	if got := strings.Join(fake.bodyLoads, ","); got != "escaped-body" {
		t.Fatalf("escaped body was not checked: %q", got)
	}
}

func TestThreadMessagesRetentionStatesAndWireBounds(t *testing.T) {
	items := []transcriptItem{
		{ID: "small", Type: "user_message", Text: "small"},
		{ID: "threehundred", Type: "agent_message", Text: "preview", ContentRef: "r300", OriginalBytes: 300 << 10},
		{ID: "meg", Type: "agent_message", Text: "preview", ContentRef: "rmeg", OriginalBytes: history.MaxTranscriptBodyBytes + 1},
	}
	fake := &fakeMessagesCatalog{pages: map[string]transcriptResponse{"": {ThreadID: "nth_one", Items: items}}}
	for _, format := range []string{"json", "text"} {
		rec := messagesRequest(t, &server{catalog: fake}, "/api/v1/threads/nth_one/messages?format="+format)
		if rec.Code != http.StatusOK || rec.Body.Len() > maxThreadTranscriptBytes {
			t.Fatalf("%s bound=%d status=%d", format, rec.Body.Len(), rec.Code)
		}
		if format == "json" {
			var response threadMessagesResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if !response.Messages[0].Complete || response.Messages[1].RetainedTruncated || !response.Messages[2].RetainedTruncated {
				t.Fatalf("states=%#v", response.Messages)
			}
		}
	}
	escaped := &fakeMessagesCatalog{pages: map[string]transcriptResponse{"": {ThreadID: "nth_one", Items: []transcriptItem{{ID: "escaped", Type: "agent_message", Text: strings.Repeat("\\\"\n", maxThreadTranscriptBytes/3)}}}}}
	if rec := messagesRequest(t, &server{catalog: escaped}, "/api/v1/threads/nth_one/messages?format=json"); rec.Body.Len() > maxThreadTranscriptBytes {
		t.Fatalf("escaped json=%d", rec.Body.Len())
	}
}

func TestThreadMessagesKeepsAnUnavailableSmallBodyAsAnHonestPreview(t *testing.T) {
	fake := &fakeMessagesCatalog{pages: map[string]transcriptResponse{"": {
		ThreadID: "nth_one", Items: []transcriptItem{{ID: "missing", Type: "user_message", Text: "inline preview", ContentRef: "body-missing", OriginalBytes: 42}},
	}}}
	recorder := messagesRequest(t, &server{catalog: fake}, "/api/v1/threads/nth_one/messages?format=json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response threadMessagesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	message := response.Messages[0]
	if message.Complete || message.ContentState != "inline_preview" || message.RetainedTruncated || message.Text != "inline preview" {
		t.Fatalf("unavailable body must remain an honest preview: %#v", message)
	}
	if strings.Join(fake.bodyLoads, ",") != "missing" {
		t.Fatalf("under-budget unavailable body was not attempted: %v", fake.bodyLoads)
	}
}

func TestThreadMessagesKeepsOtherBodyLookupFailuresAsPreviews(t *testing.T) {
	fake := &fakeMessagesCatalog{
		pages:   map[string]transcriptResponse{"": {ThreadID: "nth_one", Items: []transcriptItem{{ID: "transient", Type: "user_message", Text: "preview", ContentRef: "body-transient", OriginalBytes: 42}}}},
		bodyErr: errors.New("temporary body read failure"),
	}
	recorder := messagesRequest(t, &server{catalog: fake}, "/api/v1/threads/nth_one/messages?format=json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("lookup failure changed tail status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response threadMessagesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if message := response.Messages[0]; message.Complete || message.ContentState != "inline_preview" || message.Text != "preview" {
		t.Fatalf("transient body error leaked into tail semantics: %#v", message)
	}
}

func TestThreadMessagesMarksALoadedRetainedPrefixIncomplete(t *testing.T) {
	fake := &fakeMessagesCatalog{pages: map[string]transcriptResponse{"": {
		ThreadID: "nth_one", Items: []transcriptItem{{ID: "prefix", Type: "agent_message", Text: "preview", ContentRef: "body-prefix", OriginalBytes: 100}},
	}}, bodies: map[string]history.TranscriptRetainedBody{
		"prefix": {ItemID: "prefix", ContentRef: "body-prefix", Text: "retained prefix", Truncated: true, StoredBytes: 100, OriginalBytes: history.MaxTranscriptBodyBytes + 10},
	}}
	messages, err := collectThreadMessages(context.Background(), fake, "nth_one", threadMessagesQuery{Tail: 1, Roles: map[string]bool{threadMessageRoleAgent: true}})
	if err != nil {
		t.Fatal(err)
	}
	message := messages[0]
	if message.Complete || !message.RetainedTruncated || message.ContentState != "retained_truncated_prefix" || message.Text != "retained prefix" {
		t.Fatalf("retained prefix semantics=%#v", message)
	}
	if text := renderThreadMessagesText(messages); !strings.Contains(text, "[retained_truncated_prefix incomplete") || !strings.Contains(text, "retainedTruncated=true") {
		t.Fatalf("retained prefix marker=%q", text)
	}
}

// TestThreadMessagesWalksBackwardForAShortPage: a page dominated by tool
// traffic does not shortchange the tail — the walk follows the backward cursor
// the transcript already mints.
func TestThreadMessagesWalksBackwardForAShortPage(t *testing.T) {
	cursor := "opaque-cursor"
	head := transcriptResponse{
		ThreadID: "nth_one", View: defaultThreadTranscriptView,
		Items: []transcriptItem{
			{ID: "nit_9", ProjectedSeq: 9, Type: "tool_call", Category: "toolCall", Text: "grep"},
			{ID: "nit_10", ProjectedSeq: 10, Type: "agent_message", Category: "messages", Text: "newest"},
		},
		Page: transcriptPageInfo{StartSeq: 9, EndSeq: 10, LatestSeq: 10, SnapshotHeadSeq: 10,
			HasMoreBefore: true, NextBeforeCursor: &cursor},
		Projection: transcriptProjection{Key: "ntp_one", State: "ready", SourceStatus: "present"},
	}
	older := transcriptResponse{
		ThreadID: "nth_one", View: defaultThreadTranscriptView,
		Items: []transcriptItem{
			{ID: "nit_7", ProjectedSeq: 7, Type: "user_message", Category: "messages", Text: "older"},
		},
		Page:       transcriptPageInfo{StartSeq: 7, EndSeq: 8, LatestSeq: 10, SnapshotHeadSeq: 10},
		Projection: transcriptProjection{Key: "ntp_one", State: "ready", SourceStatus: "present"},
	}
	fake := &fakeMessagesCatalog{pages: map[string]transcriptResponse{"": head, cursor: older}}
	recorder := messagesRequest(t, &server{catalog: fake}, "/api/v1/threads/nth_one/messages?tail=2")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != "user\nolder\n\nagent\nnewest\n\n" {
		t.Fatalf("walked body = %q", body)
	}
	if len(fake.queries) != 2 || fake.queries[1].Before != cursor {
		t.Fatalf("backward walk = %#v", fake.queries)
	}
	// The walk is bounded: a thread that never fills the tail stops paging.
	endless := transcriptResponse{
		ThreadID: "nth_one", View: defaultThreadTranscriptView,
		Items: []transcriptItem{{ID: "nit_x", ProjectedSeq: 1, Type: "tool_call", Category: "toolCall", Text: "grep"}},
		Page: transcriptPageInfo{StartSeq: 1, EndSeq: 1, LatestSeq: 1, SnapshotHeadSeq: 1,
			HasMoreBefore: true, NextBeforeCursor: &cursor},
		Projection: transcriptProjection{Key: "ntp_one", State: "ready", SourceStatus: "present"},
	}
	bounded := &fakeMessagesCatalog{pages: map[string]transcriptResponse{"": endless, cursor: endless}}
	if recorder := messagesRequest(t, &server{catalog: bounded}, "/api/v1/threads/nth_one/messages"); recorder.Body.Len() != 0 {
		t.Fatalf("bounded walk body = %q", recorder.Body.String())
	}
	if len(bounded.queries) != maxThreadMessagePages {
		t.Fatalf("page walk = %d, want at most %d", len(bounded.queries), maxThreadMessagePages)
	}
}

// TestThreadMessagesResolvesReferences: an unambiguous prefix resolves to the
// durable id, an ambiguous one reports every candidate rather than guessing.
func TestThreadMessagesResolvesReferences(t *testing.T) {
	fake := &fakeMessagesCatalog{
		pages:   map[string]transcriptResponse{"": realShapedTranscript()},
		resolve: func(string) (string, error) { return "nth_one", nil },
	}
	recorder := messagesRequest(t, &server{catalog: fake}, "/api/v1/threads/nth_o/messages")
	if recorder.Code != http.StatusOK || fake.resolved != "nth_o" || fake.threadID != "nth_one" {
		t.Fatalf("prefix resolution = %d, %q => %q", recorder.Code, fake.resolved, fake.threadID)
	}

	candidates := []string{"nth_alpha", "nth_alpine"}
	ambiguous := &fakeMessagesCatalog{
		pages:   map[string]transcriptResponse{"": realShapedTranscript()},
		resolve: func(string) (string, error) { return "", &ambiguousThreadReferenceError{Candidates: candidates} },
	}
	recorder = messagesRequest(t, &server{catalog: ambiguous}, "/api/v1/threads/nth_al/messages")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous status = %d", recorder.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Candidates []string `json:"candidates"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "ambiguous_thread_id" || strings.Join(body.Candidates, ",") != strings.Join(candidates, ",") {
		t.Fatalf("ambiguous body = %#v", body)
	}
	if ambiguous.threadID != "" {
		t.Fatalf("an ambiguous reference must never load a transcript: %q", ambiguous.threadID)
	}

	missing := &fakeMessagesCatalog{resolve: func(string) (string, error) { return "", errCatalogThreadNotFound }}
	recorder = messagesRequest(t, &server{catalog: missing}, "/api/v1/threads/nowhere/messages")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d", recorder.Code)
	}
	assertCatalogErrorCode(t, recorder, "thread_not_found")
}

func TestThreadMessagesRejectsInvalidQueryAndSurfacesBackendErrors(t *testing.T) {
	s := &server{catalog: &fakeMessagesCatalog{pages: map[string]transcriptResponse{"": realShapedTranscript()}}}
	for _, target := range []string{
		"/api/v1/threads/nth_one/messages?tail=0",
		"/api/v1/threads/nth_one/messages?tail=lots",
		"/api/v1/threads/nth_one/messages?roles=system",
		"/api/v1/threads/nth_one/messages?format=yaml",
	} {
		recorder := messagesRequest(t, s, target)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d", target, recorder.Code)
		}
		assertCatalogErrorCode(t, recorder, "invalid_query")
	}

	// tail is clamped rather than rejected, so an eager caller still gets an answer.
	recorder := messagesRequest(t, s, fmt.Sprintf("/api/v1/threads/nth_one/messages?tail=%d&format=json", maxThreadMessagesTail*10))
	if recorder.Code != http.StatusOK {
		t.Fatalf("clamped tail status = %d", recorder.Code)
	}
	if got := s.catalog.(*fakeMessagesCatalog).queries[0].Limit; got != maxThreadMessagesTail {
		t.Fatalf("clamped limit = %d", got)
	}

	failing := &server{catalog: &fakeMessagesCatalog{err: errors.New("private sqlite detail")}}
	recorder = messagesRequest(t, failing, "/api/v1/threads/nth_one/messages")
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), "private sqlite detail") {
		t.Fatalf("backend error = %d %q", recorder.Code, recorder.Body.String())
	}
	assertCatalogErrorCode(t, recorder, "transcript_error")

	recorder = httptest.NewRecorder()
	(&server{}).handleThreadMessages(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_one/messages", nil), "nth_one")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status = %d", recorder.Code)
	}
	assertCatalogErrorCode(t, recorder, "catalog_unavailable")

	recorder = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/threads/nth_one/messages", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", recorder.Code)
	}
}

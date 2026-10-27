package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func operatorSendForTest(t *testing.T, s *server, recipient agentSessionRecord, clientID, subject, body string) (*httptest.ResponseRecorder, agentMessageView) {
	t.Helper()
	encoded, err := json.Marshal(agentMessageSendRequest{ClientMessageID: clientID, Operator: true, Subject: subject, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleHostedThreadMessageSend(rec, httptest.NewRequest(http.MethodPost, "/messages", bytes.NewReader(encoded)), recipient)
	var message agentMessageView
	if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &message); err != nil {
			t.Fatalf("decode message: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, message
}

func TestOperatorSendIdentityAndExclusivity(t *testing.T) {
	s, _, _, sender, recipient, _ := stage0Server(t)

	rec, message := operatorSendForTest(t, s, recipient, "op-1", "From Operator", "Please prioritize the parser.")
	if rec.Code != http.StatusCreated {
		t.Fatalf("operator send = %d %s", rec.Code, rec.Body.String())
	}
	if message.Sender.Kind != operatorSenderKind || message.Sender.Name != operatorSenderName || message.Sender.HostedThreadID != "" {
		t.Fatalf("operator sender = %+v", message.Sender)
	}
	if message.Origin != operatorSenderKind || message.Status != agentMessageUnread {
		t.Fatalf("operator message = %+v", message)
	}

	// Exactly one identity per send: both and neither are rejected.
	both, _ := json.Marshal(agentMessageSendRequest{ClientMessageID: "op-2", Operator: true, SenderThreadRef: sender.ID, Body: "x"})
	rec = httptest.NewRecorder()
	s.handleHostedThreadMessageSend(rec, httptest.NewRequest(http.MethodPost, "/messages", bytes.NewReader(both)), recipient)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sender_required") {
		t.Fatalf("both identities = %d %s", rec.Code, rec.Body.String())
	}
	neither, _ := json.Marshal(agentMessageSendRequest{ClientMessageID: "op-3", Body: "x"})
	rec = httptest.NewRecorder()
	s.handleHostedThreadMessageSend(rec, httptest.NewRequest(http.MethodPost, "/messages", bytes.NewReader(neither)), recipient)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sender_required") {
		t.Fatalf("no identity = %d %s", rec.Code, rec.Body.String())
	}

	// A disposition of operator mail must not synthesize sender feedback —
	// the operator watches status in the UI and has no hosted inbox.
	_, feedback, err := s.mail.DispositionMessage(message.MessageID, recipient.ID, "Worker", recipient.NativeThreadID, agentMessageActed, "", time.Now())
	if err != nil || feedback != nil {
		t.Fatalf("operator disposition feedback = %+v err=%v", feedback, err)
	}
}

func TestOperatorEditOnlyWhileUnread(t *testing.T) {
	s, _, _, sender, recipient, _ := stage0Server(t)
	_, message := operatorSendForTest(t, s, recipient, "op-edit", "Draft", "First wording.")

	edit := func(id, subject, body string) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(map[string]string{"subject": subject, "body": body})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/messages/"+id+"/edit", bytes.NewReader(payload))
		applicationHandler(s).ServeHTTP(rec, req)
		return rec
	}

	rec := edit(message.MessageID, "Final", "Second wording.")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Second wording.") {
		t.Fatalf("edit while unread = %d %s", rec.Code, rec.Body.String())
	}
	detail, err := s.mail.MessageDetail(message.MessageID)
	if err != nil || detail.Message.Subject != "Final" || detail.Message.Body != "Second wording." {
		t.Fatalf("edited message = %+v err=%v", detail.Message, err)
	}
	var edited *agentMessageEventView
	for i := range detail.Events {
		if detail.Events[i].Kind == "edited" {
			edited = &detail.Events[i]
		}
	}
	if edited == nil || !strings.Contains(string(edited.Detail), "First wording.") {
		t.Fatalf("edit event missing previous content: %+v", detail.Events)
	}

	// Once read, the message freezes.
	if _, _, err := s.mail.ListInbox(recipient.ID, agentMessageListOptions{Limit: 10}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec = edit(message.MessageID, "Too late", "Rewrite after read.")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "message_already_read") {
		t.Fatalf("edit after read = %d %s", rec.Code, rec.Body.String())
	}

	// Agent-authored mail is never editable.
	_, agentMessage := sendAgentMessageForTest(t, s, recipient, sender.ID, "agent-1", "Agent", "Agent words", nil)
	rec = edit(agentMessage.MessageID, "Nope", "Operator rewrite of agent mail.")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_editable") {
		t.Fatalf("edit agent mail = %d %s", rec.Code, rec.Body.String())
	}
}

func TestAgentMailCountsAndCatalogOverlay(t *testing.T) {
	s, _, _, sender, recipient, _ := stage0Server(t)
	sendAgentMessageForTest(t, s, recipient, sender.ID, "m-1", "One", "First", nil)
	sendAgentMessageForTest(t, s, recipient, sender.ID, "m-2", "Two", "Second", nil)
	operatorSendForTest(t, s, recipient, "m-3", "Three", "Third")

	// Read marks all three seen; disposition one so counts split across states.
	items, marked, err := s.mail.ListInbox(recipient.ID, agentMessageListOptions{Limit: 10}, true, time.Now())
	if err != nil || marked != 3 {
		t.Fatalf("mark seen = %d err=%v", marked, err)
	}
	if _, _, err := s.mail.DispositionMessage(items[0].MessageID, recipient.ID, "Worker", recipient.NativeThreadID, agentMessageActed, "", time.Now()); err != nil {
		t.Fatal(err)
	}

	counts, err := s.mail.CountsFor([]string{recipient.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := counts[recipient.ID]; got.Total != 3 || got.Unread != 0 || got.Seen != 2 {
		t.Fatalf("recipient counts = %+v", got)
	}
	// Disposition feedback defaults off, so routine mail stays quiet.
	all, err := s.mail.CountsFor(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := all[sender.ID]; got.Total != 0 || got.Unread != 0 {
		t.Fatalf("sender unexpectedly received disposition feedback: %+v", got)
	}

	rows := []catalogThread{{ID: "nth_x", HostedThreadID: recipient.ID}, {ID: "nth_y"}}
	s.attachMailOverlay(rows)
	if rows[0].Mail == nil || rows[0].Mail.Total != 3 || rows[0].Mail.Seen != 2 {
		t.Fatalf("mail overlay = %+v", rows[0].Mail)
	}
	if rows[1].Mail != nil {
		t.Fatalf("row without hosted identity gained mail: %+v", rows[1].Mail)
	}
}

func TestGlobalMailListingOrderFiltersAndPaging(t *testing.T) {
	s, _, _, sender, recipient, _ := stage0Server(t)
	sendAgentMessageForTest(t, s, recipient, sender.ID, "g-1", "One", "First", nil)
	sendAgentMessageForTest(t, s, recipient, sender.ID, "g-2", "Two", "Second", nil)
	operatorSendForTest(t, s, recipient, "g-3", "Three", "Third")

	list := func(query string) map[string]json.RawMessage {
		rec := httptest.NewRecorder()
		s.handleAgentMessagesGlobal(rec, httptest.NewRequest(http.MethodGet, "/api/v1/agent-messages"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("global list %q = %d %s", query, rec.Code, rec.Body.String())
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	decodeItems := func(raw json.RawMessage) []agentMessageView {
		var items []agentMessageView
		if err := json.Unmarshal(raw, &items); err != nil {
			t.Fatal(err)
		}
		return items
	}

	payload := list("")
	items := decodeItems(payload["items"])
	if len(items) != 3 || items[0].Subject != "Three" || items[2].Subject != "One" {
		t.Fatalf("global order = %+v", items)
	}
	var summary agentMailCounts
	if err := json.Unmarshal(payload["summary"], &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Total != 3 || summary.Unread != 3 {
		t.Fatalf("summary = %+v", summary)
	}

	// Page backward from the newest two.
	payload = list("?limit=2")
	items = decodeItems(payload["items"])
	if len(items) != 2 || items[0].Subject != "Three" {
		t.Fatalf("page one = %+v", items)
	}
	var nextBefore int64
	if err := json.Unmarshal(payload["nextBefore"], &nextBefore); err != nil || nextBefore <= 0 {
		t.Fatalf("nextBefore = %d err=%v", nextBefore, err)
	}
	payload = list("?limit=2&before=" + jsonNumber(nextBefore))
	items = decodeItems(payload["items"])
	if len(items) != 1 || items[0].Subject != "One" {
		t.Fatalf("page two = %+v", items)
	}

	// Status filter narrows without marking anything seen.
	payload = list("?status=unread")
	if got := decodeItems(payload["items"]); len(got) != 3 {
		t.Fatalf("unread filter = %+v", got)
	}
	counts, err := s.mail.CountsFor([]string{recipient.ID})
	if err != nil || counts[recipient.ID].Unread != 3 {
		t.Fatalf("global listing marked mail seen: %+v err=%v", counts, err)
	}
}

func jsonNumber(v int64) string {
	encoded, _ := json.Marshal(v)
	return string(encoded)
}

func TestMailHubNotifiesOnSendReadAndDisposition(t *testing.T) {
	s, _, _, sender, recipient, _ := stage0Server(t)
	s.mailEvents = newMailHub()
	ch := s.mailEvents.subscribe()
	defer s.mailEvents.unsubscribe(ch)

	next := func(stage string) mailDelta {
		select {
		case delta := <-ch:
			return delta
		default:
			t.Fatalf("no mail event after %s", stage)
			return mailDelta{}
		}
	}

	_, message := sendAgentMessageForTest(t, s, recipient, sender.ID, "n-1", "One", "First", nil)
	delta := next("send")
	if delta.Counts[recipient.ID].Unread != 1 || delta.Summary.Unread != 1 {
		t.Fatalf("send delta = %+v", delta)
	}

	rec := httptest.NewRecorder()
	s.handleHostedThreadInbox(rec, httptest.NewRequest(http.MethodGet, "/inbox?actorThreadRef="+recipient.ID, nil), recipient)
	if rec.Code != http.StatusOK {
		t.Fatalf("inbox read = %d", rec.Code)
	}
	delta = next("read")
	if delta.Counts[recipient.ID].Unread != 0 || delta.Counts[recipient.ID].Seen != 1 {
		t.Fatalf("read delta = %+v", delta)
	}

	// A peek must not notify.
	rec = httptest.NewRecorder()
	s.handleHostedThreadInbox(rec, httptest.NewRequest(http.MethodGet, "/inbox?actorThreadRef="+recipient.ID+"&peek=true", nil), recipient)
	select {
	case delta := <-ch:
		t.Fatalf("peek produced a mail event: %+v", delta)
	default:
	}

	// Disposition updates the recipient; feedback stays quiet by default.
	payload, _ := json.Marshal(map[string]string{"actorThreadRef": recipient.ID, "status": "acted"})
	rec = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/messages/"+message.MessageID+"/status", bytes.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("disposition = %d %s", rec.Code, rec.Body.String())
	}
	delta = next("disposition")
	if delta.Counts[recipient.ID].Seen != 0 || delta.Counts[sender.ID].Unread != 0 {
		t.Fatalf("disposition delta = %+v", delta)
	}
}

// TestMailSchemaV16RebuildPreservesMailAndAdmitsOperator downgrades a fresh
// store's mail tables to their v16 shape (narrow CHECK constraints), seeds a
// message, and reopens: the v17 rebuild must preserve the row, its events, its
// refs, and then accept operator mail and edited events the old CHECKs refused.
func TestMailSchemaV16RebuildPreservesMailAndAdmitsOperator(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/control.sqlite"
	store, err := openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO agent_message_seq(recipient_hosted_id,next_seq) VALUES('nht_r',2)`,
		`INSERT INTO agent_messages(message_id,client_message_id,request_hash,sender_key,sender_kind,origin,sender_hosted_id,sender_name,recipient_hosted_id,seq,subject,body,status,created_at,updated_at)
		 VALUES('nmsg_v16','v16-1','hash','nht_s','thread','agent','nht_s','Sender','nht_r',1,'Old','Old mail','unread','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`,
		`INSERT INTO agent_message_refs(message_id,kind,id) VALUES('nmsg_v16','issue.ref','sample-task')`,
		`INSERT INTO agent_message_events(message_id,kind,actor_kind,actor_hosted_id,at) VALUES('nmsg_v16','created','thread','nht_s','2026-09-01T00:00:00Z')`,
	} {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}

	// Downgrade: rebuild both tables with the v16 CHECKs and stamp version 16.
	if _, err := store.db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE agent_messages_v16 (
			message_order INTEGER PRIMARY KEY AUTOINCREMENT, message_id TEXT NOT NULL UNIQUE,
			client_message_id TEXT NOT NULL, request_hash TEXT NOT NULL, sender_key TEXT NOT NULL,
			sender_kind TEXT NOT NULL CHECK(sender_kind IN ('thread','system')),
			origin TEXT NOT NULL CHECK(origin IN ('agent','system')),
			sender_hosted_id TEXT, sender_native_id TEXT, sender_provider TEXT, sender_name TEXT,
			recipient_hosted_id TEXT NOT NULL, seq INTEGER NOT NULL CHECK(seq > 0),
			subject TEXT NOT NULL DEFAULT '', body TEXT NOT NULL, feedback_for_message_id TEXT,
			status TEXT NOT NULL CHECK(status IN ('unread','seen','acted','declined','deferred')),
			status_note TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
			UNIQUE(sender_key,client_message_id), UNIQUE(recipient_hosted_id,seq))`,
		`INSERT INTO agent_messages_v16 SELECT * FROM agent_messages`,
		`DROP TABLE agent_messages`,
		`ALTER TABLE agent_messages_v16 RENAME TO agent_messages`,
		`CREATE TABLE agent_message_events_v16 (
			event_id INTEGER PRIMARY KEY AUTOINCREMENT, message_id TEXT NOT NULL,
			kind TEXT NOT NULL CHECK(kind IN ('created','seen','acted','declined','deferred')),
			actor_kind TEXT NOT NULL CHECK(actor_kind IN ('thread','system')),
			actor_hosted_id TEXT, detail_json TEXT, at TEXT NOT NULL,
			FOREIGN KEY(message_id) REFERENCES agent_messages(message_id) ON DELETE CASCADE)`,
		`INSERT INTO agent_message_events_v16 SELECT * FROM agent_message_events`,
		`DROP TABLE agent_message_events`,
		`ALTER TABLE agent_message_events_v16 RENAME TO agent_message_events`,
		`PRAGMA user_version = 16`,
	} {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatalf("downgrade %q: %v", statement[:24], err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openControlMetadataStore(path)
	if err != nil {
		t.Fatalf("v16→v17 migration failed: %v", err)
	}
	defer reopened.Close()
	var body string
	var refs, events int
	if err := reopened.db.QueryRow(`SELECT body FROM agent_messages WHERE message_id='nmsg_v16'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	_ = reopened.db.QueryRow(`SELECT count(*) FROM agent_message_refs WHERE message_id='nmsg_v16'`).Scan(&refs)
	_ = reopened.db.QueryRow(`SELECT count(*) FROM agent_message_events WHERE message_id='nmsg_v16'`).Scan(&events)
	if body != "Old mail" || refs != 1 || events != 1 {
		t.Fatalf("migrated legacy row body=%q refs=%d events=%d", body, refs, events)
	}
	if _, err := reopened.db.Exec(`INSERT INTO agent_messages(message_id,client_message_id,request_hash,sender_key,sender_kind,origin,sender_name,recipient_hosted_id,seq,subject,body,status,created_at,updated_at)
		VALUES('nmsg_operator','op-after','hash2','operator','operator','operator','Operator','nht_r',2,'New','Operator mail','unread','2026-09-01T00:00:01Z','2026-09-01T00:00:01Z')`); err != nil {
		t.Fatalf("operator row after legacy migration: %v", err)
	}
	if _, err := reopened.db.Exec(`INSERT INTO agent_message_events(message_id,kind,actor_kind,at) VALUES('nmsg_operator','edited','operator','2026-09-01T00:00:02Z')`); err != nil {
		t.Fatalf("edited event after legacy migration: %v", err)
	}
}

package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	state "github.com/dkroshow/nano/internal/app/statestore"
	"github.com/dkroshow/nano/internal/edge/statewiring"
	featuremail "github.com/dkroshow/nano/internal/features/mail"
)

func sendAgentMessageForTest(t *testing.T, s *server, recipient agentSessionRecord, senderID, clientID, subject, body string, refs []featuremail.Ref) (*httptest.ResponseRecorder, featuremail.Message) {
	return sendAgentMessageWithFeedbackForTest(t, s, recipient, senderID, clientID, subject, body, refs, false)
}

func sendAgentMessageWithFeedbackForTest(t *testing.T, s *server, recipient agentSessionRecord, senderID, clientID, subject, body string, refs []featuremail.Ref, dispositionFeedback bool) (*httptest.ResponseRecorder, featuremail.Message) {
	t.Helper()
	encoded, err := json.Marshal(agentMessageSendRequest{ClientMessageID: clientID, SenderThreadRef: senderID, Subject: subject, Body: body, Refs: refs, DispositionFeedback: dispositionFeedback})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleHostedThreadMessageSend(rec, httptest.NewRequest(http.MethodPost, "/messages", bytes.NewReader(encoded)), recipient)
	var message featuremail.Message
	if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &message); err != nil {
			t.Fatalf("decode message: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, message
}

func TestAgentMessageSendOrderingRefsAndDedup(t *testing.T) {
	s, _, _, sender, recipient, _, _ := stage0Server(t)
	refs := []featuremail.Ref{{Kind: "nano.thread", ID: recipient.NativeThreadID}, {Kind: "issue.ref", ID: "sample-task"}}
	rec, first := sendAgentMessageForTest(t, s, recipient, sender.ID, "result-1", "First", "Result one", refs)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first send = %d %s", rec.Code, rec.Body.String())
	}
	if first.Seq != 1 || first.Status != featuremail.Unread || first.Sender.HostedThreadID != sender.ID || first.Recipient.HostedThreadID != recipient.ID {
		t.Fatalf("first message = %+v", first)
	}
	if len(first.Refs) != 2 || first.Refs[0].Kind != "issue.ref" {
		t.Fatalf("normalized refs = %+v", first.Refs)
	}
	rec, replay := sendAgentMessageForTest(t, s, recipient, sender.ID, "result-1", "First", "Result one", []featuremail.Ref{refs[1], refs[0]})
	if rec.Code != http.StatusOK || replay.MessageID != first.MessageID {
		t.Fatalf("replay = %d %+v", rec.Code, replay)
	}
	rec, _ = sendAgentMessageForTest(t, s, recipient, sender.ID, "result-1", "First", "changed", refs)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "client_message_id_conflict") {
		t.Fatalf("conflict = %d %s", rec.Code, rec.Body.String())
	}
	_, second := sendAgentMessageForTest(t, s, recipient, sender.ID, "result-2", "Second", "Result two", nil)
	if second.Seq != 2 {
		t.Fatalf("second sequence = %d", second.Seq)
	}

	outbox, err := s.mail.ListOutbox(sender.ID, featuremail.ListOptions{Limit: 100})
	if err != nil || len(outbox) != 2 || outbox[0].MessageID != first.MessageID || outbox[1].MessageID != second.MessageID {
		t.Fatalf("outbox = %+v, err=%v", outbox, err)
	}
}

func TestAgentInboxPeekSeenLintAndRecipientBoundary(t *testing.T) {
	s, _, _, sender, recipient, _, _ := stage0Server(t)
	_, message := sendAgentMessageForTest(t, s, recipient, sender.ID, "finding-1", "Finding", "Please inspect", []featuremail.Ref{{Kind: "issue.ref", ID: "sample-task"}})

	peek := httptest.NewRecorder()
	s.handleHostedThreadInbox(peek, httptest.NewRequest(http.MethodGet, "/inbox?actorThreadRef="+recipient.ID+"&peek=true", nil), recipient)
	if peek.Code != http.StatusOK || !strings.Contains(peek.Body.String(), `"status":"unread"`) {
		t.Fatalf("peek = %d %s", peek.Code, peek.Body.String())
	}
	wrong := httptest.NewRecorder()
	s.handleHostedThreadInbox(wrong, httptest.NewRequest(http.MethodGet, "/inbox?actorThreadRef="+sender.ID, nil), recipient)
	if wrong.Code != http.StatusForbidden || !strings.Contains(wrong.Body.String(), "not_recipient") {
		t.Fatalf("wrong actor = %d %s", wrong.Code, wrong.Body.String())
	}
	read := httptest.NewRecorder()
	s.handleHostedThreadInbox(read, httptest.NewRequest(http.MethodGet, "/inbox?actorThreadRef="+recipient.ID, nil), recipient)
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"status":"seen"`) {
		t.Fatalf("read = %d %s", read.Code, read.Body.String())
	}
	lint, _, err := s.mail.ListInbox(recipient.ID, featuremail.ListOptions{Limit: 10, Status: []string{featuremail.Seen}}, false, time.Now())
	if err != nil || len(lint) != 1 || lint[0].MessageID != message.MessageID {
		t.Fatalf("seen-undispositioned = %+v, err=%v", lint, err)
	}
	detail, err := s.mail.MessageDetail(message.MessageID)
	if err != nil || len(detail.Events) != 2 || detail.Events[1].Kind != featuremail.Seen {
		t.Fatalf("events = %+v, err=%v", detail.Events, err)
	}
}

func TestAgentMessageDispositionFeedbackAndNoRecursion(t *testing.T) {
	s, _, _, sender, recipient, _, _ := stage0Server(t)
	_, quiet := sendAgentMessageForTest(t, s, recipient, sender.ID, "decision-quiet", "Decision", "No feedback requested", nil)
	if quiet.DispositionFeedback {
		t.Fatal("disposition feedback defaulted on")
	}
	message, feedback, err := s.mail.DispositionMessage(quiet.MessageID, recipient.ID, "Recipient", recipient.NativeThreadID, featuremail.Acted, "Done quietly", time.Now())
	if err != nil || message.Status != featuremail.Acted || feedback != nil {
		t.Fatalf("default feedback = message:%+v feedback:%+v err:%v", message, feedback, err)
	}
	_, original := sendAgentMessageWithFeedbackForTest(t, s, recipient, sender.ID, "decision-1", "Decision", "Choose a path", nil, true)
	if !original.DispositionFeedback {
		t.Fatal("disposition feedback opt-in was not persisted")
	}
	message, deferredFeedback, err := s.mail.DispositionMessage(original.MessageID, recipient.ID, "Recipient", recipient.NativeThreadID, featuremail.Deferred, "Waiting for evidence", time.Now())
	if err != nil || message.Status != featuremail.Deferred || deferredFeedback == nil {
		t.Fatalf("defer = %+v feedback=%+v err=%v", message, deferredFeedback, err)
	}
	if deferredFeedback.Recipient.HostedThreadID != sender.ID || deferredFeedback.Sender.System != "nano" || deferredFeedback.FeedbackForMessageID != original.MessageID {
		t.Fatalf("deferred feedback = %+v", deferredFeedback)
	}
	message, actedFeedback, err := s.mail.DispositionMessage(original.MessageID, recipient.ID, "Recipient", recipient.NativeThreadID, featuremail.Acted, "Implemented", time.Now().Add(time.Second))
	if err != nil || message.Status != featuremail.Acted || actedFeedback == nil || actedFeedback.MessageID == deferredFeedback.MessageID {
		t.Fatalf("acted = %+v feedback=%+v err=%v", message, actedFeedback, err)
	}
	feedbackInbox, _, err := s.mail.ListInbox(sender.ID, featuremail.ListOptions{Limit: 10, Status: []string{featuremail.Unread}}, false, time.Now())
	if err != nil || len(feedbackInbox) != 2 || feedbackInbox[0].Seq >= feedbackInbox[1].Seq {
		t.Fatalf("sender feedback inbox = %+v, err=%v", feedbackInbox, err)
	}
	_, recursive, err := s.mail.DispositionMessage(deferredFeedback.MessageID, sender.ID, "Sender", sender.NativeThreadID, featuremail.Acted, "Noted", time.Now())
	if err != nil || recursive != nil {
		t.Fatalf("system feedback disposition recursively notified: feedback=%+v err=%v", recursive, err)
	}
	feedbackInbox, _, _ = s.mail.ListInbox(sender.ID, featuremail.ListOptions{Limit: 10}, false, time.Now())
	if len(feedbackInbox) != 2 {
		t.Fatalf("feedback disposition created extra mail: %+v", feedbackInbox)
	}
}

func TestAgentMessageDispositionRollsBackWhenFeedbackFails(t *testing.T) {
	s, _, _, sender, recipient, _, _ := stage0Server(t)
	_, original := sendAgentMessageWithFeedbackForTest(t, s, recipient, sender.ID, "rollback-1", "Rollback", "Prove atomicity", nil, true)
	// The failure fires inside the disposition transaction, after the status
	// update, when the sender feedback row is inserted.
	if _, err := s.mail.DB.Exec(`CREATE TRIGGER fail_feedback BEFORE INSERT ON agent_messages
		WHEN NEW.feedback_for_message_id IS NOT NULL BEGIN SELECT RAISE(ABORT, 'injected feedback failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.mail.DispositionMessage(original.MessageID, recipient.ID, "Recipient", recipient.NativeThreadID, featuremail.Acted, "done", time.Now()); err == nil {
		t.Fatal("injected feedback failure succeeded")
	}
	detail, err := s.mail.MessageDetail(original.MessageID)
	if err != nil || detail.Message.Status != featuremail.Unread || len(detail.Events) != 1 {
		t.Fatalf("rolled-back disposition = %+v, err=%v", detail, err)
	}
	inbox, _, err := s.mail.ListInbox(sender.ID, featuremail.ListOptions{Limit: 10}, false, time.Now())
	if err != nil || len(inbox) != 0 {
		t.Fatalf("rolled-back feedback inbox = %+v, err=%v", inbox, err)
	}
}

func TestAgentMessageConcurrentPerRecipientSequence(t *testing.T) {
	mail := newTestAgentMessageStore(t)
	const total = 20
	var wg sync.WaitGroup
	errs := make(chan error, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := featuremail.Create{ClientMessageID: fmt.Sprintf("m-%d", i), SenderKey: "sender", SenderKind: "thread", Origin: "agent", SenderHostedID: "sender", RecipientHostedID: "recipient", Body: fmt.Sprintf("body-%d", i), CreatedAt: time.Now()}
			req.RequestHash = featuremail.RequestHashWithFeedback(req.RecipientHostedID, "", req.Body, nil, false, "")
			_, _, err := mail.CreateMessage(req)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	items, _, err := mail.ListInbox("recipient", featuremail.ListOptions{Limit: 100}, false, time.Now())
	if err != nil || len(items) != total {
		t.Fatalf("items=%d err=%v", len(items), err)
	}
	for i, item := range items {
		if item.Seq != int64(i+1) {
			t.Fatalf("sequence[%d] = %d", i, item.Seq)
		}
	}
}

func TestAgentMessageFailedSendRollsBackSequenceAllocation(t *testing.T) {
	mail := newTestAgentMessageStore(t)
	failed := featuremail.Create{ClientMessageID: "fail-1", SenderKey: "sender", SenderKind: "thread", Origin: "agent", SenderHostedID: "sender", RecipientHostedID: "recipient", Body: "fail", CreatedAt: time.Now()}
	failed.RequestHash = featuremail.RequestHashWithFeedback(failed.RecipientHostedID, "", failed.Body, nil, false, "")
	// The failure fires inside the send transaction, after the recipient's
	// sequence number is allocated.
	if _, err := mail.DB.Exec(`CREATE TRIGGER fail_send BEFORE INSERT ON agent_messages
		WHEN NEW.client_message_id = 'fail-1' BEGIN SELECT RAISE(ABORT, 'injected send failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mail.CreateMessage(failed); err == nil {
		t.Fatal("injected send failure succeeded")
	}
	good := failed
	good.ClientMessageID, good.Body = "good-1", "good"
	good.RequestHash = featuremail.RequestHashWithFeedback(good.RecipientHostedID, "", good.Body, nil, false, "")
	message, created, err := mail.CreateMessage(good)
	if err != nil || !created || message.Seq != 1 {
		t.Fatalf("send after rollback = %+v created=%v err=%v", message, created, err)
	}
}

func TestInboxSendToDetachedAndExitedRecipientCreatesNoTurn(t *testing.T) {
	for _, status := range []string{agentSessionDetached, sessionStatusExited} {
		t.Run(status, func(t *testing.T) {
			s, store, sess, sender, recipient, prompts, _ := stage0Server(t)
			if err := store.UpdateSession(recipient.ID, "", recipient.NativeThreadID, status, ""); err != nil {
				t.Fatal(err)
			}
			recipient, _ = store.Session(recipient.ID)
			rec, message := sendAgentMessageForTest(t, s, recipient, sender.ID, "detached-"+status, "Status", "Stored only", nil)
			if rec.Code != http.StatusCreated || message.Status != featuremail.Unread {
				t.Fatalf("send = %d %+v %s", rec.Code, message, rec.Body.String())
			}
			if got := sess.QueuedPromptCount(); got != 0 {
				t.Fatalf("inbox send queued %d prompt turns", got)
			}
			select {
			case prompt := <-prompts:
				t.Fatalf("inbox send reached provider: %q", prompt)
			case <-time.After(25 * time.Millisecond):
			}
		})
	}
}

func TestControlMetadataV15AgentInboxMigrationPreservesHostedRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sqlite")
	control, err := openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.EnsureHostedThread("nht_preserved", "codex", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TABLE agent_message_events`, `DROP TABLE agent_message_refs`, `DROP TABLE agent_messages`, `DROP TABLE agent_message_seq`, `PRAGMA user_version=15`,
	} {
		if _, err := control.CanonicalDatabase().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	control, err = openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	var provider string
	if err := control.CanonicalDatabase().QueryRow(`SELECT provider FROM hosted_threads WHERE hosted_thread_id='nht_preserved'`).Scan(&provider); err != nil || provider != "codex" {
		t.Fatalf("preserved hosted row provider=%q err=%v", provider, err)
	}
	var version int
	if err := control.CanonicalDatabase().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != controlSchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var mailRows int
	if err := control.CanonicalDatabase().QueryRow(`SELECT count(*) FROM agent_messages`).Scan(&mailRows); err != nil || mailRows != 0 {
		t.Fatalf("legacy mail table rows=%d err=%v", mailRows, err)
	}
}

func TestAgentMailStoreCopiesLegacyRowsOnceAndLeavesSource(t *testing.T) {
	dir := t.TempDir()
	controlPath := filepath.Join(dir, "control.sqlite")
	control, err := openControlMetadataStore(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	statements := []string{
		`INSERT INTO agent_message_seq(recipient_hosted_id,next_seq) VALUES('recipient',2)`,
		`INSERT INTO agent_messages(message_order,message_id,client_message_id,request_hash,sender_key,sender_kind,origin,sender_hosted_id,recipient_hosted_id,seq,subject,body,status,created_at,updated_at)
		 VALUES(7,'nmsg_legacy','legacy-1','hash','sender','thread','agent','sender','recipient',1,'Legacy','Preserve me','unread','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`,
		`INSERT INTO agent_message_events(message_id,kind,actor_kind,actor_hosted_id,at) VALUES('nmsg_legacy','created','thread','sender','2026-09-01T00:00:00Z')`,
		`INSERT INTO agent_message_refs(message_id,kind,id) VALUES('nmsg_legacy','issue.ref','sample-task')`,
	}
	for _, statement := range statements {
		if _, err := control.CanonicalDatabase().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}

	mailPath := filepath.Join(dir, "mail.sqlite")
	mail, report, err := featuremail.OpenStore(mailPath, controlPath)
	if err != nil {
		t.Fatal(err)
	}
	if report != (featuremail.MigrationReport{Messages: 1, Events: 1, Refs: 1, Sequences: 1}) {
		t.Fatalf("migration report = %+v", report)
	}
	detail, err := mail.MessageDetail("nmsg_legacy")
	if err != nil || detail.Message.Body != "Preserve me" || detail.Message.DispositionFeedback || len(detail.Events) != 1 || len(detail.Message.Refs) != 1 {
		t.Fatalf("copied mail = %+v err=%v", detail, err)
	}
	if err := mail.Close(); err != nil {
		t.Fatal(err)
	}
	mail, replay, err := featuremail.OpenStore(mailPath, controlPath)
	if err != nil {
		t.Fatal(err)
	}
	defer mail.Close()
	if replay != report {
		t.Fatalf("replayed migration report = %+v, want %+v", replay, report)
	}
	var sourceMessages int
	if err := control.CanonicalDatabase().QueryRow(`SELECT count(*) FROM agent_messages WHERE message_id='nmsg_legacy'`).Scan(&sourceMessages); err != nil || sourceMessages != 1 {
		t.Fatalf("legacy source rows=%d err=%v", sourceMessages, err)
	}
}

func TestAgentMailRetentionDefaultsOffAndDeletesOnlyExpiredDispositionedMail(t *testing.T) {
	mail, _, err := featuremail.OpenStore(filepath.Join(t.TempDir(), "mail.sqlite"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer mail.Close()
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"expired-acted", "expired-seen", "recent-acted"} {
		request := featuremail.Create{
			ClientMessageID: id, SenderKey: "sender", SenderKind: "thread", Origin: "agent",
			SenderHostedID: "sender", RecipientHostedID: "recipient", Body: id, CreatedAt: now,
			Refs: []featuremail.Ref{{Kind: "test", ID: id}},
		}
		request.RequestHash = featuremail.RequestHashWithFeedback(request.RecipientHostedID, "", request.Body, request.Refs, false, "")
		if _, created, err := mail.CreateMessage(request); err != nil || !created {
			t.Fatalf("create %s: created=%v err=%v", id, created, err)
		}
	}
	old := now.AddDate(0, 0, -31).Format(time.RFC3339Nano)
	recent := now.AddDate(0, 0, -29).Format(time.RFC3339Nano)
	for _, statement := range []string{
		`UPDATE agent_messages SET status='acted',updated_at='` + old + `' WHERE client_message_id='expired-acted'`,
		`UPDATE agent_messages SET status='seen',updated_at='` + old + `' WHERE client_message_id='expired-seen'`,
		`UPDATE agent_messages SET status='acted',updated_at='` + recent + `' WHERE client_message_id='recent-acted'`,
	} {
		if _, err := mail.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if days, enabled, deleted, err := mail.ConfigureRetention(-1, now); err != nil || days != 0 || enabled || deleted != 0 {
		t.Fatalf("default retention = days:%d enabled:%v deleted:%d err:%v", days, enabled, deleted, err)
	}
	days, enabled, deleted, err := mail.ConfigureRetention(30, now)
	if err != nil || days != 30 || !enabled || deleted != 1 {
		t.Fatalf("configured retention = days:%d enabled:%v deleted:%d err:%v", days, enabled, deleted, err)
	}
	var messages, events, expiredRefs int
	if err := mail.DB.QueryRow(`SELECT count(*) FROM agent_messages`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := mail.DB.QueryRow(`SELECT count(*) FROM agent_message_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := mail.DB.QueryRow(`SELECT count(*) FROM agent_message_refs WHERE id='expired-acted'`).Scan(&expiredRefs); err != nil {
		t.Fatal(err)
	}
	if messages != 2 || events != 2 || expiredRefs != 0 {
		t.Fatalf("retained rows = messages:%d events:%d expired-refs:%d", messages, events, expiredRefs)
	}
	if days, enabled, deleted, err := mail.ConfigureRetention(-1, now); err != nil || days != 30 || !enabled || deleted != 0 {
		t.Fatalf("persisted retention = days:%d enabled:%v deleted:%d err:%v", days, enabled, deleted, err)
	}
	if days, enabled, deleted, err := mail.ConfigureRetention(0, now); err != nil || days != 0 || enabled || deleted != 0 {
		t.Fatalf("disabled retention = days:%d enabled:%v deleted:%d err:%v", days, enabled, deleted, err)
	}
}

func TestControlMetadataAgentInboxIntegrityRejectsCorruptStatusAndSequence(t *testing.T) {
	t.Run("invalid status", func(t *testing.T) {
		control := newTestControlMetadata(t)
		if _, err := control.CanonicalDatabase().Exec(`PRAGMA ignore_check_constraints=ON`); err != nil {
			t.Fatal(err)
		}
		if _, err := control.CanonicalDatabase().Exec(`INSERT INTO agent_message_seq(recipient_hosted_id,next_seq) VALUES('recipient',2)`); err != nil {
			t.Fatal(err)
		}
		if _, err := control.CanonicalDatabase().Exec(`INSERT INTO agent_messages(message_id,client_message_id,request_hash,sender_key,sender_kind,origin,recipient_hosted_id,seq,subject,body,status,created_at,updated_at)
			VALUES('nmsg_bad_status','bad-status','hash','sender','thread','agent','recipient',1,'','body','unknown','2026-08-27T00:00:00Z','2026-08-27T00:00:00Z')`); err != nil {
			t.Fatal(err)
		}
		if err := state.CheckMetadataIntegrity(control.CanonicalDatabase(), statewiring.Migrations()); err == nil || !strings.Contains(err.Error(), "agent message rows have invalid") {
			t.Fatalf("integrity check error = %v", err)
		}
	})

	t.Run("duplicate recipient sequence", func(t *testing.T) {
		control := newTestControlMetadata(t)
		for _, statement := range []string{
			`PRAGMA foreign_keys=OFF`,
			`DROP TABLE agent_message_events`,
			`DROP TABLE agent_message_refs`,
			`DROP TABLE agent_messages`,
			`CREATE TABLE agent_messages (message_id TEXT, recipient_hosted_id TEXT, seq INTEGER, status TEXT)`,
			`CREATE TABLE agent_message_events (message_id TEXT)`,
			`CREATE TABLE agent_message_refs (message_id TEXT)`,
			`INSERT INTO agent_message_seq(recipient_hosted_id,next_seq) VALUES('recipient',2)`,
			`INSERT INTO agent_messages(message_id,recipient_hosted_id,seq,status) VALUES('nmsg_one','recipient',1,'unread'),('nmsg_two','recipient',1,'seen')`,
		} {
			if _, err := control.CanonicalDatabase().Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		if err := state.CheckMetadataIntegrity(control.CanonicalDatabase(), statewiring.Migrations()); err == nil || !strings.Contains(err.Error(), "agent message rows have invalid") {
			t.Fatalf("integrity check error = %v", err)
		}
	})
}

func TestUnreadFooterOnAgentHumanAndSystemPrompts(t *testing.T) {
	t.Run("agent", func(t *testing.T) {
		s, _, _, sender, recipient, prompts, _ := stage0Server(t)
		_, _ = sendAgentMessageForTest(t, s, recipient, sender.ID, "mail-agent", "Mail", "Unread", nil)
		body := `{"clientActionId":"agent-turn","prompt":"Review","senderThreadRef":"` + sender.ID + `"}`
		rec := httptest.NewRecorder()
		s.handleAgentTurnSubmit(rec, httptest.NewRequest(http.MethodPost, "/turns", strings.NewReader(body)), recipient.ID)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("submit=%d %s", rec.Code, rec.Body.String())
		}
		got := <-prompts
		if !strings.Contains(got, senderFooter(turnAttribution{Origin: turnOriginAgent, SenderName: "Claude sender", SenderNativeThreadID: sender.NativeThreadID, SenderSubmittedAt: mustAgentTurn(t, s, recipient.ID, "agent-turn").SenderSubmittedAt})+"\n\n[nano: 1 unread — GET /api/v1/hosted-threads/"+recipient.ID+"/inbox]") {
			t.Fatalf("agent prompt footers = %q", got)
		}
	})
	t.Run("human", func(t *testing.T) {
		s, _, sess, sender, recipient, prompts, _ := stage0Server(t)
		_, _ = sendAgentMessageForTest(t, s, recipient, sender.ID, "mail-human", "Mail", "Unread", nil)
		rec := httptest.NewRecorder()
		sess.HandleTurn(rec, httptest.NewRequest(http.MethodPost, "/turn", strings.NewReader(`{"text":"Hello","clientActionId":"human-turn"}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("submit=%d %s", rec.Code, rec.Body.String())
		}
		if got := <-prompts; got != "Hello\n\n[nano: 1 unread — GET /api/v1/hosted-threads/"+recipient.ID+"/inbox]" {
			t.Fatalf("human prompt = %q", got)
		}
	})
	t.Run("system", func(t *testing.T) {
		s, _, _, sender, recipient, prompts, _ := stage0Server(t)
		_, _ = sendAgentMessageForTest(t, s, recipient, sender.ID, "mail-system", "Mail", "Unread", nil)
		if _, _, err := s.submitHostedTurnWithOptions(recipient.ID, "nano:test-system", "Nudge", false); err != nil {
			t.Fatal(err)
		}
		if got := <-prompts; got != "Nudge\n\n[from nano]\n\n[nano: 1 unread — GET /api/v1/hosted-threads/"+recipient.ID+"/inbox]" {
			t.Fatalf("system prompt = %q", got)
		}
	})
}

func mustAgentTurn(t *testing.T, s *server, hostedID, actionID string) agentTurnRecord {
	t.Helper()
	record, ok := fixtureTurnForAction(s.agentExecutions, hostedID, actionID)
	if !ok {
		t.Fatalf("turn %s missing", actionID)
	}
	return record
}

func TestQueuedUnreadFooterRetryUsesCallerHashAndPersistedText(t *testing.T) {
	s, _, sess, sender, recipient, _, fake := stage0Server(t)
	_, _ = sendAgentMessageForTest(t, s, recipient, sender.ID, "mail-queued", "Mail", "Unread", nil)
	(&providerRuntimeFixture{session: sess, codex: fake}).activate(t)
	// Held for the next turn, so it stays queued while the turn works.
	request := `{"text":"Queued","clientActionId":"queued-human","nextTurn":true}`
	first := httptest.NewRecorder()
	sess.HandleTurn(first, httptest.NewRequest(http.MethodPost, "/turn", strings.NewReader(request)))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first=%d %s", first.Code, first.Body.String())
	}
	queued, ok := s.queuedTurns.Get("queued-human")
	if !ok || !strings.Contains(queued.Text, "[nano: 1 unread") || queued.TextHash == queued.RequestHash || queued.RequestHash != turnRequestHash("Queued", true) {
		t.Fatalf("queued record = %+v", queued)
	}
	_, _, err := s.mail.ListInbox(recipient.ID, featuremail.ListOptions{Limit: 10}, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	retry := httptest.NewRecorder()
	sess.HandleTurn(retry, httptest.NewRequest(http.MethodPost, "/turn", strings.NewReader(request)))
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry=%d %s", retry.Code, retry.Body.String())
	}
	again, _ := s.queuedTurns.Get("queued-human")
	if again.Text != queued.Text {
		t.Fatalf("retry rewrote queued text: before=%q after=%q", queued.Text, again.Text)
	}
}

func TestPromptUsesNoticeSnapshotWithoutReadingMailStore(t *testing.T) {
	s, store, _, sender, recipient, prompts, _ := stage0Server(t)
	s.RegisterSession(newFixtureSession(sessionConfig{ID: "nano-sender", HostedThreadID: sender.ID, Provider: sender.Provider, DisplayName: "Claude sender"}, sender.NativeThreadID))
	_, _ = sendAgentMessageForTest(t, s, recipient, sender.ID, "notice-snapshot", "Mail", "Unread", nil)
	if err := s.mail.Close(); err != nil {
		t.Fatal(err)
	}
	body := `{"clientActionId":"notice-snapshot","prompt":"Deliver without a mail read","senderThreadRef":"` + sender.ID + `"}`
	rec := httptest.NewRecorder()
	s.handleAgentTurnSubmit(rec, httptest.NewRequest(http.MethodPost, "/turns", strings.NewReader(body)), recipient.ID)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit=%d %s", rec.Code, rec.Body.String())
	}
	if _, ok := fixtureTurnForAction(store, recipient.ID, "notice-snapshot"); !ok {
		t.Fatal("notice-backed prompt did not create an operation")
	}
	select {
	case prompt := <-prompts:
		if !strings.Contains(prompt, "[nano: 1 unread — GET /api/v1/hosted-threads/"+recipient.ID+"/inbox]") {
			t.Fatalf("notice snapshot missing from prompt: %q", prompt)
		}
	case <-time.After(time.Second):
		t.Fatal("prompt was not delivered")
	}
}

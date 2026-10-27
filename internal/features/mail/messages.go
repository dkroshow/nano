package mail

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	Unread   = "unread"
	Seen     = "seen"
	Acted    = "acted"
	Declined = "declined"
	Deferred = "deferred"

	MaxSubjectBytes = 120
	MaxBodyBytes    = 16 << 10
	MaxNoteBytes    = 500
	MaxRefs         = 16
)

type Ref struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type SenderView struct {
	Kind           string `json:"kind"`
	HostedThreadID string `json:"hostedThreadId,omitempty"`
	NativeThreadID string `json:"nativeThreadId,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Name           string `json:"name,omitempty"`
	System         string `json:"system,omitempty"`
}

type RecipientView struct {
	HostedThreadID string `json:"hostedThreadId"`
}

type Message struct {
	MessageID            string        `json:"messageId"`
	ClientMessageID      string        `json:"clientMessageId"`
	Origin               string        `json:"origin"`
	Sender               SenderView    `json:"sender"`
	Recipient            RecipientView `json:"recipient"`
	Seq                  int64         `json:"seq"`
	Subject              string        `json:"subject,omitempty"`
	Body                 string        `json:"body"`
	Refs                 []Ref         `json:"refs"`
	Status               string        `json:"status"`
	StatusNote           string        `json:"statusNote,omitempty"`
	DispositionFeedback  bool          `json:"dispositionFeedback"`
	FeedbackForMessageID string        `json:"feedbackForMessageId,omitempty"`
	CreatedAt            string        `json:"createdAt"`
	UpdatedAt            string        `json:"updatedAt"`
	MessageOrder         int64
	senderKey            string
}

type EventView struct {
	ID            int64           `json:"id"`
	MessageID     string          `json:"messageId"`
	Kind          string          `json:"kind"`
	ActorKind     string          `json:"actorKind"`
	ActorHostedID string          `json:"actorHostedThreadId,omitempty"`
	Detail        json.RawMessage `json:"detail,omitempty"`
	At            string          `json:"at"`
}

type DetailView struct {
	Message Message     `json:"message"`
	Events  []EventView `json:"events"`
}

type Create struct {
	ClientMessageID      string
	RequestHash          string
	SenderKey            string
	SenderKind           string
	Origin               string
	SenderHostedID       string
	SenderNativeID       string
	SenderProvider       string
	SenderName           string
	RecipientHostedID    string
	Subject              string
	Body                 string
	Refs                 []Ref
	DispositionFeedback  bool
	FeedbackForMessageID string
	CreatedAt            time.Time
}

type ListOptions struct {
	Since  int64
	Limit  int
	Status []string
	Ref    *Ref
}

type StoreError struct {
	Code   string
	Detail string
}

func (e *StoreError) Error() string { return e.Detail }

var ErrNotFound = errors.New("agent message not found")
var ErrUnreadNoticeUnavailable = errors.New("unread footer unavailable")

const agentMessageSelect = `SELECT message_order,message_id,client_message_id,origin,sender_key,sender_kind,
	COALESCE(sender_hosted_id,''),COALESCE(sender_native_id,''),COALESCE(sender_provider,''),COALESCE(sender_name,''),
	recipient_hosted_id,seq,subject,body,COALESCE(feedback_for_message_id,''),disposition_feedback,status,COALESCE(status_note,''),created_at,updated_at
	FROM agent_messages`

type sqlScanner interface {
	Scan(dest ...any) error
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func scanAgentMessage(scanner sqlScanner) (Message, error) {
	var value Message
	var senderKind, senderHosted, senderNative, senderProvider, senderName string
	err := scanner.Scan(&value.MessageOrder, &value.MessageID, &value.ClientMessageID, &value.Origin,
		&value.senderKey, &senderKind, &senderHosted, &senderNative, &senderProvider, &senderName,
		&value.Recipient.HostedThreadID, &value.Seq, &value.Subject, &value.Body,
		&value.FeedbackForMessageID, &value.DispositionFeedback, &value.Status, &value.StatusNote, &value.CreatedAt, &value.UpdatedAt)
	if err != nil {
		return Message{}, err
	}
	value.Refs = []Ref{}
	value.Sender = SenderView{Kind: senderKind, HostedThreadID: senderHosted,
		NativeThreadID: senderNative, Provider: senderProvider, Name: senderName}
	if senderKind == "system" {
		value.Sender.System = "nano"
	}
	return value, nil
}

func newAgentMessageID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return "nmsg_" + hex.EncodeToString(data), nil
}

func NormalizeRefs(refs []Ref) []Ref {
	seen := make(map[string]bool, len(refs))
	result := make([]Ref, 0, len(refs))
	for _, ref := range refs {
		ref.Kind, ref.ID = strings.TrimSpace(ref.Kind), strings.TrimSpace(ref.ID)
		key := ref.Kind + "\x00" + ref.ID
		if !seen[key] {
			seen[key] = true
			result = append(result, ref)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind == result[j].Kind {
			return result[i].ID < result[j].ID
		}
		return result[i].Kind < result[j].Kind
	})
	return result
}

func RequestHash(recipient, subject, body string, refs []Ref, feedbackFor string) string {
	return RequestHashWithFeedback(recipient, subject, body, refs, false, feedbackFor)
}

func RequestHashWithFeedback(recipient, subject, body string, refs []Ref, dispositionFeedback bool, feedbackFor string) string {
	encoded, _ := json.Marshal(struct {
		Recipient           string `json:"recipient"`
		Subject             string `json:"subject"`
		Body                string `json:"body"`
		Refs                []Ref  `json:"refs"`
		DispositionFeedback bool   `json:"dispositionFeedback"`
		FeedbackFor         string `json:"feedbackFor,omitempty"`
	}{recipient, subject, body, NormalizeRefs(refs), dispositionFeedback, feedbackFor})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (s *Store) CreateMessage(req Create) (Message, bool, error) {
	if s == nil || s.DB == nil {
		return Message{}, false, errors.New("durable agent inbox unavailable")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return Message{}, false, err
	}
	defer tx.Rollback()
	message, created, err := insertAgentMessageTx(tx, req)
	if err != nil {
		return Message{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Message{}, false, err
	}
	return message, created, nil
}

func insertAgentMessageTx(tx *sql.Tx, req Create) (Message, bool, error) {
	existing, err := scanAgentMessage(tx.QueryRow(agentMessageSelect+` WHERE sender_key=? AND client_message_id=?`, req.SenderKey, req.ClientMessageID))
	if err == nil {
		var existingHash string
		if err := tx.QueryRow(`SELECT request_hash FROM agent_messages WHERE message_id=?`, existing.MessageID).Scan(&existingHash); err != nil {
			return Message{}, false, err
		}
		if existingHash != req.RequestHash {
			return Message{}, false, &StoreError{Code: "client_message_id_conflict", Detail: "clientMessageId was already used for different mail"}
		}
		existing.Refs, err = agentMessageRefsTx(tx, existing.MessageID)
		return existing, false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, err
	}
	var seq int64
	err = tx.QueryRow(`INSERT INTO agent_message_seq(recipient_hosted_id,next_seq) VALUES(?,2)
		ON CONFLICT(recipient_hosted_id) DO UPDATE SET next_seq=agent_message_seq.next_seq+1 RETURNING next_seq-1`, req.RecipientHostedID).Scan(&seq)
	if err != nil {
		return Message{}, false, err
	}
	now := req.CreatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	stamp := now.Format(time.RFC3339Nano)
	refs := NormalizeRefs(req.Refs)
	var messageID string
	for attempt := 0; attempt < 3; attempt++ {
		messageID, err = newAgentMessageID()
		if err != nil {
			return Message{}, false, err
		}
		_, err = tx.Exec(`INSERT INTO agent_messages(message_id,client_message_id,request_hash,sender_key,sender_kind,origin,
			sender_hosted_id,sender_native_id,sender_provider,sender_name,recipient_hosted_id,seq,subject,body,
			feedback_for_message_id,disposition_feedback,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			messageID, req.ClientMessageID, req.RequestHash, req.SenderKey, req.SenderKind, req.Origin,
			nullableText(req.SenderHostedID), nullableText(req.SenderNativeID), nullableText(req.SenderProvider), nullableText(req.SenderName),
			req.RecipientHostedID, seq, req.Subject, req.Body, nullableText(req.FeedbackForMessageID), req.DispositionFeedback, Unread, stamp, stamp)
		if err == nil {
			break
		}
		if !strings.Contains(strings.ToLower(err.Error()), "message_id") {
			return Message{}, false, err
		}
	}
	if err != nil {
		return Message{}, false, err
	}
	for _, ref := range refs {
		if _, err := tx.Exec(`INSERT INTO agent_message_refs(message_id,kind,id) VALUES(?,?,?)`, messageID, ref.Kind, ref.ID); err != nil {
			return Message{}, false, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO agent_message_events(message_id,kind,actor_kind,actor_hosted_id,at) VALUES(?,?,?,?,?)`,
		messageID, "created", req.SenderKind, nullableText(req.SenderHostedID), stamp); err != nil {
		return Message{}, false, err
	}
	message, err := scanAgentMessage(tx.QueryRow(agentMessageSelect+` WHERE message_id=?`, messageID))
	if err != nil {
		return Message{}, false, err
	}
	message.Refs = refs
	return message, true, nil
}

func agentMessageRefsTx(tx *sql.Tx, messageID string) ([]Ref, error) {
	rows, err := tx.Query(`SELECT kind,id FROM agent_message_refs WHERE message_id=? ORDER BY kind,id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := []Ref{}
	for rows.Next() {
		var ref Ref
		if err := rows.Scan(&ref.Kind, &ref.ID); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// listAgentInbox returns one recipient's mail and, unless peeking, marks the
// returned unread rows seen. The second result is how many rows this call
// actually flipped, so the API layer knows whether counts changed.
func (s *Store) ListInbox(recipient string, opts ListOptions, markSeen bool, now time.Time) ([]Message, int, error) {
	if s == nil || s.DB == nil {
		return nil, 0, errors.New("durable agent inbox unavailable")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	where := []string{"recipient_hosted_id=?", "seq>?"}
	args := []any{recipient, opts.Since}
	where, args = appendAgentMessageListFilters(where, args, opts)
	query := agentMessageSelect + ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY seq LIMIT ?`
	args = append(args, opts.Limit)
	items, err := queryAgentMessagesTx(tx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if now.IsZero() {
		stamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	marked := 0
	if markSeen {
		for i := range items {
			if items[i].Status != Unread {
				continue
			}
			result, err := tx.Exec(`UPDATE agent_messages SET status='seen',updated_at=? WHERE message_id=? AND status='unread'`, stamp, items[i].MessageID)
			if err != nil {
				return nil, 0, err
			}
			changed, _ := result.RowsAffected()
			if changed == 1 {
				if _, err := tx.Exec(`INSERT INTO agent_message_events(message_id,kind,actor_kind,actor_hosted_id,at) VALUES(?,?,?,?,?)`, items[i].MessageID, Seen, "thread", recipient, stamp); err != nil {
					return nil, 0, err
				}
				items[i].Status, items[i].UpdatedAt = Seen, stamp
				marked++
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return items, marked, nil
}

func (s *Store) ListOutbox(sender string, opts ListOptions) ([]Message, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("durable agent inbox unavailable")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	where := []string{"sender_hosted_id=?", "message_order>?"}
	args := []any{sender, opts.Since}
	where, args = appendAgentMessageListFilters(where, args, opts)
	query := agentMessageSelect + ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY message_order LIMIT ?`
	args = append(args, opts.Limit)
	items, err := queryAgentMessagesTx(tx, query, args...)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return items, nil
}

func appendAgentMessageListFilters(where []string, args []any, opts ListOptions) ([]string, []any) {
	if len(opts.Status) > 0 {
		marks := make([]string, len(opts.Status))
		for i, status := range opts.Status {
			marks[i], args = "?", append(args, status)
		}
		where = append(where, "status IN ("+strings.Join(marks, ",")+")")
	}
	if opts.Ref != nil {
		where = append(where, `EXISTS (SELECT 1 FROM agent_message_refs r WHERE r.message_id=agent_messages.message_id AND r.kind=? AND r.id=?)`)
		args = append(args, opts.Ref.Kind, opts.Ref.ID)
	}
	return where, args
}

func queryAgentMessagesTx(tx *sql.Tx, query string, args ...any) ([]Message, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	items := []Message{}
	for rows.Next() {
		item, err := scanAgentMessage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Refs, err = agentMessageRefsTx(tx, items[i].MessageID)
		if err != nil {
			return nil, err
		}
	}
	return items, nil
}

func (s *Store) MessageDetail(messageID string) (DetailView, error) {
	if s == nil || s.DB == nil {
		return DetailView{}, errors.New("durable agent inbox unavailable")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return DetailView{}, err
	}
	defer tx.Rollback()
	message, err := scanAgentMessage(tx.QueryRow(agentMessageSelect+` WHERE message_id=?`, messageID))
	if errors.Is(err, sql.ErrNoRows) {
		return DetailView{}, ErrNotFound
	}
	if err != nil {
		return DetailView{}, err
	}
	message.Refs, err = agentMessageRefsTx(tx, messageID)
	if err != nil {
		return DetailView{}, err
	}
	rows, err := tx.Query(`SELECT event_id,message_id,kind,actor_kind,COALESCE(actor_hosted_id,''),COALESCE(detail_json,''),at FROM agent_message_events WHERE message_id=? ORDER BY event_id`, messageID)
	if err != nil {
		return DetailView{}, err
	}
	events := []EventView{}
	for rows.Next() {
		var event EventView
		var detail string
		if err := rows.Scan(&event.ID, &event.MessageID, &event.Kind, &event.ActorKind, &event.ActorHostedID, &detail, &event.At); err != nil {
			rows.Close()
			return DetailView{}, err
		}
		if detail != "" {
			event.Detail = json.RawMessage(detail)
		}
		events = append(events, event)
	}
	if err := rows.Close(); err != nil {
		return DetailView{}, err
	}
	if err := tx.Commit(); err != nil {
		return DetailView{}, err
	}
	return DetailView{Message: message, Events: events}, nil
}

func validAgentMessageTransition(from, to string) bool {
	switch from {
	case Unread, Seen:
		return to == Acted || to == Declined || to == Deferred
	case Deferred:
		return to == Acted || to == Declined
	default:
		return false
	}
}

func (s *Store) DispositionMessage(messageID, actorHostedID, actorName, actorNativeID, status, note string, now time.Time) (Message, *Message, error) {
	if s == nil || s.DB == nil {
		return Message{}, nil, errors.New("durable agent inbox unavailable")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return Message{}, nil, err
	}
	defer tx.Rollback()
	message, err := scanAgentMessage(tx.QueryRow(agentMessageSelect+` WHERE message_id=?`, messageID))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, nil, ErrNotFound
	}
	if err != nil {
		return Message{}, nil, err
	}
	message.Refs, err = agentMessageRefsTx(tx, messageID)
	if err != nil {
		return Message{}, nil, err
	}
	if message.Recipient.HostedThreadID != actorHostedID {
		return Message{}, nil, &StoreError{Code: "not_recipient", Detail: "only the recipient may disposition this message"}
	}
	if message.Status == status && message.StatusNote == note {
		if err := tx.Commit(); err != nil {
			return Message{}, nil, err
		}
		return message, nil, nil
	}
	if !validAgentMessageTransition(message.Status, status) {
		return Message{}, nil, &StoreError{Code: "status_transition_invalid", Detail: fmt.Sprintf("cannot change message status from %s to %s", message.Status, status)}
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if now.IsZero() {
		stamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	result, err := tx.Exec(`UPDATE agent_messages SET status=?,status_note=?,updated_at=? WHERE message_id=? AND status=?`, status, nullableText(note), stamp, messageID, message.Status)
	if err != nil {
		return Message{}, nil, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Message{}, nil, errors.New("message status changed concurrently")
	}
	detail, _ := json.Marshal(map[string]string{"note": note})
	if _, err := tx.Exec(`INSERT INTO agent_message_events(message_id,kind,actor_kind,actor_hosted_id,detail_json,at) VALUES(?,?,?,?,?,?)`, messageID, status, "thread", actorHostedID, string(detail), stamp); err != nil {
		return Message{}, nil, err
	}
	message.Status, message.StatusNote, message.UpdatedAt = status, note, stamp
	var feedback *Message
	if message.DispositionFeedback && message.Sender.Kind == "thread" && message.Sender.HostedThreadID != "" {
		who := strings.TrimSpace(actorName)
		if who == "" {
			who = "recipient"
		}
		if native := strings.TrimSpace(actorNativeID); native != "" {
			who += " (" + native + ")"
		}
		body := fmt.Sprintf("%s %s message %s.", who, status, message.MessageID)
		if note != "" {
			body += " Note: " + note
		}
		clientID := "nano:outcome:" + message.MessageID + ":" + status
		refs := []Ref{{Kind: "nano.message", ID: message.MessageID}}
		request := Create{ClientMessageID: clientID, SenderKey: "nano", SenderKind: "system", Origin: "system",
			RecipientHostedID: message.Sender.HostedThreadID, Subject: "Message " + status, Body: body,
			Refs: refs, FeedbackForMessageID: message.MessageID, CreatedAt: now}
		request.RequestHash = RequestHash(request.RecipientHostedID, request.Subject, request.Body, refs, message.MessageID)
		createdFeedback, _, err := insertAgentMessageTx(tx, request)
		if err != nil {
			return Message{}, nil, err
		}
		feedback = &createdFeedback
	}
	if err := tx.Commit(); err != nil {
		return Message{}, nil, err
	}
	return message, feedback, nil
}

// Counts summarizes one recipient's inbox for UI badges: total mail
// ever received, mail the agent has not read, and mail read but never
// dispositioned (the defined error state worth surfacing).
type Counts struct {
	Total  int `json:"total"`
	Unread int `json:"unread"`
	Seen   int `json:"seen"`
}

// agentMailCountsFor returns per-recipient inbox counts. A nil or empty
// recipients slice returns counts for every recipient with mail. Recipients
// without mail are absent from the map.
func (s *Store) CountsFor(recipients []string) (map[string]Counts, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("durable agent inbox unavailable")
	}
	query := `SELECT recipient_hosted_id,count(*),
		COALESCE(sum(status='unread'),0),COALESCE(sum(status='seen'),0)
		FROM agent_messages`
	args := []any{}
	if len(recipients) > 0 {
		marks := make([]string, len(recipients))
		for i, id := range recipients {
			marks[i], args = "?", append(args, id)
		}
		query += ` WHERE recipient_hosted_id IN (` + strings.Join(marks, ",") + `)`
	}
	query += ` GROUP BY recipient_hosted_id`
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]Counts{}
	for rows.Next() {
		var id string
		var counts Counts
		if err := rows.Scan(&id, &counts.Total, &counts.Unread, &counts.Seen); err != nil {
			return nil, err
		}
		result[id] = counts
	}
	return result, rows.Err()
}

// listAgentMessagesAll is the operator's cross-thread mail listing: every
// recipient's mail in descending durable creation order. Read-only — it never
// marks mail seen. opts.Since is interpreted as "strictly before this
// message_order" so pages walk backward from newest; zero means newest.
func (s *Store) ListAll(opts ListOptions) ([]Message, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("durable agent inbox unavailable")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	where := []string{"1=1"}
	args := []any{}
	if opts.Since > 0 {
		where, args = []string{"message_order<?"}, []any{opts.Since}
	}
	where, args = appendAgentMessageListFilters(where, args, opts)
	query := agentMessageSelect + ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY message_order DESC LIMIT ?`
	args = append(args, opts.Limit)
	items, err := queryAgentMessagesTx(tx, query, args...)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return items, nil
}

// editAgentMessage rewrites the subject and body of one operator-authored
// message while it is still unread. Once the recipient has read the message it
// is frozen: dispositions and audits refer to the words the agent saw. The
// previous content is preserved as an "edited" event so history stays complete.
func (s *Store) EditMessage(messageID, subject, body string, now time.Time) (Message, error) {
	if s == nil || s.DB == nil {
		return Message{}, errors.New("durable agent inbox unavailable")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback()
	message, err := scanAgentMessage(tx.QueryRow(agentMessageSelect+` WHERE message_id=?`, messageID))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, err
	}
	if message.Sender.Kind != "operator" {
		return Message{}, &StoreError{Code: "not_editable", Detail: "only operator-authored messages may be edited"}
	}
	if message.Status != Unread {
		return Message{}, &StoreError{Code: "message_already_read", Detail: "the recipient has read this message; send a follow-up instead"}
	}
	message.Refs, err = agentMessageRefsTx(tx, messageID)
	if err != nil {
		return Message{}, err
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if now.IsZero() {
		stamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	hash := RequestHashWithFeedback(message.Recipient.HostedThreadID, subject, body, message.Refs, message.DispositionFeedback, message.FeedbackForMessageID)
	result, err := tx.Exec(`UPDATE agent_messages SET subject=?,body=?,request_hash=?,updated_at=? WHERE message_id=? AND status='unread'`,
		subject, body, hash, stamp, messageID)
	if err != nil {
		return Message{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Message{}, errors.New("message changed concurrently")
	}
	detail, _ := json.Marshal(map[string]string{"previousSubject": message.Subject, "previousBody": message.Body})
	if _, err := tx.Exec(`INSERT INTO agent_message_events(message_id,kind,actor_kind,detail_json,at) VALUES(?,?,?,?,?)`,
		messageID, "edited", "operator", string(detail), stamp); err != nil {
		return Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return Message{}, err
	}
	message.Subject, message.Body, message.UpdatedAt = subject, body, stamp
	return message, nil
}

func (s *Store) UnreadCount(recipient string) (int, int64, error) {
	if s == nil || s.DB == nil {
		return 0, 0, errors.New("durable agent inbox unavailable")
	}
	var count int
	var highSeq int64
	if err := s.DB.QueryRow(`SELECT count(*),COALESCE(max(seq),0) FROM agent_messages WHERE recipient_hosted_id=? AND status='unread'`, recipient).Scan(&count, &highSeq); err != nil {
		return 0, 0, err
	}
	return count, highSeq, nil
}

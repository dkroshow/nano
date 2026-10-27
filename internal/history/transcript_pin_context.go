package history

// transcript_pin_context.go — read side for thread transcript pins.
//
// A pin anchors one normalized user or agent message by its canonical item id.
// Canonical ids are deterministic source-lineage hashes, so they survive a
// delete-and-rebuild of this catalog; legacy projection-scoped ids resolve
// through conversation_item_aliases. These reads never write.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrPinAnchorNotFound means the thread exists but its active projection has
// no user or agent message with the requested id. Callers report it as a
// broken anchor; they never repair or delete the pin.
var ErrPinAnchorNotFound = errors.New("pin anchor message not found")

// MaxPinContextTurns bounds how many preceding turns one context read returns.
const MaxPinContextTurns = 50

type PinMessage struct {
	ItemID        string             `json:"itemId"`
	TurnKey       string             `json:"turnId"`
	TurnOrdinal   int                `json:"turnOrdinal"`
	ProjectedSeq  int64              `json:"projectedSeq"`
	Type          TranscriptItemType `json:"type"`
	Text          string             `json:"text"`
	CreatedAt     *time.Time         `json:"createdAt,omitempty"`
	ContentRef    string             `json:"contentRef,omitempty"`
	OriginalBytes int64              `json:"originalBytes"`
	// Body holds the retained full text when the inline text is a preview.
	Body          *string `json:"-"`
	BodyTruncated bool    `json:"-"`
}

type PinTurn struct {
	Key          string               `json:"turnId"`
	NativeTurnID string               `json:"nativeTurnId,omitempty"`
	Ordinal      int                  `json:"ordinal"`
	Status       TranscriptTurnStatus `json:"status"`
	StartedAt    *time.Time           `json:"startedAt,omitempty"`
	CompletedAt  *time.Time           `json:"completedAt,omitempty"`
}

type PinContext struct {
	ThreadID      string
	ProjectionKey string
	Anchor        PinMessage
	// Turns are chronological; the anchor's own turn is last.
	Turns []PinTurn
	// Messages are chronological user/agent messages from the first returned
	// turn through the anchor itself. Later messages in the anchor turn are
	// excluded: the pin marks a point in the conversation, not the turn.
	Messages []PinMessage
	// HasEarlierHistory reports whether any turn precedes the first returned turn.
	HasEarlierHistory bool
}

const pinMessageColumns = `
ci.item_id, ci.turn_key, ci.turn_ordinal, ci.projected_seq, ci.item_type,
ci.display_text, ci.created_at, COALESCE(ci.content_ref, ''), ci.original_bytes`

const pinMessageTypes = `ci.item_type IN ('user_message', 'agent_message')`

func scanPinMessage(row interface{ Scan(...any) error }) (PinMessage, error) {
	var message PinMessage
	var created sql.NullString
	if err := row.Scan(&message.ItemID, &message.TurnKey, &message.TurnOrdinal, &message.ProjectedSeq,
		&message.Type, &message.Text, &created, &message.ContentRef, &message.OriginalBytes); err != nil {
		return PinMessage{}, err
	}
	var err error
	message.CreatedAt, err = parseNullTime(created)
	return message, err
}

// activePinProjection returns the active projection id for a thread, or
// ErrThreadNotFound / ErrPinAnchorNotFound when there is nothing to anchor in.
func activePinProjection(ctx context.Context, tx *sql.Tx, threadID string) (int64, string, error) {
	var projectionID int64
	var key string
	err := tx.QueryRowContext(ctx, `SELECT id, projection_key FROM conversation_projections WHERE thread_id = ? AND state = 'active'`, threadID).Scan(&projectionID, &key)
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if checkErr := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads WHERE id = ?`, threadID).Scan(&exists); checkErr != nil {
			return 0, "", checkErr
		}
		if exists == 0 {
			return 0, "", ErrThreadNotFound
		}
		return 0, "", ErrPinAnchorNotFound
	}
	return projectionID, key, err
}

// LocatePinAnchor resolves itemID (canonical or legacy alias) to the user or
// agent message it names in the thread's active projection. An empty itemID
// selects the thread's latest user or agent message: "pin this".
func (s *Store) LocatePinAnchor(ctx context.Context, threadID, itemID string) (PinMessage, error) {
	threadID, itemID = strings.TrimSpace(threadID), strings.TrimSpace(itemID)
	if threadID == "" {
		return PinMessage{}, ErrInvalidTranscriptPage
	}
	if itemID != "" {
		canonical, err := s.ResolveTranscriptItemID(ctx, threadID, itemID)
		if err != nil {
			return PinMessage{}, err
		}
		itemID = canonical
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PinMessage{}, err
	}
	defer tx.Rollback()
	projectionID, _, err := activePinProjection(ctx, tx, threadID)
	if err != nil {
		return PinMessage{}, err
	}
	var row *sql.Row
	if itemID == "" {
		row = tx.QueryRowContext(ctx, `SELECT `+pinMessageColumns+` FROM conversation_items ci
WHERE ci.projection_id = ? AND `+pinMessageTypes+` ORDER BY ci.projected_seq DESC LIMIT 1`, projectionID)
	} else {
		row = tx.QueryRowContext(ctx, `SELECT `+pinMessageColumns+` FROM conversation_items ci
WHERE ci.projection_id = ? AND ci.item_id = ? AND `+pinMessageTypes, projectionID, itemID)
	}
	message, err := scanPinMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PinMessage{}, ErrPinAnchorNotFound
	}
	return message, err
}

// ReadPinContext returns the anchored message and the whole turns before it:
// up to `turns` preceding turns plus the anchor's own turn, cut at the anchor.
func (s *Store) ReadPinContext(ctx context.Context, threadID, itemID string, turns int) (PinContext, error) {
	threadID, itemID = strings.TrimSpace(threadID), strings.TrimSpace(itemID)
	if threadID == "" || itemID == "" || turns < 0 || turns > MaxPinContextTurns {
		return PinContext{}, ErrInvalidTranscriptPage
	}
	canonical, err := s.ResolveTranscriptItemID(ctx, threadID, itemID)
	if err != nil {
		return PinContext{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PinContext{}, err
	}
	defer tx.Rollback()
	projectionID, key, err := activePinProjection(ctx, tx, threadID)
	if err != nil {
		return PinContext{}, err
	}
	result := PinContext{ThreadID: threadID, ProjectionKey: key}
	result.Anchor, err = scanPinMessage(tx.QueryRowContext(ctx, `SELECT `+pinMessageColumns+` FROM conversation_items ci
WHERE ci.projection_id = ? AND ci.item_id = ? AND `+pinMessageTypes, projectionID, canonical))
	if errors.Is(err, sql.ErrNoRows) {
		return PinContext{}, ErrPinAnchorNotFound
	}
	if err != nil {
		return PinContext{}, err
	}

	rows, err := tx.QueryContext(ctx, `
SELECT turn_key, COALESCE(native_turn_id, ''), ordinal, status, started_at, completed_at
FROM conversation_turns
WHERE projection_id = ? AND ordinal <= ?
ORDER BY ordinal DESC LIMIT ?`, projectionID, result.Anchor.TurnOrdinal, turns+1)
	if err != nil {
		return PinContext{}, err
	}
	var descending []PinTurn
	for rows.Next() {
		var turn PinTurn
		var started, completed sql.NullString
		if err := rows.Scan(&turn.Key, &turn.NativeTurnID, &turn.Ordinal, &turn.Status, &started, &completed); err != nil {
			rows.Close()
			return PinContext{}, err
		}
		if turn.StartedAt, err = parseNullTime(started); err != nil {
			rows.Close()
			return PinContext{}, err
		}
		if turn.CompletedAt, err = parseNullTime(completed); err != nil {
			rows.Close()
			return PinContext{}, err
		}
		descending = append(descending, turn)
	}
	if err := rows.Close(); err != nil {
		return PinContext{}, err
	}
	for index := len(descending) - 1; index >= 0; index-- {
		result.Turns = append(result.Turns, descending[index])
	}
	firstOrdinal := result.Anchor.TurnOrdinal
	if len(result.Turns) > 0 {
		firstOrdinal = result.Turns[0].Ordinal
	}
	var earlier int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_turns WHERE projection_id = ? AND ordinal < ?)`,
		projectionID, firstOrdinal).Scan(&earlier); err != nil {
		return PinContext{}, err
	}
	result.HasEarlierHistory = earlier == 1

	rows, err = tx.QueryContext(ctx, `SELECT `+pinMessageColumns+`, cib.body_text, COALESCE(cib.truncated, 0)
FROM conversation_items ci
LEFT JOIN conversation_item_bodies cib ON cib.projection_id = ci.projection_id AND cib.item_id = ci.item_id
WHERE ci.projection_id = ? AND `+pinMessageTypes+` AND ci.turn_ordinal >= ? AND ci.projected_seq <= ?
ORDER BY ci.projected_seq`, projectionID, firstOrdinal, result.Anchor.ProjectedSeq)
	if err != nil {
		return PinContext{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var message PinMessage
		var created, body sql.NullString
		if err := rows.Scan(&message.ItemID, &message.TurnKey, &message.TurnOrdinal, &message.ProjectedSeq,
			&message.Type, &message.Text, &created, &message.ContentRef, &message.OriginalBytes,
			&body, &message.BodyTruncated); err != nil {
			return PinContext{}, err
		}
		if message.CreatedAt, err = parseNullTime(created); err != nil {
			return PinContext{}, err
		}
		if message.ContentRef != "" && body.Valid {
			text := body.String
			message.Body = &text
		}
		result.Messages = append(result.Messages, message)
	}
	if err := rows.Err(); err != nil {
		return PinContext{}, err
	}
	return result, tx.Commit()
}

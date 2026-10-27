package history

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	fleetCourtHeadRunes = 240
	fleetCourtTailRunes = 600
)

// FleetCourtMessage is one thread's court-deciding message. ItemType is empty
// when the thread has no such message at all — no projection yet, or nothing
// but tool traffic — which the classifier reads as an empty thread.
// LastActivity is nil when the provider file carried no timestamps.
type FleetCourtMessage struct {
	ItemType     TranscriptItemType
	Head         string
	Tail         string
	LastActivity *time.Time
	Interrupted  bool
}

func (s *Store) FleetCourtMessages(ctx context.Context, threadIDs []string) (map[string]FleetCourtMessage, error) {
	out := make(map[string]FleetCourtMessage, len(threadIDs))
	if len(threadIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(threadIDs))
	args := make([]any, 0, len(threadIDs)+3)
	args = append(args, fleetCourtHeadRunes, -fleetCourtTailRunes, string(ThreadSubagent))
	for index, id := range threadIDs {
		placeholders[index] = "?"
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT t.id, t.updated_at,
       COALESCE(ci.item_type, ''),
       COALESCE(substr(ci.display_text, 1, ?), ''),
       COALESCE(substr(ci.display_text, ?), ''),
       COALESCE((
         SELECT latest.status = 'interrupted' FROM conversation_turns latest
         WHERE latest.projection_id = cp.id
         ORDER BY latest.ordinal DESC LIMIT 1
       ), 0)
FROM threads t
LEFT JOIN conversation_projections cp ON cp.thread_id = t.id AND cp.state = 'active'
LEFT JOIN conversation_items ci ON ci.projection_id = cp.id AND ci.projected_seq = (
  SELECT MAX(court.projected_seq) FROM conversation_items court
  WHERE court.projection_id = cp.id
    AND (court.sidechain = 0 OR t.kind = ?)
    AND (court.item_type = 'user_message'
         OR (court.item_type = 'agent_message' AND court.is_final_answer = 1))
)
WHERE t.id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read fleet court messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, itemType, head, tail string
		var interrupted bool
		var updatedAt sql.NullString
		if err := rows.Scan(&id, &updatedAt, &itemType, &head, &tail, &interrupted); err != nil {
			return nil, err
		}
		lastActivity, err := parseNullTime(updatedAt)
		if err != nil {
			return nil, err
		}
		out[id] = FleetCourtMessage{
			ItemType: TranscriptItemType(itemType), Head: head, Tail: tail,
			LastActivity: lastActivity, Interrupted: interrupted,
		}
	}
	return out, rows.Err()
}

// ThreadReference is one candidate match for an agent-supplied thread
// reference: an exact nano or native id, or a prefix of either.
type ThreadReference struct {
	ID       string
	Provider Provider
	NativeID string
}

// ResolveThreadReferences matches exact ids first and only falls back to prefix
// matching when nothing is exact — so a reference that is both a complete id
// and a prefix of a longer one resolves to the thread the caller named. The
// limit bounds the candidate list a caller reports back on an ambiguous match.
func (s *Store) ResolveThreadReferences(ctx context.Context, reference string, limit int) ([]ThreadReference, error) {
	exact, err := s.threadReferences(ctx, `
SELECT id, provider, native_thread_id FROM threads
WHERE id = ? OR native_thread_id = ? ORDER BY id LIMIT ?`, reference, reference, limit)
	if err != nil || len(exact) > 0 {
		return exact, err
	}
	pattern := escapeLikePrefix(reference)
	return s.threadReferences(ctx, `
SELECT id, provider, native_thread_id FROM threads
WHERE id LIKE ? ESCAPE '\' OR native_thread_id LIKE ? ESCAPE '\' ORDER BY id LIMIT ?`,
		pattern, pattern, limit)
}

func (s *Store) threadReferences(ctx context.Context, query string, args ...any) ([]ThreadReference, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("resolve thread reference: %w", err)
	}
	defer rows.Close()
	var matches []ThreadReference
	for rows.Next() {
		var match ThreadReference
		if err := rows.Scan(&match.ID, &match.Provider, &match.NativeID); err != nil {
			return nil, err
		}
		matches = append(matches, match)
	}
	return matches, rows.Err()
}

// escapeLikePrefix turns a caller-supplied reference into a LIKE prefix pattern
// with the wildcards neutralized, so `%` in a reference matches a literal `%`
// rather than every thread in the catalog.
func escapeLikePrefix(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value) + "%"
}

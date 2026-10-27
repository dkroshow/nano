package history

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// loadItemObservationMap decodes the observations of the items selected by
// filter, a fixed SQL tail over conversation_item_observations m.
func loadItemObservationMap(ctx context.Context, tx *sql.Tx, projectionID int64, filter string, args ...any) (map[string][]model.Observation, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT m.item_id,o.key_id,o.scope_id,o.source_id,o.block_path_id,o.source_line,o.source_frame,o.byte_start,o.byte_length,o.source_sha256,o.facts_blob
FROM conversation_item_observations m
JOIN conversation_observations o ON o.id=m.observation_id AND o.projection_id=m.projection_id
`+filter+`
ORDER BY m.item_id,o.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string][]model.Observation{}
	for rows.Next() {
		var itemID string
		var keyID, scopeID, sourceID, pathID, byteLength int64
		var observation model.Observation
		var facts, sourceHash []byte
		if err := rows.Scan(&itemID, &keyID, &scopeID, &sourceID, &pathID, &observation.Source.Line, &observation.Source.Frame, &observation.Source.Start, &byteLength, &sourceHash, &facts); err != nil {
			return nil, err
		}
		if err := loadTranscriptValueTx(ctx, tx, projectionID, keyID, &observation.ID); err != nil {
			return nil, err
		}
		if err := loadTranscriptValueTx(ctx, tx, projectionID, scopeID, &observation.Scope); err != nil {
			return nil, err
		}
		position := observation.Source
		if err := loadTranscriptValueTx(ctx, tx, projectionID, sourceID, &observation.Source); err != nil {
			return nil, err
		}
		if pathID != 0 {
			if err := loadTranscriptValueTx(ctx, tx, projectionID, pathID, &observation.Source.BlockPath); err != nil {
				return nil, err
			}
		}
		observation.Source.Line, observation.Source.Frame = position.Line, position.Frame
		observation.Source.Start, observation.Source.End = position.Start, position.Start+byteLength
		observation.Source.SHA256 = hex.EncodeToString(sourceHash)
		facts, err = decodeTranscriptFacts(facts)
		if err != nil {
			return nil, err
		}
		var saved storedObservationFacts
		if err := json.Unmarshal(facts, &saved); err != nil {
			return nil, err
		}
		if saved.NativeEvent != 0 {
			if err := loadTranscriptValueTx(ctx, tx, projectionID, saved.NativeEvent, &observation.NativeEventID); err != nil {
				return nil, err
			}
		}
		observation.Kind, observation.NativeEventDigest = saved.Kind, saved.NativeHash
		observation.Complete, observation.Outcome, observation.Capture = saved.Complete, saved.Outcome, saved.Capture
		observation.Metadata = saved.Metadata
		times, err := expandObservationTimes(saved.Times, scopeID, keyID)
		if err != nil {
			return nil, err
		}
		observation.Times, err = loadTranscriptTimesTx(ctx, tx, projectionID, times)
		if err != nil {
			return nil, err
		}
		result[itemID] = append(result[itemID], observation)
	}
	return result, rows.Err()
}

// ErrSavedTranscriptUnavailable reports that a thread has no active, healthy
// saved projection to prove anything against.
var ErrSavedTranscriptUnavailable = errors.New("saved transcript unavailable")

// ErrSavedReaderOutdated is a saved transcript an older reader built: not yet
// readable, and not an absence. It is ErrSavedTranscriptUnavailable too.
var ErrSavedReaderOutdated = fmt.Errorf("%w: built by an older reader", ErrSavedTranscriptUnavailable)

// healthyActiveProjection selects a thread's active projection only while its
// source is present and it has no error: the only projection saved rows can
// prove anything against.
const healthyActiveProjection = `cp.thread_id=? AND cp.state='active' AND t.source_status='present' AND cp.error_summary IS NULL`

// SavedUserKeys are exact provider identities recorded when prompts were
// delivered: native turn ids (Codex turns, Grok prompts) and native event ids
// (Claude's stamped row UUIDs). TurnKeys select the user rows of saved turns a
// prompt's own proof already decided; they are projection-scoped, so a read
// that names ProjectionKey fails when the active projection is another
// generation rather than mixing two.
//
// MaxRows, when set, bounds the rows a read may return: a read that finds
// more refuses with ErrSavedRowsOverflow before loading any of them.
type SavedUserKeys struct {
	NativeTurnIDs  []string
	NativeEventIDs []string
	TurnKeys       []string
	ProjectionKey  string
	MaxRows        int
}

// ErrSavedGenerationChanged reports that the saved projection a decision was
// made against is no longer the active one.
var ErrSavedGenerationChanged = errors.New("saved transcript generation changed")

// ErrSavedRowsOverflow reports a read that matched more rows than its bound.
var ErrSavedRowsOverflow = errors.New("saved user rows exceed the read bound")

// SavedUserRow is one saved user row with the status of its saved turn and
// the projection generation it was read from.
type SavedUserRow struct {
	Item          model.Item
	TurnStatus    TranscriptTurnStatus
	ProjectionKey string
}

// The saved user-row lookups. CROSS JOIN fixes the join order so each exact
// key drives its own index; with a plain JOIN the planner scans every item of
// the projection instead.
const (
	savedUserRowsByTurn = `SELECT i.item_id, t.status FROM conversation_turns t
CROSS JOIN conversation_items i ON i.projection_id=t.projection_id AND i.projected_seq BETWEEN t.start_seq AND t.end_seq AND i.turn_key=t.turn_key
WHERE t.projection_id=? AND t.native_turn_id=? AND i.item_type='user_message'`
	savedUserRowsByEvent = `SELECT i.item_id, t.status FROM conversation_observation_aliases a
CROSS JOIN conversation_observations o ON o.projection_id=a.projection_id AND o.key_id=a.observation_value_id
CROSS JOIN conversation_items i ON i.projection_id=o.projection_id AND i.source_line=o.source_line
CROSS JOIN conversation_turns t ON t.projection_id=i.projection_id AND t.turn_key=i.turn_key
WHERE a.projection_id=? AND a.namespace='native.eventId' AND a.value_id=? AND i.item_type='user_message'`
	savedUserRowsByTurnKey = `SELECT i.item_id, t.status FROM conversation_turns t
CROSS JOIN conversation_items i ON i.projection_id=t.projection_id AND i.turn_key=t.turn_key
WHERE t.projection_id=? AND t.turn_key=? AND i.item_type='user_message'`
)

// ReadSavedUserRows returns the user rows of the thread's active, healthy
// projection that carry one of the keys: the native turn id of their saved
// turn, or a native event id on one of their observations. Each key is one
// indexed lookup; no other row is read and no text is loaded. Keys only select
// candidates. The caller decides identity from the returned evidence.
func (s *Store) ReadSavedUserRows(ctx context.Context, threadID string, keys SavedUserKeys) ([]SavedUserRow, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var projectionID int64
	var projectionKey, providerName, nativeThreadID, sourceLabel string
	var readerCurrent bool
	claudeParser := CurrentTranscriptParserVersion(ProviderClaude)
	err = tx.QueryRowContext(ctx, `
SELECT cp.id, cp.projection_key, t.provider, t.native_thread_id, sf.canonical_path, `+projectionReaderCurrent("?", "?")+`
FROM conversation_projections cp
JOIN threads t ON t.id=cp.thread_id
JOIN source_files sf ON sf.id=cp.source_file_id
WHERE `+healthyActiveProjection, claudeParser, claudeParser, CurrentTranscriptCompletionRuleVersion(ProviderClaude), threadID).Scan(&projectionID, &projectionKey, &providerName, &nativeThreadID, &sourceLabel, &readerCurrent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSavedTranscriptUnavailable
	}
	if err != nil {
		return nil, err
	}
	// A Claude prompt's answer is its own Claude turn's, and only the current
	// reader groups saved turns that way: a projection an older reader built
	// is read again by the upgrade backfill, whose commit retries the read,
	// unless the upgrade proved its exact source unaffected.
	if Provider(providerName) == ProviderClaude && !readerCurrent {
		return nil, ErrSavedReaderOutdated
	}
	if keys.ProjectionKey != "" && keys.ProjectionKey != projectionKey {
		return nil, ErrSavedGenerationChanged
	}
	status := map[string]TranscriptTurnStatus{}
	collect := func(query string, args ...any) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var itemID string
			var turnStatus TranscriptTurnStatus
			if err := rows.Scan(&itemID, &turnStatus); err != nil {
				return err
			}
			status[itemID] = turnStatus
		}
		return rows.Err()
	}
	for _, turnID := range keys.NativeTurnIDs {
		if err := collect(savedUserRowsByTurn, projectionID, turnID); err != nil {
			return nil, err
		}
	}
	for _, turnKey := range keys.TurnKeys {
		query, args := savedUserRowsByTurnKey, []any{projectionID, turnKey}
		if keys.MaxRows > 0 {
			query, args = query+` LIMIT ?`, append(args, keys.MaxRows+1)
		}
		if err := collect(query, args...); err != nil {
			return nil, err
		}
	}
	for _, eventID := range keys.NativeEventIDs {
		// The same value the indexer interned for this event's native.eventId alias.
		raw, _ := json.Marshal(eventID)
		scalar, ok := transcriptNativeScalar(raw)
		if !ok {
			continue
		}
		value, err := findTranscriptValueTx(ctx, tx, projectionID, scalar)
		if err != nil {
			return nil, err
		}
		if value == 0 {
			continue
		}
		if err := collect(savedUserRowsByEvent, projectionID, value); err != nil {
			return nil, err
		}
	}
	if keys.MaxRows > 0 && len(status) > keys.MaxRows {
		return nil, ErrSavedRowsOverflow
	}
	result := make([]SavedUserRow, 0, len(status))
	provider := model.Provider(providerName)
	for itemID, turnStatus := range status {
		var raw []byte
		var item model.Item
		var itemType, role string
		var synthetic, sidechain bool
		err := tx.QueryRowContext(ctx, `SELECT parsed_json, source_key, projected_seq, turn_key, turn_ordinal, item_type, COALESCE(role,''), synthetic, sidechain
FROM conversation_items WHERE projection_id=? AND item_id=?`, projectionID, itemID).Scan(&raw, &item.SourceKey, &item.SourceSeq, &item.TurnKey, &item.TurnOrdinal, &itemType, &role, &synthetic, &sidechain)
		if err != nil {
			return nil, err
		}
		parsed, err := decodeParsedItemTx(ctx, tx, projectionID, raw)
		if err != nil {
			return nil, err
		}
		observations, err := loadItemObservationMap(ctx, tx, projectionID, `WHERE m.projection_id=? AND m.item_id=?`, projectionID, itemID)
		if err != nil {
			return nil, err
		}
		if parsed.ID == "" {
			parsed.ID = item.SourceKey
		}
		parsed.SourceKey, parsed.SourceSeq = item.SourceKey, item.SourceSeq
		parsed.TurnKey, parsed.TurnOrdinal, parsed.Scope.TurnKey = item.TurnKey, item.TurnOrdinal, item.TurnKey
		parsed.Type, parsed.Role = model.ItemType(itemType), model.Role(role)
		parsed.Synthetic, parsed.Sidechain = synthetic, sidechain
		parsed.Observations = observations[itemID]
		completeSavedItemScope(&parsed, provider, sourceLabel, projectionKey, nativeThreadID)
		// Claude starts a root turn only after the last one ends, so a saved
		// root turn a later root turn follows is over. One that saved no end of
		// its own was cut off when its process stopped (a reboot, a crash):
		// interrupted.
		if provider == model.ProviderClaude && !sidechain && (turnStatus == TurnInProgress || turnStatus == TurnUnknown) {
			var newer bool
			if err := tx.QueryRowContext(ctx, newerRootTurnQuery, projectionID, item.TurnOrdinal).Scan(&newer); err != nil {
				return nil, err
			}
			if newer {
				turnStatus = TurnInterrupted
			}
		}
		result = append(result, SavedUserRow{Item: parsed, TurnStatus: turnStatus, ProjectionKey: projectionKey})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Item.SourceSeq < result[j].Item.SourceSeq })
	return result, tx.Commit()
}

// completeSavedItemScope fills a saved item's scope from the projection it
// was read from, where the stored item left it empty.
func completeSavedItemScope(item *model.Item, provider model.Provider, sourceLabel, projectionKey, nativeThreadID string) {
	if item.Scope.Kind == "" {
		item.Scope.Kind = model.ScopeItem
	}
	if item.Scope.Provider == "" {
		item.Scope.Provider = provider
	}
	if item.Scope.Source == "" {
		item.Scope.Source = sourceLabel
	}
	if item.Scope.Incarnation == "" {
		item.Scope.Incarnation = projectionKey
	}
	if item.Scope.ItemID == "" {
		item.Scope.ItemID = item.ID
	}
	if item.Scope.NativeThreadID == nil && nativeThreadID != "" {
		native := nativeThreadID
		item.Scope.NativeThreadID = &native
	}
}

// SavedTurnFinal is the catalog's own final answer row of one saved turn.
type SavedTurnFinal struct {
	ItemID       string
	NativeItemID string
	Text         string
	Truncated    bool
}

// ReadSavedTurnFinal returns the final answer row the catalog associated with
// one saved turn of the given projection generation: the turn's own
// final_item_id, a root agent message. A turn without one, or a row of another
// type or scope, reports ErrTranscriptBodyUnavailable; the caller compares
// NativeItemID with the answer it expects (Grok instead uses the exact saved
// ItemID because it has no native message identity) and reads a truncated body through
// LoadTranscriptBody.
func (s *Store) ReadSavedTurnFinal(ctx context.Context, threadID, projectionKey, turnKey string) (SavedTurnFinal, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SavedTurnFinal{}, err
	}
	defer tx.Rollback()
	var projectionID int64
	var activeKey, provider string
	err = tx.QueryRowContext(ctx, `
SELECT cp.id, cp.projection_key, t.provider
FROM conversation_projections cp
JOIN threads t ON t.id=cp.thread_id
WHERE `+healthyActiveProjection, threadID).Scan(&projectionID, &activeKey, &provider)
	if errors.Is(err, sql.ErrNoRows) {
		return SavedTurnFinal{}, ErrSavedTranscriptUnavailable
	}
	if err != nil {
		return SavedTurnFinal{}, err
	}
	if activeKey != projectionKey {
		return SavedTurnFinal{}, ErrSavedGenerationChanged
	}
	var final SavedTurnFinal
	var itemType string
	var native sql.NullString
	var sidechain, truncated bool
	err = tx.QueryRowContext(ctx, `SELECT i.item_id, i.native_item_id, i.display_text, i.content_truncated, i.item_type, i.sidechain
FROM conversation_turns t
CROSS JOIN conversation_items i ON i.projection_id=t.projection_id AND i.item_id=t.final_item_id
WHERE t.projection_id=? AND t.turn_key=?`, projectionID, turnKey).Scan(&final.ItemID, &native, &final.Text, &truncated, &itemType, &sidechain)
	if errors.Is(err, sql.ErrNoRows) {
		return SavedTurnFinal{}, ErrTranscriptBodyUnavailable
	}
	if err != nil {
		return SavedTurnFinal{}, err
	}
	if itemType != string(model.ItemAgentMessage) || sidechain || provider != "grok" && (!native.Valid || native.String == "") {
		return SavedTurnFinal{}, ErrTranscriptBodyUnavailable
	}
	final.NativeItemID, final.Truncated = native.String, truncated
	return final, tx.Commit()
}

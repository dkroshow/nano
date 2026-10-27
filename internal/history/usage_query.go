package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	DefaultThreadUsageTurnLimit = 100
	MaxThreadUsageTurnLimit     = 500
	DefaultThreadUsageListLimit = 100
	MaxThreadUsageListLimit     = 250
)

type ThreadUsageMessage struct {
	ItemID    string             `json:"itemId"`
	Type      TranscriptItemType `json:"type"`
	Role      TranscriptRole     `json:"role,omitempty"`
	Text      string             `json:"text,omitempty"`
	CreatedAt *time.Time         `json:"createdAt,omitempty"`
}

type ThreadUsageTurn struct {
	TurnKey        string                `json:"turnKey"`
	NativeTurnID   string                `json:"nativeTurnId,omitempty"`
	Ordinal        int                   `json:"ordinal"`
	StartedAt      *time.Time            `json:"startedAt,omitempty"`
	CompletedAt    *time.Time            `json:"completedAt,omitempty"`
	DurationMS     int64                 `json:"durationMs,omitempty"`
	Model          string                `json:"model,omitempty"`
	Effort         string                `json:"effort,omitempty"`
	ServiceTier    string                `json:"serviceTier,omitempty"`
	Usage          *TranscriptTokenUsage `json:"usage,omitempty"`
	ItemTypeCounts map[string]int        `json:"itemTypeCounts,omitempty"`
	Messages       []ThreadUsageMessage  `json:"messages"`
}

type ThreadUsagePage struct {
	ThreadID       string            `json:"threadId"`
	Provider       Provider          `json:"provider"`
	NativeThreadID string            `json:"nativeThreadId"`
	ParserVersion  string            `json:"parserVersion,omitempty"`
	UsageContract  *int              `json:"usageContract,omitempty"`
	Turns          []ThreadUsageTurn `json:"turns"`
	HasMoreAfter   bool              `json:"hasMoreAfter"`
}

type ThreadUsageTotals struct {
	ThreadID         string     `json:"threadId"`
	Provider         Provider   `json:"provider"`
	NativeThreadID   string     `json:"nativeThreadId"`
	Title            string     `json:"title,omitempty"`
	UpdatedAt        *time.Time `json:"updatedAt,omitempty"`
	TurnsTotal       int64      `json:"turnsTotal"`
	TurnsWithUsage   int64      `json:"turnsWithUsage"`
	InputTokens      *int64     `json:"inputTokens,omitempty"`
	OutputTokens     *int64     `json:"outputTokens,omitempty"`
	ReasoningTokens  *int64     `json:"reasoningTokens,omitempty"`
	CacheReadTokens  *int64     `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens *int64     `json:"cacheWriteTokens,omitempty"`
}

type ThreadUsageListQuery struct {
	Provider Provider
	Kind     ThreadKind
	Archived ArchivedFilter
	Limit    int
	AfterID  string
}

type ThreadUsageListPage struct {
	Items      []ThreadUsageTotals `json:"items"`
	NextCursor string              `json:"nextCursor,omitempty"`
}

func (s *Store) ThreadTurnUsage(ctx context.Context, threadID string, afterOrdinal, limit int) (ThreadUsagePage, error) {
	if limit <= 0 {
		limit = DefaultThreadUsageTurnLimit
	}
	if limit > MaxThreadUsageTurnLimit {
		limit = MaxThreadUsageTurnLimit
	}
	page := ThreadUsagePage{ThreadID: threadID, Turns: []ThreadUsageTurn{}}
	var contract sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
SELECT t.provider, t.native_thread_id, COALESCE(cp.parser_version, ''),
       (SELECT MAX(ct.usage_contract) FROM conversation_turns ct
        WHERE ct.projection_id = cp.id)
FROM threads t
LEFT JOIN conversation_projections cp ON cp.thread_id = t.id AND cp.state = 'active'
WHERE t.id = ?`, threadID).Scan(&page.Provider, &page.NativeThreadID, &page.ParserVersion, &contract)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadUsagePage{}, ErrThreadNotFound
	}
	if err != nil {
		return ThreadUsagePage{}, fmt.Errorf("read thread usage identity: %w", err)
	}
	if contract.Valid {
		value := int(contract.Int64)
		page.UsageContract = &value
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT turn_key, native_turn_id, turn_ordinal, started_at, completed_at, duration_ms,
       input_tokens, output_tokens, reasoning_tokens, cache_read_tokens, cache_write_tokens,
       native_total_tokens, context_window, model, effort, service_tier, native_json, usage_contract
FROM thread_turn_usage
WHERE thread_id = ? AND turn_ordinal > ?
ORDER BY turn_ordinal
LIMIT ?`, threadID, afterOrdinal, limit+1)
	if err != nil {
		return ThreadUsagePage{}, fmt.Errorf("read thread turn usage: %w", err)
	}
	defer rows.Close()
	var turns []ThreadUsageTurn
	for rows.Next() {
		turn, err := scanUsageTurn(rows)
		if err != nil {
			return ThreadUsagePage{}, err
		}
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		return ThreadUsagePage{}, err
	}
	if len(turns) > limit {
		page.HasMoreAfter = true
		turns = turns[:limit]
	}
	if err := s.attachUsageTurnDetails(ctx, threadID, turns); err != nil {
		return ThreadUsagePage{}, err
	}
	page.Turns = turns
	return page, nil
}

func (s *Store) attachUsageTurnDetails(ctx context.Context, threadID string, turns []ThreadUsageTurn) error {
	if len(turns) == 0 {
		return nil
	}
	keys := make([]string, len(turns))
	indexByKey := make(map[string]int, len(turns))
	for i, turn := range turns {
		keys[i] = turn.TurnKey
		indexByKey[turn.TurnKey] = i
		turns[i].Messages = []ThreadUsageMessage{}
		turns[i].ItemTypeCounts = map[string]int{}
	}
	placeholders := strings.Repeat("?,", len(keys))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(keys)+2)
	args = append(args, threadID, string(ThreadSubagent))
	for _, key := range keys {
		args = append(args, key)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT ci.turn_key, ci.item_id, ci.item_type, COALESCE(ci.role, ''), ci.display_text, ci.created_at
FROM conversation_items ci
JOIN conversation_projections cp ON cp.id = ci.projection_id AND cp.state = 'active'
JOIN threads t ON t.id = cp.thread_id
WHERE t.id = ? AND (ci.sidechain = 0 OR t.kind = ?) AND ci.turn_key IN (`+placeholders+`)
ORDER BY ci.projected_seq`, args...)
	if err != nil {
		return fmt.Errorf("read thread usage messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, itemID, itemType, role, text string
		var created sql.NullString
		if err := rows.Scan(&key, &itemID, &itemType, &role, &text, &created); err != nil {
			return err
		}
		index, ok := indexByKey[key]
		if !ok {
			continue
		}
		turns[index].ItemTypeCounts[itemType]++
		if itemType != string(ItemUserMessage) && itemType != string(ItemAgentMessage) {
			continue
		}
		createdAt, err := parseNullTime(created)
		if err != nil {
			return err
		}
		turns[index].Messages = append(turns[index].Messages, ThreadUsageMessage{
			ItemID: itemID, Type: TranscriptItemType(itemType), Role: TranscriptRole(role),
			Text: text, CreatedAt: createdAt,
		})
	}
	return rows.Err()
}

func (s *Store) ListThreadUsageTotals(ctx context.Context, query ThreadUsageListQuery) (ThreadUsageListPage, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = DefaultThreadUsageListLimit
	}
	if limit > MaxThreadUsageListLimit {
		limit = MaxThreadUsageListLimit
	}
	archived := query.Archived
	if archived == "" {
		archived = ArchivedExclude
	}
	args := []any{}
	where := []string{"1=1"}
	if query.Provider != "" {
		where = append(where, "t.provider = ?")
		args = append(args, query.Provider)
	}
	if query.Kind != "" {
		where = append(where, "t.kind = ?")
		args = append(args, query.Kind)
	}
	switch archived {
	case ArchivedExclude:
		where = append(where, "t.archived = 0")
	case ArchivedOnly:
		where = append(where, "t.archived = 1")
	}
	if query.AfterID != "" {
		where = append(where, "(t.updated_at < (SELECT updated_at FROM threads WHERE id = ?) OR (t.updated_at = (SELECT updated_at FROM threads WHERE id = ?) AND t.id < ?))")
		args = append(args, query.AfterID, query.AfterID, query.AfterID)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `
SELECT t.id, t.provider, t.native_thread_id, COALESCE(t.title, ''), t.updated_at,
       COUNT(u.turn_key),
       COUNT(CASE WHEN u.input_tokens IS NOT NULL OR u.output_tokens IS NOT NULL
                    OR u.reasoning_tokens IS NOT NULL OR u.cache_read_tokens IS NOT NULL
                    OR u.cache_write_tokens IS NOT NULL THEN 1 END),
       SUM(u.input_tokens), SUM(u.output_tokens), SUM(u.reasoning_tokens),
       SUM(u.cache_read_tokens), SUM(u.cache_write_tokens)
FROM threads t
LEFT JOIN thread_turn_usage u ON u.thread_id = t.id
WHERE `+strings.Join(where, " AND ")+`
GROUP BY t.id
ORDER BY t.updated_at DESC, t.id DESC
LIMIT ?`, args...)
	if err != nil {
		return ThreadUsageListPage{}, fmt.Errorf("list thread usage totals: %w", err)
	}
	defer rows.Close()
	page := ThreadUsageListPage{Items: []ThreadUsageTotals{}}
	for rows.Next() {
		var item ThreadUsageTotals
		var title string
		var updated sql.NullString
		var input, output, reasoning, cacheRead, cacheWrite sql.NullInt64
		if err := rows.Scan(&item.ThreadID, &item.Provider, &item.NativeThreadID, &title, &updated,
			&item.TurnsTotal, &item.TurnsWithUsage, &input, &output, &reasoning, &cacheRead, &cacheWrite); err != nil {
			return ThreadUsageListPage{}, err
		}
		item.Title = title
		var parseErr error
		item.UpdatedAt, parseErr = parseNullTime(updated)
		if parseErr != nil {
			return ThreadUsageListPage{}, parseErr
		}
		item.InputTokens = int64PointerFromNull(input)
		item.OutputTokens = int64PointerFromNull(output)
		item.ReasoningTokens = int64PointerFromNull(reasoning)
		item.CacheReadTokens = int64PointerFromNull(cacheRead)
		item.CacheWriteTokens = int64PointerFromNull(cacheWrite)
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return ThreadUsageListPage{}, err
	}
	if len(page.Items) > limit {
		page.NextCursor = page.Items[limit-1].ThreadID
		page.Items = page.Items[:limit]
	}
	return page, nil
}

func scanUsageTurn(rows *sql.Rows) (ThreadUsageTurn, error) {
	var turn ThreadUsageTurn
	var nativeID, started, completed, model, effort, serviceTier sql.NullString
	var duration, input, output, reasoning, cacheRead, cacheWrite, total, contextWindow, contract sql.NullInt64
	var native []byte
	if err := rows.Scan(&turn.TurnKey, &nativeID, &turn.Ordinal, &started, &completed, &duration,
		&input, &output, &reasoning, &cacheRead, &cacheWrite, &total, &contextWindow, &model, &effort, &serviceTier,
		&native, &contract); err != nil {
		return ThreadUsageTurn{}, err
	}
	turn.NativeTurnID = stringFromNull(nativeID)
	turn.Model = stringFromNull(model)
	turn.Effort = stringFromNull(effort)
	turn.ServiceTier = stringFromNull(serviceTier)
	var err error
	turn.StartedAt, err = parseNullTime(started)
	if err != nil {
		return ThreadUsageTurn{}, err
	}
	turn.CompletedAt, err = parseNullTime(completed)
	if err != nil {
		return ThreadUsageTurn{}, err
	}
	turn.DurationMS = int64FromNull(duration)
	turn.Usage = scanTurnUsage(input, sql.NullInt64{}, output, reasoning, total, contextWindow, cacheRead, cacheWrite, native, contract)
	return turn, nil
}

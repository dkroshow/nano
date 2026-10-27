package history

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	ConversationSearchMessageGroup  = "message"
	ConversationSearchThreadGroup   = "thread"
	maxConversationSearchQueryBytes = 4096
	maxConversationSearchLimit      = 100
)

var (
	ErrConversationSearchInvalidCursor = errors.New("invalid conversation search cursor")
	ErrConversationSearchChanged       = errors.New("conversation search changed")
)

// ConversationSearchUnavailableError says why a requested mode cannot serve a
// complete corpus. It is intentionally typed so the HTTP layer can return the
// documented rebuilding response without treating it as a catalog failure.
type ConversationSearchUnavailableError struct{ Status ConversationSearchStatus }

func (e *ConversationSearchUnavailableError) Error() string {
	return "conversation search index is not ready"
}

type ConversationSearchQuery struct {
	Query     string
	Mode      ConversationSearchMode
	Since     *time.Time
	Until     *time.Time
	Roles     []TranscriptRole
	Providers []Provider
	Kinds     []ThreadKind
	Group     string
	Limit     int
	Cursor    string
}

type ConversationSearchItem struct {
	ThreadID          string             `json:"threadId"`
	Provider          Provider           `json:"provider"`
	NativeThreadID    string             `json:"nativeThreadId"`
	Kind              ThreadKind         `json:"kind"`
	ItemID            string             `json:"itemId"`
	Role              TranscriptRole     `json:"role"`
	ItemType          TranscriptItemType `json:"itemType"`
	CreatedAt         *time.Time         `json:"createdAt"`
	Snippet           string             `json:"snippet"`
	RetainedTruncated bool               `json:"retainedTruncated"`
	MatchCount        int                `json:"matchCount,omitempty"`
	score             float64
	text              string
}

type ConversationSearchPage struct {
	Mode           ConversationSearchMode   `json:"mode"`
	Group          string                   `json:"group"`
	SearchRevision int64                    `json:"searchRevision"`
	Items          []ConversationSearchItem `json:"items"`
	NextCursor     *string                  `json:"nextCursor"`
}

// NormalizeConversationSearchQuery validates the provider-neutral contract and
// supplies its defaults for HTTP and future internal callers alike.
func NormalizeConversationSearchQuery(query ConversationSearchQuery) (ConversationSearchQuery, error) {
	if err := validateConversationSearchQuery(&query); err != nil {
		return ConversationSearchQuery{}, err
	}
	return query, nil
}

type conversationSearchCursor struct {
	Version  int     `json:"v"`
	Hash     string  `json:"h"`
	Revision int64   `json:"r"`
	Score    float64 `json:"s"`
	Created  *string `json:"c"`
	ThreadID string  `json:"t"`
	ItemID   string  `json:"i"`
}

func (s *Store) SearchConversationItems(ctx context.Context, query ConversationSearchQuery) (ConversationSearchPage, error) {
	var err error
	query, err = NormalizeConversationSearchQuery(query)
	if err != nil {
		return ConversationSearchPage{}, err
	}
	hash, err := conversationSearchQueryHash(query)
	if err != nil {
		return ConversationSearchPage{}, err
	}
	cursor, err := decodeConversationSearchCursor(query.Cursor)
	if err != nil {
		return ConversationSearchPage{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ConversationSearchPage{}, err
	}
	defer tx.Rollback()
	statuses, err := conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return ConversationSearchPage{}, err
	}
	status, ok := searchStatusForMode(statuses, query.Mode)
	if !ok || status.State != ConversationSearchReady {
		return ConversationSearchPage{}, &ConversationSearchUnavailableError{Status: status}
	}
	if cursor != nil && (cursor.Hash != hash || cursor.Revision != status.Revision) {
		if cursor.Hash != hash {
			return ConversationSearchPage{}, ErrConversationSearchInvalidCursor
		}
		return ConversationSearchPage{}, ErrConversationSearchChanged
	}
	page := ConversationSearchPage{Mode: query.Mode, Group: query.Group, SearchRevision: status.Revision, Items: []ConversationSearchItem{}}
	if query.Group == ConversationSearchThreadGroup {
		// Exact grouped matchCount and representative selection require every
		// verified hit, unlike the bounded default message path below.
		items, err := collectConversationSearchCandidatesTx(ctx, tx, query)
		if err != nil {
			return ConversationSearchPage{}, err
		}
		if err := tx.Commit(); err != nil {
			return ConversationSearchPage{}, err
		}
		items = groupConversationSearchItems(items, query.Mode)
		sortConversationSearchItems(items, query.Mode)
		if cursor != nil {
			var found bool
			items, found = conversationSearchItemsAfter(items, query.Mode, *cursor)
			if !found {
				return ConversationSearchPage{}, ErrConversationSearchInvalidCursor
			}
		}
		if len(items) > query.Limit {
			items = items[:query.Limit]
			last := items[len(items)-1]
			encoded, encodeErr := encodeConversationSearchCursor(conversationSearchCursorFor(last, status.Revision, hash))
			if encodeErr != nil {
				return ConversationSearchPage{}, encodeErr
			}
			page.NextCursor = &encoded
		}
		page.Items = items
		return page, nil
	}
	items, hasNext, foundCursor, err := streamConversationSearchMessagePageTx(ctx, tx, query, cursor)
	if err != nil {
		return ConversationSearchPage{}, err
	}
	if err := tx.Commit(); err != nil {
		return ConversationSearchPage{}, err
	}
	if cursor != nil && !foundCursor {
		return ConversationSearchPage{}, ErrConversationSearchInvalidCursor
	}
	if hasNext {
		last := items[len(items)-1]
		encoded, err := encodeConversationSearchCursor(conversationSearchCursorFor(last, status.Revision, hash))
		if err != nil {
			return ConversationSearchPage{}, err
		}
		page.NextCursor = &encoded
	}
	page.Items = items
	return page, nil
}

func (s *Store) ConversationSearchStatus(ctx context.Context) ([]ConversationSearchStatus, error) {
	return s.ConversationSearchStatuses(ctx)
}

func validateConversationSearchQuery(query *ConversationSearchQuery) error {
	if query.Query == "" || len(query.Query) > maxConversationSearchQueryBytes || !utf8.ValidString(query.Query) {
		return errors.New("q must be valid UTF-8 and between 1 and 4096 bytes")
	}
	if query.Mode != ConversationSearchLiteral && query.Mode != ConversationSearchTerms {
		return errors.New("mode must be literal or terms")
	}
	if query.Group == "" {
		query.Group = ConversationSearchMessageGroup
	}
	if query.Group != ConversationSearchMessageGroup && query.Group != ConversationSearchThreadGroup {
		return errors.New("group must be message or thread")
	}
	if query.Limit == 0 {
		query.Limit = 20
	}
	if query.Limit < 1 || query.Limit > maxConversationSearchLimit {
		return fmt.Errorf("limit must be between 1 and %d", maxConversationSearchLimit)
	}
	if query.Since != nil && query.Until != nil && !query.Since.Before(*query.Until) {
		return errors.New("since must be before until")
	}
	if query.Mode == ConversationSearchLiteral && utf8.RuneCountInString(query.Query) < 3 {
		return errors.New("literal q must contain at least three Unicode characters")
	}
	if query.Mode == ConversationSearchTerms && len(conversationSearchTokens(query.Query)) == 0 {
		return errors.New("terms q must contain at least one Unicode word token")
	}
	for _, role := range query.Roles {
		if role != RoleUser && role != RoleAssistant {
			return errors.New("role must be user or assistant")
		}
	}
	for _, provider := range query.Providers {
		if provider != ProviderCodex && provider != ProviderClaude && provider != ProviderGrok {
			return errors.New("unsupported provider")
		}
	}
	for _, kind := range query.Kinds {
		if kind != ThreadMain && kind != ThreadSubagent && kind != ThreadFork {
			return errors.New("kind must be main, subagent, or fork")
		}
	}
	query.Roles = canonicalSearchRoles(query.Roles)
	query.Providers = canonicalSearchProviders(query.Providers)
	query.Kinds = canonicalSearchKinds(query.Kinds)
	return nil
}

func visitConversationSearchCandidatesTx(ctx context.Context, tx *sql.Tx, query ConversationSearchQuery, visit func(ConversationSearchItem) (bool, error)) error {
	table := searchFTSTable(query.Mode)
	where := []string{"csd.document_id = " + table + ".rowid"}
	args := []any{conversationSearchFTSQuery(query)}
	where = append(where, table+" MATCH ?", "cp.state = 'active'")
	if query.Since != nil {
		where, args = append(where, "ci.created_at >= ?"), append(args, formatTime(*query.Since))
	}
	if query.Until != nil {
		where, args = append(where, "ci.created_at < ?"), append(args, formatTime(*query.Until))
	}
	if query.Since != nil || query.Until != nil {
		where = append(where, "ci.created_at IS NOT NULL")
	}
	appendSearchFilter := func(column string, values []string) {
		if len(values) == 0 {
			return
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")
		where = append(where, column+" IN ("+marks+")")
		for _, value := range values {
			args = append(args, value)
		}
	}
	roles := make([]string, len(query.Roles))
	for i := range query.Roles {
		roles[i] = string(query.Roles[i])
	}
	providers := make([]string, len(query.Providers))
	for i := range query.Providers {
		providers[i] = string(query.Providers[i])
	}
	kinds := make([]string, len(query.Kinds))
	for i := range query.Kinds {
		kinds[i] = string(query.Kinds[i])
	}
	appendSearchFilter("ci.role", roles)
	appendSearchFilter("t.provider", providers)
	appendSearchFilter("t.kind", kinds)
	rows, err := tx.QueryContext(ctx, `
SELECT csd.document_id, t.id, t.provider, t.native_thread_id, t.kind,
       ci.item_id, ci.role, ci.item_type, ci.created_at, ci.content_truncated,
       CASE WHEN cib.item_id IS NULL THEN 0 ELSE 1 END, cib.body_text,
       COALESCE(cib.truncated, 0), ci.display_text, `+searchConversationScoreSQL(query.Mode, table)+`
FROM `+table+`
JOIN conversation_search_documents csd ON csd.document_id = `+table+`.rowid
JOIN conversation_projections cp ON cp.id = csd.projection_id
JOIN threads t ON t.id = cp.thread_id
JOIN conversation_items ci ON ci.projection_id = csd.projection_id AND ci.item_id = csd.item_id
LEFT JOIN conversation_item_bodies cib ON cib.projection_id = ci.projection_id AND cib.item_id = ci.item_id
WHERE `+strings.Join(where, " AND ")+searchConversationOrderSQL(query.Mode, table), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var documentID int64
		var provider, kind, role, itemType string
		var created, body sql.NullString
		var bodyPresent, contentTruncated, bodyTruncated int
		var score float64
		var item ConversationSearchItem
		if err := rows.Scan(&documentID, &item.ThreadID, &provider, &item.NativeThreadID, &kind, &item.ItemID, &role, &itemType, &created, &contentTruncated, &bodyPresent, &body, &bodyTruncated, &item.Snippet, &score); err != nil {
			return err
		}
		item.Provider, item.Kind, item.Role, item.ItemType, item.score = Provider(provider), ThreadKind(kind), TranscriptRole(role), TranscriptItemType(itemType), score
		if bodyPresent != 0 {
			item.text = stringFromNull(body)
		} else {
			item.text = item.Snippet
		}
		item.RetainedTruncated = contentTruncated != 0 || bodyTruncated != 0
		if item.CreatedAt, err = parseNullTime(created); err != nil {
			return err
		}
		if query.Mode == ConversationSearchLiteral {
			if !strings.Contains(item.text, query.Query) {
				continue
			}
			item.Snippet = conversationSearchSnippet(item.text, query.Query, false)
		} else {
			if !conversationSearchHasAllTokens(item.text, conversationSearchTokens(query.Query)) {
				continue
			}
			item.Snippet = conversationSearchSnippet(item.text, conversationSearchTokens(query.Query)[0], true)
		}
		stop, err := visit(item)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	return rows.Err()
}

func collectConversationSearchCandidatesTx(ctx context.Context, tx *sql.Tx, query ConversationSearchQuery) ([]ConversationSearchItem, error) {
	items := []ConversationSearchItem{}
	err := visitConversationSearchCandidatesTx(ctx, tx, query, func(item ConversationSearchItem) (bool, error) { items = append(items, item); return false, nil })
	return items, err
}

func streamConversationSearchMessagePageTx(ctx context.Context, tx *sql.Tx, query ConversationSearchQuery, cursor *conversationSearchCursor) ([]ConversationSearchItem, bool, bool, error) {
	items := make([]ConversationSearchItem, 0, query.Limit)
	foundCursor := cursor == nil
	hasNext := false
	err := visitConversationSearchCandidatesTx(ctx, tx, query, func(item ConversationSearchItem) (bool, error) {
		if !foundCursor {
			if conversationSearchItemMatchesCursor(item, query.Mode, *cursor) {
				foundCursor = true
			}
			return false, nil
		}
		if len(items) == query.Limit {
			hasNext = true
			return true, nil
		}
		items = append(items, item)
		return false, nil
	})
	return items, hasNext, foundCursor, err
}

func searchConversationScoreSQL(mode ConversationSearchMode, table string) string {
	if mode == ConversationSearchTerms {
		return "bm25(" + table + ")"
	}
	return "0.0"
}

// The SQL ordering deliberately matches compareConversationSearchItems so the
// default message path can stop after one verified look-ahead result instead
// of collecting and sorting the entire candidate set.
func searchConversationOrderSQL(mode ConversationSearchMode, table string) string {
	order := " ORDER BY "
	if mode == ConversationSearchTerms {
		order += "bm25(" + table + ") ASC, "
	}
	return order + "ci.created_at IS NULL ASC, ci.created_at DESC, t.id ASC, ci.item_id ASC"
}
func conversationSearchFTSQuery(query ConversationSearchQuery) string {
	quote := func(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
	if query.Mode == ConversationSearchLiteral {
		return quote(query.Query)
	}
	parts := conversationSearchTokens(query.Query)
	for i := range parts {
		parts[i] = quote(parts[i])
	}
	return strings.Join(parts, " AND ")
}

func conversationSearchTokens(value string) []string {
	var tokens []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			tokens = append(tokens, strings.ToLower(string(current)))
			current = nil
		}
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			current = append(current, r)
		} else {
			flush()
		}
	}
	flush()
	return tokens
}
func conversationSearchHasAllTokens(text string, want []string) bool {
	got := map[string]bool{}
	for _, token := range conversationSearchTokens(text) {
		got[token] = true
	}
	for _, token := range want {
		if !got[token] {
			return false
		}
	}
	return true
}

func conversationSearchSnippet(text, needle string, fold bool) string {
	runes := []rune(text)
	index := 0
	if fold {
		// Unicode case folding can change encoded byte length (for example,
		// U+0130 becomes ASCII i). Locate the word in original rune
		// coordinates rather than slicing with an offset from a folded string.
		want := strings.ToLower(needle)
		for i := 0; i < len(runes); {
			if !unicode.IsLetter(runes[i]) && !unicode.IsNumber(runes[i]) {
				i++
				continue
			}
			start := i
			for i < len(runes) && (unicode.IsLetter(runes[i]) || unicode.IsNumber(runes[i])) {
				i++
			}
			if strings.ToLower(string(runes[start:i])) == want {
				index = start
				break
			}
		}
	} else {
		if byteIndex := strings.Index(text, needle); byteIndex > 0 {
			index = utf8.RuneCountInString(text[:byteIndex])
		}
	}
	if len(runes) <= 512 {
		return text
	}
	start := index - 240
	if start < 0 {
		start = 0
	}
	end := start + 510
	if end > len(runes) {
		end = len(runes)
		start = end - 510
	}
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(runes) {
		suffix = "…"
	}
	return prefix + string(runes[start:end]) + suffix
}

func sortConversationSearchItems(items []ConversationSearchItem, mode ConversationSearchMode) {
	sort.SliceStable(items, func(i, j int) bool { return compareConversationSearchItems(items[i], items[j], mode) < 0 })
}
func compareConversationSearchItems(a, b ConversationSearchItem, mode ConversationSearchMode) int {
	if mode == ConversationSearchTerms && a.score != b.score {
		if a.score < b.score {
			return -1
		}
		return 1
	}
	if a.CreatedAt == nil || b.CreatedAt == nil {
		if a.CreatedAt != nil {
			return -1
		}
		if b.CreatedAt != nil {
			return 1
		}
	} else if !a.CreatedAt.Equal(*b.CreatedAt) {
		if a.CreatedAt.After(*b.CreatedAt) {
			return -1
		}
		return 1
	}
	if a.ThreadID != b.ThreadID {
		if a.ThreadID < b.ThreadID {
			return -1
		}
		return 1
	}
	if a.ItemID < b.ItemID {
		return -1
	}
	if a.ItemID > b.ItemID {
		return 1
	}
	return 0
}
func groupConversationSearchItems(items []ConversationSearchItem, mode ConversationSearchMode) []ConversationSearchItem {
	byThread := map[string]ConversationSearchItem{}
	for _, item := range items {
		existing, ok := byThread[item.ThreadID]
		if !ok {
			item.MatchCount = 1
			byThread[item.ThreadID] = item
			continue
		}
		existing.MatchCount++
		if compareConversationSearchItems(item, existing, mode) < 0 {
			item.MatchCount = existing.MatchCount
			byThread[item.ThreadID] = item
		} else {
			byThread[item.ThreadID] = existing
		}
	}
	result := make([]ConversationSearchItem, 0, len(byThread))
	for _, item := range byThread {
		result = append(result, item)
	}
	return result
}
func conversationSearchItemsAfter(items []ConversationSearchItem, mode ConversationSearchMode, cursor conversationSearchCursor) ([]ConversationSearchItem, bool) {
	for i, item := range items {
		if conversationSearchItemMatchesCursor(item, mode, cursor) {
			return items[i+1:], true
		}
	}
	return nil, false
}

func conversationSearchItemMatchesCursor(item ConversationSearchItem, mode ConversationSearchMode, cursor conversationSearchCursor) bool {
	itemCursor := conversationSearchCursorFor(item, cursor.Revision, cursor.Hash)
	return itemCursor.ThreadID == cursor.ThreadID && itemCursor.ItemID == cursor.ItemID && equalConversationSearchCursorTime(itemCursor.Created, cursor.Created) && (mode != ConversationSearchTerms || item.score == cursor.Score)
}

func equalConversationSearchCursorTime(left, right *string) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}
func conversationSearchCursorFor(item ConversationSearchItem, revision int64, hash string) conversationSearchCursor {
	var created *string
	if item.CreatedAt != nil {
		value := formatTime(*item.CreatedAt)
		created = &value
	}
	return conversationSearchCursor{Version: 1, Hash: hash, Revision: revision, Score: item.score, Created: created, ThreadID: item.ThreadID, ItemID: item.ItemID}
}
func encodeConversationSearchCursor(cursor conversationSearchCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
func decodeConversationSearchCursor(raw string) (*conversationSearchCursor, error) {
	if raw == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrConversationSearchInvalidCursor
	}
	var cursor conversationSearchCursor
	if json.Unmarshal(decoded, &cursor) != nil || cursor.Version != 1 || cursor.Hash == "" || cursor.ThreadID == "" || cursor.ItemID == "" || math.IsNaN(cursor.Score) || math.IsInf(cursor.Score, 0) {
		return nil, ErrConversationSearchInvalidCursor
	}
	if cursor.Created != nil {
		if _, err := time.Parse(time.RFC3339, *cursor.Created); err != nil {
			return nil, ErrConversationSearchInvalidCursor
		}
	}
	return &cursor, nil
}
func conversationSearchQueryHash(query ConversationSearchQuery) (string, error) {
	type shape struct {
		Query        string
		Mode         ConversationSearchMode
		Since, Until *string
		Roles        []TranscriptRole
		Providers    []Provider
		Kinds        []ThreadKind
		Group        string
	}
	var since, until *string
	if query.Since != nil {
		v := formatTime(*query.Since)
		since = &v
	}
	if query.Until != nil {
		v := formatTime(*query.Until)
		until = &v
	}
	raw, err := json.Marshal(shape{query.Query, query.Mode, since, until, query.Roles, query.Providers, query.Kinds, query.Group})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}
func searchStatusForMode(statuses []ConversationSearchStatus, mode ConversationSearchMode) (ConversationSearchStatus, bool) {
	for _, status := range statuses {
		if status.Mode == mode {
			return status, true
		}
	}
	return ConversationSearchStatus{}, false
}
func canonicalSearchRoles(values []TranscriptRole) []TranscriptRole {
	seen := map[TranscriptRole]bool{}
	for _, v := range values {
		seen[v] = true
	}
	out := make([]TranscriptRole, 0, len(seen))
	for _, v := range []TranscriptRole{RoleUser, RoleAssistant} {
		if seen[v] {
			out = append(out, v)
		}
	}
	return out
}
func canonicalSearchProviders(values []Provider) []Provider {
	seen := map[Provider]bool{}
	for _, v := range values {
		seen[v] = true
	}
	out := make([]Provider, 0, len(seen))
	for _, v := range []Provider{ProviderCodex, ProviderClaude, ProviderGrok} {
		if seen[v] {
			out = append(out, v)
		}
	}
	return out
}
func canonicalSearchKinds(values []ThreadKind) []ThreadKind {
	seen := map[ThreadKind]bool{}
	for _, v := range values {
		seen[v] = true
	}
	out := make([]ThreadKind, 0, len(seen))
	for _, v := range []ThreadKind{ThreadMain, ThreadSubagent, ThreadFork} {
		if seen[v] {
			out = append(out, v)
		}
	}
	return out
}

package history

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSearchConversationLiteralTermsGroupingAndCursor(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fixture := addSearchBackfillFixture(t, ctx, store)
	buildSearchModes(t, ctx, store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 1)
	literal, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "alpha user", Mode: ConversationSearchLiteral, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(literal.Items) != 1 || literal.Items[0].ItemID != "a-user" || literal.Items[0].Snippet != "alpha user" || literal.Items[0].Role != RoleUser {
		t.Fatalf("literal = %#v", literal)
	}
	terms, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "alpha user", Mode: ConversationSearchTerms, Group: ConversationSearchThreadGroup, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(terms.Items) != 1 || terms.Items[0].ThreadID != fixture.MainThreadID || terms.Items[0].MatchCount != 1 {
		t.Fatalf("terms group = %#v", terms)
	}
	if _, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "alpha missing", Mode: ConversationSearchTerms}); err != nil {
		t.Fatal(err)
	}
	ftsSyntax, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "alpha OR user", Mode: ConversationSearchTerms})
	if err != nil || len(ftsSyntax.Items) != 0 {
		t.Fatalf("terms syntax was interpreted instead of requiring every token: %#v, %v", ftsSyntax, err)
	}
	if _, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "ab", Mode: ConversationSearchLiteral}); err == nil {
		t.Fatal("short literal accepted")
	}

	if err := appendSearchBackfillItem(ctx, store, fixture, "user-before-cursor", 6); err != nil {
		t.Fatal(err)
	}
	page, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "user", Mode: ConversationSearchTerms, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor == nil {
		t.Fatal("expected a next cursor")
	}
	cursor, err := decodeConversationSearchCursor(*page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	cursor.ItemID = "not-an-item-in-this-ready-snapshot"
	tampered, err := encodeConversationSearchCursor(*cursor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "user", Mode: ConversationSearchTerms, Limit: 1, Cursor: tampered}); !errors.Is(err, ErrConversationSearchInvalidCursor) {
		t.Fatalf("tampered ordering cursor = %v", err)
	}
	second, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "user", Mode: ConversationSearchTerms, Limit: 1, Cursor: *page.NextCursor})
	if err != nil || len(second.Items) != 1 || second.NextCursor != nil {
		t.Fatalf("second page = %#v, %v", second, err)
	}
	if err := appendSearchBackfillItem(ctx, store, fixture, "g-after-cursor", 7); err != nil {
		t.Fatal(err)
	}
	_, err = store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "user", Mode: ConversationSearchTerms, Limit: 1, Cursor: *page.NextCursor})
	if !errors.Is(err, ErrConversationSearchChanged) {
		t.Fatalf("changed cursor = %v", err)
	}
}

func TestSearchConversationIndexesFable51SplitAgentTextOnce(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	data := readTranscriptFixture(t, "claude-fable-5-1-split.jsonl")
	projected, err := newTranscriptFixture(t, ProviderClaude).ProjectTranscript(ctx, TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderClaude), Reader: strings.NewReader(string(data)),
	})
	if err != nil {
		t.Fatal(err)
	}
	items := make([]TranscriptItem, 0, len(projected.Items))
	for _, item := range projected.Items {
		if item.Type != ItemUserMessage && item.Type != ItemAgentMessage {
			continue
		}
		items = append(items, searchEligibilityItem(item.ID, int64(len(items)+1), item.Type, item.Role, item.Text))
	}
	root := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: filepath.Join(t.TempDir(), "claude")}
	addSearchQueryTranscript(t, ctx, store, root, "fable.jsonl", "fable-search", ThreadMain, items, nil)
	buildSearchModes(t, ctx, store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 10)
	result, err := store.SearchConversationItems(ctx, ConversationSearchQuery{
		Query: "verified correction", Mode: ConversationSearchTerms, Roles: []TranscriptRole{RoleAssistant},
		Providers: []Provider{ProviderClaude}, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].Snippet == "" {
		t.Fatalf("split Fable search results = %#v", result)
	}
}

func TestConversationSearchSnippetIsUnicodeBounded(t *testing.T) {
	text := strings.Repeat("界", 700) + "needle" + strings.Repeat("界", 700)
	snippet := conversationSearchSnippet(text, "needle", false)
	if got := len([]rune(snippet)); got > 512 || !strings.Contains(snippet, "needle") {
		t.Fatalf("snippet runes=%d content=%q", got, snippet)
	}
}

func TestConversationSearchTermSnippetUsesOriginalUnicodeRuneCoordinates(t *testing.T) {
	// strings.ToLower("İ") changes a two-byte rune to one ASCII byte. Its byte
	// offset cannot be used to slice the original text before a later word.
	text := strings.Repeat("İ ", 700) + "Needle " + strings.Repeat("界", 700)
	snippet := conversationSearchSnippet(text, "needle", true)
	if got := len([]rune(snippet)); got > 512 || !strings.Contains(snippet, "Needle") {
		t.Fatalf("Unicode folded snippet runes=%d content=%q", got, snippet)
	}
}

func TestConversationSearchCursorRejectsMalformedOrderingFields(t *testing.T) {
	created := "not-rfc3339"
	encoded, err := encodeConversationSearchCursor(conversationSearchCursor{Version: 1, Hash: "hash", Revision: 1, Score: 0, Created: &created, ThreadID: "thread", ItemID: "item"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeConversationSearchCursor(encoded); !errors.Is(err, ErrConversationSearchInvalidCursor) {
		t.Fatalf("invalid created cursor = %v", err)
	}
	// JSON permits an exponent whose decoded magnitude overflows float64. It
	// must not become an ordering key for a terms cursor.
	raw := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"h":"hash","r":1,"s":1e999,"t":"thread","i":"item"}`))
	if _, err := decodeConversationSearchCursor(raw); !errors.Is(err, ErrConversationSearchInvalidCursor) {
		t.Fatalf("non-finite score cursor = %v", err)
	}
}

func TestConversationSearchMessagePathStopsAfterLookAheadWhileThreadGroupingScans(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	addSearchQueryTranscript(t, ctx, store, root, "bounded.jsonl", "search-query-bounded", ThreadMain, []TranscriptItem{
		searchQueryItem("first", 1, ItemUserMessage, RoleUser, "bounded", ptrTime(base.Add(2*time.Hour))),
		searchQueryItem("second", 2, ItemUserMessage, RoleUser, "bounded", ptrTime(base.Add(time.Hour))),
		searchQueryItem("third", 3, ItemUserMessage, RoleUser, "bounded", ptrTime(base)),
	}, nil)
	buildSearchModes(t, ctx, store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 20)
	if _, err := store.db.ExecContext(ctx, `UPDATE conversation_items SET created_at = '0000-not-a-time' WHERE item_id = 'third'`); err != nil {
		t.Fatal(err)
	}
	page, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "bounded", Mode: ConversationSearchLiteral, Limit: 1})
	if err != nil || len(page.Items) != 1 || page.NextCursor == nil {
		t.Fatalf("bounded message page = %#v, %v", page, err)
	}
	if _, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "bounded", Mode: ConversationSearchLiteral, Group: ConversationSearchThreadGroup}); err == nil {
		t.Fatal("grouped search did not scan the third verified candidate")
	}
}

func TestSearchConversationFiltersGroupingAndCanonicalBody(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "codex")}
	mainItems := []TranscriptItem{
		searchQueryItem("body", 1, ItemUserMessage, RoleUser, "preview does not match", &start),
		searchQueryItem("at-since", 2, ItemUserMessage, RoleUser, "timefilter", &start),
		searchQueryItem("at-until", 3, ItemUserMessage, RoleUser, "timefilter", &end),
		searchQueryItem("nil-time", 4, ItemUserMessage, RoleUser, "timefilter", nil),
		searchQueryItem("term-short", 5, ItemAgentMessage, RoleAssistant, "alpha", &start),
		searchQueryItem("term-long", 6, ItemAgentMessage, RoleAssistant, "alpha alpha alpha alpha alpha alpha alpha alpha", &end),
	}
	mainID := addSearchQueryTranscript(t, ctx, store, root, "main.jsonl", "search-query-main", ThreadMain, mainItems, []TranscriptBody{{ItemID: "body", ContentRef: "body", Text: "Case  spaced  body"}})
	claudeRoot := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: filepath.Join(t.TempDir(), "claude")}
	addSearchQueryTranscript(t, ctx, store, claudeRoot, "subagent.jsonl", "search-query-claude", ThreadSubagent, []TranscriptItem{searchQueryItem("claude-assistant", 1, ItemAgentMessage, RoleAssistant, "filterrole", &start)}, nil)
	buildSearchModes(t, ctx, store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 20)

	body, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "Case  spaced", Mode: ConversationSearchLiteral})
	if err != nil || len(body.Items) != 1 || body.Items[0].ItemID != "body" || body.Items[0].Snippet != "Case  spaced  body" {
		t.Fatalf("canonical retained body literal = %#v, %v", body, err)
	}
	if result, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "case  spaced", Mode: ConversationSearchLiteral}); err != nil || len(result.Items) != 0 {
		t.Fatalf("literal case verification = %#v, %v", result, err)
	}
	timed, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "timefilter", Mode: ConversationSearchLiteral, Since: &start, Until: &end})
	if err != nil || len(timed.Items) != 1 || timed.Items[0].ItemID != "at-since" { // until and nil must both be excluded.
		t.Fatalf("half-open time filter = %#v, %v", timed, err)
	}
	sinceOnly, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "timefilter", Mode: ConversationSearchLiteral, Since: &start})
	if err != nil || len(sinceOnly.Items) != 2 || sinceOnly.Items[0].ItemID != "at-until" || sinceOnly.Items[1].ItemID != "at-since" {
		t.Fatalf("since excludes nil timestamp = %#v, %v", sinceOnly, err)
	}
	untilOnly, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "timefilter", Mode: ConversationSearchLiteral, Until: &end})
	if err != nil || len(untilOnly.Items) != 1 || untilOnly.Items[0].ItemID != "at-since" {
		t.Fatalf("until excludes nil timestamp = %#v, %v", untilOnly, err)
	}
	filtered, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "filterrole", Mode: ConversationSearchTerms, Roles: []TranscriptRole{RoleAssistant}, Providers: []Provider{ProviderClaude}, Kinds: []ThreadKind{ThreadSubagent}})
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].ItemID != "claude-assistant" {
		t.Fatalf("repeatable storage filters = %#v, %v", filtered, err)
	}
	raw, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "alpha", Mode: ConversationSearchTerms})
	grouped, groupErr := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "alpha", Mode: ConversationSearchTerms, Group: ConversationSearchThreadGroup})
	if err != nil || groupErr != nil || len(raw.Items) != 2 || len(grouped.Items) != 1 || grouped.Items[0].ThreadID != mainID || grouped.Items[0].MatchCount != 2 || grouped.Items[0].ItemID != raw.Items[0].ItemID {
		t.Fatalf("terms representative raw=%#v (%v) grouped=%#v (%v)", raw, err, grouped, groupErr)
	}
}

func searchQueryItem(id string, sequence int64, itemType TranscriptItemType, role TranscriptRole, text string, created *time.Time) TranscriptItem {
	item := searchEligibilityItem(id, sequence, itemType, role, text)
	item.CreatedAt = created
	return item
}

func ptrTime(value time.Time) *time.Time { return &value }

func addSearchQueryTranscript(t *testing.T, ctx context.Context, store *Store, root SourceRoot, name, nativeID string, kind ThreadKind, items []TranscriptItem, bodies []TranscriptBody) string {
	t.Helper()
	metadata := testSource(root, name, nativeID, kind, nil)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	source, ok, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("stored source = %#v, %t, %v", source, ok, err)
	}
	projection := searchEligibilityProjection("search-query-"+nativeID, items, bodies)
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: source.ID, SourceGeneration: source.Generation, Provider: root.Provider, ParserVersion: "search-query", CompletionRuleVersion: "search-query", Result: projection, IndexedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	return applied.ThreadID
}

func TestSearchConversationRejectsIncompleteMode(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.SearchConversationItems(context.Background(), ConversationSearchQuery{Query: "alpha", Mode: ConversationSearchLiteral})
	var unavailable *ConversationSearchUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Status.State != ConversationSearchNeedsBuild {
		t.Fatalf("unavailable = %#v, %v", unavailable, err)
	}
}

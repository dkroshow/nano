package history

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEligibleConversationItemUsesCanonicalRetainedText(t *testing.T) {
	body := "complete retained body"
	emptyBody := ""
	base := ConversationSearchCandidate{
		Identity: ConversationSearchIdentity{ProjectionKey: "projection", ThreadID: "thread", ItemID: "item"},
		Active:   true, ThreadKind: ThreadMain, Type: ItemUserMessage, Role: RoleUser,
	}
	for _, test := range []struct {
		name      string
		candidate ConversationSearchCandidate
		want      string
	}{
		{"small display text", base, "inline text"},
		{"retained body wins", ConversationSearchCandidate{Identity: base.Identity, Active: true, ThreadKind: ThreadMain, Type: ItemAgentMessage, Role: RoleAssistant, DisplayText: "preview", BodyText: &body}, body},
		{"present empty retained body wins", ConversationSearchCandidate{Identity: base.Identity, Active: true, ThreadKind: ThreadMain, Type: ItemUserMessage, Role: RoleUser, DisplayText: "preview", BodyText: &emptyBody}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := test.candidate
			if candidate.DisplayText == "" && candidate.BodyText == nil {
				candidate.DisplayText = "inline text"
			}
			eligible, ok := EligibleConversationItem(candidate)
			if !ok || eligible.Text != test.want || eligible.Identity != base.Identity {
				t.Fatalf("eligibility = %#v, %t; want text %q and stable identity %#v", eligible, ok, test.want, base.Identity)
			}
		})
	}
}

func TestEligibleConversationItemRequiresSemanticConversation(t *testing.T) {
	base := ConversationSearchCandidate{
		Identity: ConversationSearchIdentity{ProjectionKey: "projection", ThreadID: "thread", ItemID: "item"},
		Active:   true, ThreadKind: ThreadMain, DisplayText: "text",
	}
	eligible := []ConversationSearchCandidate{
		{Identity: base.Identity, Active: true, ThreadKind: ThreadMain, Type: ItemUserMessage, Role: RoleUser, DisplayText: "user"},
		{Identity: base.Identity, Active: true, ThreadKind: ThreadSubagent, Type: ItemAgentMessage, Role: RoleAssistant, DisplayText: "assistant"},
		{Identity: base.Identity, Active: true, ThreadKind: ThreadFork, Type: ItemInteraction, Role: RoleAssistant, DisplayText: "question"},
		{Identity: base.Identity, Active: true, ThreadKind: ThreadMain, Type: ItemInteraction, Role: RoleUser, DisplayText: "answer"},
		// A programmatic judge is still an ordinary user message: eligibility is
		// provider-neutral and does not inspect prompt text or thread origin.
		{Identity: base.Identity, Active: true, ThreadKind: ThreadMain, Type: ItemUserMessage, Role: RoleUser, DisplayText: "Security judge prompt"},
	}
	for _, candidate := range eligible {
		if _, ok := EligibleConversationItem(candidate); !ok {
			t.Fatalf("eligible conversation rejected: %#v", candidate)
		}
	}

	excluded := []ConversationSearchCandidate{
		{Identity: base.Identity, Active: false, ThreadKind: ThreadMain, Type: ItemUserMessage, Role: RoleUser, DisplayText: "inactive"},
		{Identity: base.Identity, Active: true, ThreadKind: ThreadGuardian, Type: ItemUserMessage, Role: RoleUser, DisplayText: "guardian"},
		{Identity: base.Identity, Active: true, ThreadKind: ThreadMain, Type: ItemInteraction, Role: RoleUser, Synthetic: true, DisplayText: "synthetic question"},
		{Identity: base.Identity, Active: true, ThreadKind: ThreadMain, Type: ItemUserMessage, Role: RoleAssistant, DisplayText: "wrong role"},
		{Identity: base.Identity, Active: true, ThreadKind: ThreadMain, Type: ItemAgentMessage, Role: RoleUser, DisplayText: "wrong role"},
	}
	for _, itemType := range []TranscriptItemType{ItemReasoning, ItemCommand, ItemToolCall, ItemToolResult, ItemFileChange, ItemPlan, ItemCompaction, ItemNotice, ItemProviderEvent} {
		excluded = append(excluded, ConversationSearchCandidate{Identity: base.Identity, Active: true, ThreadKind: ThreadMain, Type: itemType, Role: RoleAssistant, DisplayText: string(itemType)})
	}
	for _, candidate := range excluded {
		if eligible, ok := EligibleConversationItem(candidate); ok {
			t.Fatalf("excluded item admitted: candidate %#v eligible %#v", candidate, eligible)
		}
	}
}

func TestStoreLoadEligibleConversationItemUsesCanonicalBodyAndProjectionState(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	metadata := testSource(root, "thread.jsonl", "search-main", ThreadMain, nil)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("stored source = %#v, %t, %v", stored, ok, err)
	}
	body := "complete retained text"
	first := searchEligibilityProjection("search-projection-old", []TranscriptItem{
		searchEligibilityItem("small", 1, ItemUserMessage, RoleUser, "small display text"),
		searchEligibilityItem("large", 2, ItemAgentMessage, RoleAssistant, "inline preview"),
		searchEligibilityItem("empty", 3, ItemUserMessage, RoleUser, "inline preview"),
	}, []TranscriptBody{
		{ItemID: "large", ContentRef: "large-body", Text: body, OriginalBytes: int64(len(body)), StoredBytes: int64(len(body))},
		{ItemID: "empty", ContentRef: "empty-body", Text: "", OriginalBytes: 0, StoredBytes: 0},
	})
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: ProviderCodex, ParserVersion: "search-test-v1", CompletionRuleVersion: "search-test-rules", Result: first,
		IndexedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ itemID, want string }{
		{"small", "small display text"},
		{"large", body},
		{"empty", ""},
	} {
		eligible, found, err := store.LoadEligibleConversationItem(ctx, first.ProjectionKey, test.itemID)
		if err != nil || !found || eligible.Text != test.want {
			t.Fatalf("%s eligibility = %#v, %t, %v; want %q", test.itemID, eligible, found, err, test.want)
		}
		if eligible.Identity.ProjectionID <= 0 || eligible.Identity.ProjectionKey != first.ProjectionKey ||
			eligible.Identity.ThreadID != applied.ThreadID || eligible.Identity.ItemID != test.itemID {
			t.Fatalf("%s stable identity = %#v", test.itemID, eligible.Identity)
		}
	}

	second := searchEligibilityProjection("search-projection-active", []TranscriptItem{
		searchEligibilityItem("replacement", 1, ItemUserMessage, RoleUser, "replacement"),
	}, nil)
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: ProviderCodex, ParserVersion: "search-test-v2", CompletionRuleVersion: "search-test-rules", Result: second,
		IndexedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if eligible, found, err := store.LoadEligibleConversationItem(ctx, first.ProjectionKey, "small"); err != nil || found {
		t.Fatalf("inactive projection eligibility = %#v, %t, %v", eligible, found, err)
	}

	guardian := testSource(root, "guardian.jsonl", "search-guardian", ThreadMain, nil)
	guardian.Result.Projection.Kind = ThreadGuardian
	guardianApplied, err := store.ApplySource(ctx, guardian)
	if err != nil {
		t.Fatal(err)
	}
	guardianStored, ok, err := store.LoadSource(ctx, mustRootID(t, store, root), guardian.Source.Path)
	if err != nil || !ok {
		t.Fatalf("guardian stored source = %#v, %t, %v", guardianStored, ok, err)
	}
	guardianProjection := searchEligibilityProjection("guardian-projection", []TranscriptItem{
		searchEligibilityItem("guardian-message", 1, ItemUserMessage, RoleUser, "guardian prompt"),
	}, nil)
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: guardianApplied.ThreadID, SourceID: guardianStored.ID, SourceGeneration: guardianStored.Generation,
		Provider: ProviderCodex, ParserVersion: "search-test-v1", CompletionRuleVersion: "search-test-rules", Result: guardianProjection,
		IndexedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if eligible, found, err := store.LoadEligibleConversationItem(ctx, guardianProjection.ProjectionKey, "guardian-message"); err != nil || found {
		t.Fatalf("guardian eligibility = %#v, %t, %v", eligible, found, err)
	}
	var visited []EligibleConversationSearchItem
	if err := VisitPersistedEligibleConversationItems(ctx, store.db, func(item EligibleConversationSearchItem) error {
		visited = append(visited, item)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(visited) != 1 || visited[0].Identity.ItemID != "replacement" || visited[0].Text != "replacement" || visited[0].Role != RoleUser {
		t.Fatalf("persisted eligible items = %#v", visited)
	}
}

func TestPersistedConversationSearchIdentityEligibilityMatchesTextPath(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fixture := addSearchBackfillFixture(t, ctx, store)
	body := "complete retained body"
	emptyBody := ""
	appendResult := searchEligibilityProjection("", []TranscriptItem{
		searchEligibilityItem("body-present", 6, ItemAgentMessage, RoleAssistant, "body preview"),
		searchEligibilityItem("empty-body", 7, ItemUserMessage, RoleUser, "empty body preview"),
		searchEligibilityItem("wrong-role", 8, ItemUserMessage, RoleAssistant, "wrong-role exclusion"),
	}, []TranscriptBody{
		{ItemID: "body-present", ContentRef: "body-present-ref", Text: body, OriginalBytes: int64(len(body)), StoredBytes: int64(len(body))},
		{ItemID: "empty-body", ContentRef: "empty-body-ref", Text: emptyBody},
	})
	appendResult.ProjectionKey = fixture.MainProjectionKey
	appendResult.CompleteOffset, appendResult.CompleteLines = 8, 8
	appendResult.Turns[0].EndSeq = 8
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptAppend, ThreadID: fixture.MainThreadID, SourceID: fixture.MainSourceID, SourceGeneration: fixture.MainGeneration,
		Provider: ProviderCodex, ParserVersion: "backfill", CompletionRuleVersion: "backfill",
		ExpectedProjectionKey: fixture.MainProjectionKey, Result: appendResult, IndexedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	full := map[string]string{}
	if err := VisitPersistedEligibleConversationItems(ctx, store.db, func(item EligibleConversationSearchItem) error {
		full[item.Identity.ItemID] = item.Text
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	identities := map[string]struct{}{}
	if err := VisitPersistedEligibleConversationItemIdentities(ctx, store.db, func(identity ConversationSearchIdentity) error {
		identities[identity.ItemID] = struct{}{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fullIdentities := make(map[string]struct{}, len(full))
	for itemID := range full {
		fullIdentities[itemID] = struct{}{}
	}
	if !reflect.DeepEqual(fullIdentities, identities) {
		t.Fatalf("text and identity eligibility differ: text=%#v identity=%#v", fullIdentities, identities)
	}
	if full["body-present"] != body || full["empty-body"] != "" {
		t.Fatalf("canonical body parity evidence = %#v", full)
	}
	for _, itemID := range []string{"inactive-user", "guardian-user", "e-injected", "wrong-role"} {
		if _, exists := full[itemID]; exists {
			t.Fatalf("excluded text item %q admitted", itemID)
		}
		if _, exists := identities[itemID]; exists {
			t.Fatalf("excluded identity item %q admitted", itemID)
		}
	}
	for _, itemID := range []string{"body-present", "empty-body"} {
		if _, exists := full[itemID]; !exists {
			t.Fatalf("eligible body item %q rejected", itemID)
		}
	}
}

func TestPersistedConversationSearchCheckpointPlanSeeksConversationItemsCompositeIndex(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	addLargeSearchBackfillProjections(t, ctx, store, 3, 256)
	var projectionID int64
	if err := store.db.QueryRowContext(ctx, `SELECT projection_id FROM conversation_items GROUP BY projection_id ORDER BY projection_id LIMIT 1 OFFSET 1`).Scan(&projectionID); err != nil {
		t.Fatal(err)
	}
	var itemID string
	if err := store.db.QueryRowContext(ctx, `SELECT item_id FROM conversation_items WHERE projection_id = ? ORDER BY item_id LIMIT 1`, projectionID).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	where, args := persistedConversationSearchCheckpointClause(projectionID, itemID)
	args = append(args, 7)
	rows, err := store.db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+persistedConversationSearchCandidatesSQL(where, "LIMIT ?"), args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var firstAccess string
	for rows.Next() {
		var id, parent, ignored int
		var detail string
		if err := rows.Scan(&id, &parent, &ignored, &detail); err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(detail)
		if firstAccess == "" && (strings.Contains(lower, "search ") || strings.Contains(lower, "scan ")) {
			firstAccess = lower
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(firstAccess, "search ci using") || !strings.Contains(firstAccess, "sqlite_autoindex_conversation_items_1") || !strings.Contains(firstAccess, "projection_id") {
		t.Fatalf("checkpoint paging is no longer driven by the conversation_items composite range: first access %q", firstAccess)
	}
}

func searchEligibilityItem(id string, seq int64, itemType TranscriptItemType, role TranscriptRole, text string) TranscriptItem {
	return TranscriptItem{
		ID: id, ProjectedSeq: seq, SourceKey: "source-" + id, TurnKey: "turn", TurnOrdinal: 1,
		Type: itemType, Status: ItemStatusCompleted, Role: role, Text: text,
		SourceLineSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
}

func searchEligibilityProjection(key string, items []TranscriptItem, bodies []TranscriptBody) TranscriptProjectionResult {
	return TranscriptProjectionResult{
		ProjectionKey: key, CompleteOffset: 1, CompleteLines: 1, ParserState: json.RawMessage(`{}`),
		Items: items, Bodies: bodies,
		Turns: []TranscriptTurn{{Key: "turn", Ordinal: 1, StartSeq: 1, EndSeq: int64(len(items)), Status: TurnCompleted, CompletionEvidence: CompletionTaskComplete}},
	}
}

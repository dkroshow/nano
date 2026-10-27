package history

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type conversationSearchCandidateQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// VisitPersistedEligibleConversationItems adapts normalized persistence rows to
// the one provider-neutral search corpus boundary. Future historical backfill
// and incremental maintenance must use this function (or
// EligibleConversationItem directly) instead of duplicating SQL predicates.
// It intentionally reads every normalized item and lets the shared boundary
// reject inactive, Guardian, synthetic, and non-conversation evidence.
func VisitPersistedEligibleConversationItems(ctx context.Context, queryer conversationSearchCandidateQueryer, visit func(EligibleConversationSearchItem) error) error {
	return VisitPersistedConversationSearchCandidatesAfter(ctx, queryer, nil, nil, func(_ ConversationSearchCandidate, eligible EligibleConversationSearchItem, ok bool) error {
		if !ok {
			return nil
		}
		return visit(eligible)
	})
}

// VisitPersistedEligibleConversationItemIdentities streams the shared metadata
// corpus policy for callers, such as integrity reconciliation, that compare
// only source identities. It deliberately does not join display or body text;
// canonical text is not part of an identity comparison.
func VisitPersistedEligibleConversationItemIdentities(ctx context.Context, queryer conversationSearchCandidateQueryer, visit func(ConversationSearchIdentity) error) error {
	rows, err := queryer.QueryContext(ctx, persistedConversationSearchIdentityCandidatesSQL())
	if err != nil {
		return fmt.Errorf("query persisted conversation search identity candidates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		candidate, err := scanPersistedConversationSearchIdentityCandidate(rows)
		if err != nil {
			return err
		}
		if !eligibleConversationSearchMetadata(candidate) {
			continue
		}
		if err := visit(candidate.Identity); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate persisted conversation search identity candidates: %w", err)
	}
	return nil
}

// VisitPersistedEligibleConversationItemsAfter continues after the durable
// projection/item identity checkpoint. The SQL clause only establishes source
// order; EligibleConversationItem remains the sole corpus predicate.
func VisitPersistedEligibleConversationItemsAfter(ctx context.Context, queryer conversationSearchCandidateQueryer, projectionID *int64, itemID *string, visit func(EligibleConversationSearchItem) error) error {
	return VisitPersistedConversationSearchCandidatesAfter(ctx, queryer, projectionID, itemID, func(_ ConversationSearchCandidate, eligible EligibleConversationSearchItem, ok bool) error {
		if !ok {
			return nil
		}
		return visit(eligible)
	})
}

// VisitPersistedConversationSearchCandidatesAfter iterates durable source rows
// in checkpoint order and applies EligibleConversationItem exactly once per
// row. Backfill uses this to checkpoint excluded rows without inventing a
// second corpus predicate.
func VisitPersistedConversationSearchCandidatesAfter(ctx context.Context, queryer conversationSearchCandidateQueryer, projectionID *int64, itemID *string, visit func(ConversationSearchCandidate, EligibleConversationSearchItem, bool) error) error {
	return VisitPersistedConversationSearchCandidatesAfterLimit(ctx, queryer, projectionID, itemID, 0, visit)
}

// VisitPersistedConversationSearchCandidatesAfterLimit bounds raw normalized
// source rows, including rows the eligibility boundary rejects. A non-positive
// limit means no bound.
func VisitPersistedConversationSearchCandidatesAfterLimit(ctx context.Context, queryer conversationSearchCandidateQueryer, projectionID *int64, itemID *string, limit int, visit func(ConversationSearchCandidate, EligibleConversationSearchItem, bool) error) error {
	if limit < 0 {
		return fmt.Errorf("conversation search candidate limit must not be negative")
	}
	where := ""
	args := []any(nil)
	if projectionID != nil || itemID != nil {
		if projectionID == nil || itemID == nil {
			return fmt.Errorf("conversation search checkpoint is incomplete")
		}
		where, args = persistedConversationSearchCheckpointClause(*projectionID, *itemID)
	}
	limitClause := ""
	if limit > 0 {
		limitClause = "LIMIT ?"
		args = append(args, limit)
	}
	return visitPersistedConversationSearchCandidates(ctx, queryer, where, args, limitClause, visit)
}

// persistedConversationSearchCheckpointClause is lexicographically equivalent
// to projection_id > ? OR (projection_id = ? AND item_id > ?), but SQLite can
// turn the row-value predicate into a range seek on conversation_items'
// (projection_id, item_id) primary key instead of an ordered full scan.
func persistedConversationSearchCheckpointClause(projectionID int64, itemID string) (string, []any) {
	return "WHERE (ci.projection_id, ci.item_id) > (?, ?)", []any{projectionID, itemID}
}

// VisitPersistedConversationSearchCandidatesForProjectionSequenceRange scopes
// the shared boundary to a contiguous appended sequence range. It uses three
// fixed bind parameters regardless of append size; eligibility still comes
// solely from EligibleConversationItem.
func VisitPersistedConversationSearchCandidatesForProjectionSequenceRange(ctx context.Context, queryer conversationSearchCandidateQueryer, projectionID, afterSequence, throughSequence int64, visit func(ConversationSearchCandidate, EligibleConversationSearchItem, bool) error) error {
	if projectionID <= 0 || throughSequence <= afterSequence {
		return nil
	}
	return visitPersistedConversationSearchCandidates(ctx, queryer, "WHERE cp.id = ? AND ci.projected_seq > ? AND ci.projected_seq <= ?", []any{projectionID, afterSequence, throughSequence}, "", visit)
}

// VisitPersistedConversationSearchCandidatesForProjections scopes the shared
// boundary to projections whose active/thread state changed.
func VisitPersistedConversationSearchCandidatesForProjections(ctx context.Context, queryer conversationSearchCandidateQueryer, projectionIDs []int64, visit func(ConversationSearchCandidate, EligibleConversationSearchItem, bool) error) error {
	if len(projectionIDs) == 0 {
		return nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(projectionIDs)), ",")
	args := make([]any, len(projectionIDs))
	for index := range projectionIDs {
		args[index] = projectionIDs[index]
	}
	return visitPersistedConversationSearchCandidates(ctx, queryer, "WHERE cp.id IN ("+marks+")", args, "", visit)
}

func visitPersistedConversationSearchCandidates(ctx context.Context, queryer conversationSearchCandidateQueryer, where string, args []any, limitClause string, visit func(ConversationSearchCandidate, EligibleConversationSearchItem, bool) error, shadowKind ...ThreadKind) error {
	rows, err := queryer.QueryContext(ctx, persistedConversationSearchCandidatesSQL(where, limitClause), args...)
	if err != nil {
		return fmt.Errorf("query persisted conversation search candidates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		candidate, err := scanPersistedConversationSearchCandidate(rows)
		if err != nil {
			return err
		}
		if len(shadowKind) > 0 {
			candidate.Active = true
			candidate.ThreadKind = shadowKind[0]
		}
		eligible, ok := EligibleConversationItem(candidate)
		if err := visit(candidate, eligible, ok); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate persisted conversation search candidates: %w", err)
	}
	return nil
}

// persistedConversationSearchCandidatesSQL keeps the durable source ordering
// on conversation_items so checkpointed batches can seek the composite primary
// key range instead of revisiting each projection's full item set.
func persistedConversationSearchCandidatesSQL(where string, limitClause string) string {
	return `
SELECT cp.id, cp.projection_key, cp.thread_id, cp.state, t.kind,
       ci.item_id, ci.item_type, ci.role, ci.synthetic, ci.display_text,
       CASE WHEN cib.item_id IS NULL THEN 0 ELSE 1 END, cib.body_text
FROM conversation_items ci
JOIN conversation_projections cp ON cp.id = ci.projection_id
JOIN threads t ON t.id = cp.thread_id
LEFT JOIN conversation_item_bodies cib
  ON cib.projection_id = ci.projection_id AND cib.item_id = ci.item_id
` + where + `
ORDER BY ci.projection_id, ci.item_id
` + limitClause
}

func persistedConversationSearchIdentityCandidatesSQL() string {
	return `
SELECT cp.id, cp.projection_key, cp.thread_id, cp.state, t.kind,
       ci.item_id, ci.item_type, ci.role, ci.synthetic
FROM conversation_items ci
JOIN conversation_projections cp ON cp.id = ci.projection_id
JOIN threads t ON t.id = cp.thread_id
ORDER BY ci.projection_id, ci.item_id`
}

func scanPersistedConversationSearchCandidate(scanner interface{ Scan(...any) error }) (ConversationSearchCandidate, error) {
	var candidate ConversationSearchCandidate
	var projectionState, kind, itemType string
	var role, bodyText sql.NullString
	var bodyPresent int
	if err := scanner.Scan(
		&candidate.Identity.ProjectionID, &candidate.Identity.ProjectionKey, &candidate.Identity.ThreadID,
		&projectionState, &kind, &candidate.Identity.ItemID, &itemType, &role,
		&candidate.Synthetic, &candidate.DisplayText, &bodyPresent, &bodyText); err != nil {
		return ConversationSearchCandidate{}, fmt.Errorf("scan persisted conversation search candidate: %w", err)
	}
	candidate.Active = projectionState == "active"
	candidate.ThreadKind = ThreadKind(kind)
	candidate.Type = TranscriptItemType(itemType)
	candidate.Role = TranscriptRole(stringFromNull(role))
	if bodyPresent != 0 {
		text := stringFromNull(bodyText)
		candidate.BodyText = &text
	}
	return candidate, nil
}

func scanPersistedConversationSearchIdentityCandidate(scanner interface{ Scan(...any) error }) (ConversationSearchCandidate, error) {
	var candidate ConversationSearchCandidate
	var projectionState, kind, itemType string
	var role sql.NullString
	if err := scanner.Scan(
		&candidate.Identity.ProjectionID, &candidate.Identity.ProjectionKey, &candidate.Identity.ThreadID,
		&projectionState, &kind, &candidate.Identity.ItemID, &itemType, &role,
		&candidate.Synthetic); err != nil {
		return ConversationSearchCandidate{}, fmt.Errorf("scan persisted conversation search identity candidate: %w", err)
	}
	candidate.Active = projectionState == "active"
	candidate.ThreadKind = ThreadKind(kind)
	candidate.Type = TranscriptItemType(itemType)
	candidate.Role = TranscriptRole(stringFromNull(role))
	return candidate, nil
}

func visitBuildingConversationSearchCandidates(ctx context.Context, queryer conversationSearchCandidateQueryer, projectionID, after, through int64, kind ThreadKind, visit func(ConversationSearchCandidate, EligibleConversationSearchItem, bool) error) error {
	return visitPersistedConversationSearchCandidates(ctx, queryer, "WHERE cp.id = ? AND ci.projected_seq > ? AND ci.projected_seq <= ?", []any{projectionID, after, through}, "", visit, kind)
}

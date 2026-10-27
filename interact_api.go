package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

const (
	defaultCompletedTurnFeedLimit = 100
	maxCompletedTurnFeedLimit     = 1000
)

func (s *server) handleCompletedTurns(w http.ResponseWriter, r *http.Request, hostedThreadID string) {
	if s.completedTurns == nil {
		httpapi.WriteHTTPError(w, "completed-turn source unavailable", http.StatusServiceUnavailable)
		return
	}
	limit := defaultCompletedTurnFeedLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			httpapi.WriteHTTPError(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = min(parsed, maxCompletedTurnFeedLimit)
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("after"))
	rows, err := s.completedTurns.after(hostedThreadID, cursor, limit)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page := interact.CompletedTurnPage{Items: make([]interact.CompletedTurn, 0, len(rows)), NextCursor: cursor}
	for _, row := range rows {
		item, ready := s.publicCompletedTurn(r.Context(), hostedThreadID, row)
		if !ready {
			// The page ends before an answer not decided yet, or an operation
			// not readable now; every catalog commit of a Claude thread, and
			// every finished turn, sweeps its owners' subscriptions.
			break
		}
		page.Items = append(page.Items, item)
		page.NextCursor = row.TurnID
	}
	httpapi.WriteJSON(w, page)
}

// publicCompletedTurn is a completed turn as consumers see it. A Claude turn
// that answered a Nano prompt publishes that prompt's answer by the rule
// (claudeAnswers): the final message and outcome of the saved turn that
// consumed it, and that turn's identity. No stored final stands in for it: a
// completed row waits until the rule decides (Claude ended the turn, so its
// saved turn ends too), while a failed or interrupted row, a decision the
// rule does not reverse, publishes no answer of its own until it decides one.
// An execution two prompts claim publishes no answer. A turn with no Nano
// prompt, such as one Claude began itself, and every other provider's turn
// publish as stored.
func (s *server) publicCompletedTurn(ctx context.Context, hosted string, row completedTurn) (interact.CompletedTurn, bool) {
	var usage *interact.TokenUsage
	if row.UsageInputTokens != nil || row.UsageCachedInputTokens != nil || row.UsageOutputTokens != nil {
		usage = &interact.TokenUsage{InputTokens: row.UsageInputTokens, CachedInputTokens: row.UsageCachedInputTokens, OutputTokens: row.UsageOutputTokens}
	}
	item := interact.CompletedTurn{Provider: row.Provider, TurnID: row.TurnID,
		FinalMessageID: row.FinalMessageID, Outcome: row.Outcome, Usage: usage, CompletedAt: row.CompletedAt}
	if item.Outcome == "" {
		item.Outcome = agentTurnCompleted
	}
	// The row keeps the action its prompt was delivered under, so the
	// operation is the one delivered under it, if any: a person's prompt has
	// none, and retention removes old ones.
	if row.ClientActionID != "" {
		op, found, err := s.agentExecutions.persistedTurnForTransportAction(ctx, hosted, row.ClientActionID)
		if err != nil {
			return item, false
		}
		if found {
			item.OperationID = op.ID
		}
	}
	if row.Provider != string(model.ProviderClaude) || s.turns == nil {
		return item, true
	}
	claims, err := s.turns.ProviderTurn(ctx, hosted, model.ProviderClaude, "", row.TurnID)
	if err != nil {
		return item, false
	}
	switch len(claims) {
	case 0:
		return item, true
	case 1:
	default:
		// Two prompts claim one execution: no single prompt's answer is its,
		// and its retained final is not one either.
		item.FinalMessageID = ""
		return item, true
	}
	resolve := func(handle string) string {
		native, err := s.controlMetadata.resolvedNative(ctx, hosted, string(model.ProviderClaude), handle)
		if err != nil {
			return ""
		}
		return native
	}
	record := claims[0]
	answer, err := s.claudeAnswers(hosted).answer(ctx, claudeSettlementEntry(record.Outbox, record.NativeThreadID, "", resolve))
	switch {
	case err == nil:
		item.FinalMessageID, item.Outcome, item.NativeThreadID, item.SavedTurnID = answer.finalID, answer.outcome, answer.native, answer.turn
		return item, true
	case !errors.Is(err, history.ErrSavedTranscriptUnavailable) || item.Outcome == agentTurnCompleted:
		return item, false
	default:
		item.FinalMessageID = ""
		return item, true
	}
}

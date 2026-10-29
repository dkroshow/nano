package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/threads"
)

// NewExecutionStore uses normalized control.sqlite rows.
func NewExecutionStore(control *MetadataStore, runID string, featureAction func(string) bool) (*ExecutionStore, error) {
	if control == nil {
		return nil, fmt.Errorf("control execution store unavailable")
	}
	s := &ExecutionStore{runID: runID, sessions: map[string]threads.SessionRecord{}, turns: map[string]threads.TurnRecord{}, byAction: map[string]string{}, byClient: map[string]string{}, deletedSessions: map[string]bool{}, deletedTurns: map[string]bool{}, sessionBaseline: map[string]int64{}, turnBaseline: map[string]int64{}, changed: make(chan struct{}), changes: newAgentChangeLog(), control: control}
	s.resetTurnIndexesLocked()
	if err := loadControlExecutionRows(s); err != nil {
		return nil, err
	}
	// Apply the same restart honesty rules as the JSON predecessor, but commit
	// them only to SQLite.
	changed := false
	for id, rec := range s.sessions {
		if (rec.Status == agentSessionCreating || rec.Status == "starting") && rec.RunID != "" && rec.RunID != runID && rec.NativeThreadID == "" {
			rec.Status = agentSessionCreationUnknown
			rec.Error = "Nano restarted before provider session creation was confirmed"
			touchAgentSession(&rec)
			s.sessions[id] = rec
			changed = true
		} else if rec.RunID != "" && rec.RunID != runID && rec.Status != StatusExited {
			rec.Status = agentSessionDetached
			rec.NanoSessionID = ""
			rec.Error = ""
			touchAgentSession(&rec)
			s.sessions[id] = rec
			changed = true
		}
	}
	for _, rec := range s.turns {
		if rec.Status == agentTurnDeliveryPending && rec.RunID != "" && rec.RunID != runID && rec.ProviderTurnID == "" && !featureAction(rec.ClientActionID) {
			rec.Status = agentTurnDeliveryUnknown
			rec.Error = &threads.ExecutionError{Source: "delivery", Code: "nano_restarted", Message: "Nano restarted before provider acceptance was confirmed"}
			touchAgentTurn(&rec)
			s.setTurnLocked(rec.ID, &rec)
			changed = true
		}
	}
	if changed {
		if err := s.persistLocked(nil); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func loadControlExecutionRows(s *ExecutionStore) error {
	rows, err := s.control.db.Query(`SELECT e.hosted_thread_id,COALESCE(e.client_session_id,''),e.request_hash,h.provider,h.presentation_mode,h.sidebar_placement,COALESCE(e.nano_session_id,''),COALESCE(h.current_native_id,''),COALESCE(h.invoker_hosted_id,''),COALESCE(h.invoker_native_id,''),COALESCE(h.invoker_provider,''),h.notify_invoker_on_terminal,h.terminal_notification_policy,COALESCE(h.last_human_prompt_at,''),e.status,COALESCE(e.error,''),e.run_id,e.created_at,e.updated_at,e.revision FROM hosted_execution_sessions e JOIN hosted_threads h ON h.hosted_thread_id=e.hosted_thread_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r threads.SessionRecord
		var legacyPrompt bool
		var storedPolicy string
		if err := rows.Scan(&r.ID, &r.ClientSessionID, &r.RequestHash, &r.Provider, &r.PresentationMode, &r.SidebarPlacement, &r.NanoSessionID, &r.NativeThreadID, &r.InvokerHostedThreadID, &r.InvokerNativeThreadID, &r.InvokerProvider, &legacyPrompt, &storedPolicy, &r.LastHumanPromptAt, &r.Status, &r.Error, &r.RunID, &r.CreatedAt, &r.UpdatedAt, &r.Revision); err != nil {
			return err
		}
		setStoredSubscriptions(&r, storedPolicy, legacyPrompt)
		s.sessions[r.ID] = r
		s.sessionBaseline[r.ID] = r.Revision
		if r.ClientSessionID != "" {
			s.byClient[r.ClientSessionID] = r.ID
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = s.control.db.Query(`SELECT o.operation_id,o.hosted_thread_id,o.client_action_id,o.transport_action_id,o.prompt_hash,o.assignment_event_id,h.provider,COALESCE(e.nano_session_id,''),COALESCE(h.current_native_id,''),COALESCE(o.provider_turn_id,''),o.status,o.error_source,o.error_code,o.error_message,o.error_details_json,COALESCE(o.final_item_id,''),COALESCE(o.final_response,''),o.final_response_truncated,o.final_response_bytes,COALESCE(o.candidate_item_id,''),COALESCE(o.candidate_phase,''),COALESCE(o.candidate_response,''),o.candidate_bytes,o.candidate_truncated,COALESCE(o.origin,''),COALESCE(o.sender_hosted_id,''),COALESCE(o.sender_native_id,''),COALESCE(o.sender_provider,''),COALESCE(o.sender_name,''),COALESCE(o.sender_submitted_at,''),o.run_id,o.created_at,o.updated_at,o.revision FROM hosted_turn_operations o JOIN hosted_threads h ON h.hosted_thread_id=o.hosted_thread_id LEFT JOIN hosted_execution_sessions e ON e.hosted_thread_id=o.hosted_thread_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r threads.TurnRecord
		var source, code, msg, details sql.NullString
		var assignmentID sql.NullInt64
		if err := rows.Scan(&r.ID, &r.HostedThreadID, &r.ClientActionID, &r.TransportActionID, &r.PromptHash, &assignmentID, &r.Provider, &r.NanoSessionID, &r.NativeThreadID, &r.ProviderTurnID, &r.Status, &source, &code, &msg, &details, &r.FinalItemID, &r.FinalResponse, &r.FinalResponseTruncated, &r.FinalResponseBytes, &r.CandidateItemID, &r.CandidatePhase, &r.CandidateResponse, &r.CandidateBytes, &r.CandidateTruncated, &r.Origin, &r.SenderHostedThreadID, &r.SenderNativeThreadID, &r.SenderProvider, &r.SenderName, &r.SenderSubmittedAt, &r.RunID, &r.CreatedAt, &r.UpdatedAt, &r.Revision); err != nil {
			return err
		}
		if assignmentID.Valid {
			id := assignmentID.Int64
			r.AssignmentEventID = &id
		}
		if source.Valid {
			r.Error = &threads.ExecutionError{Source: source.String, Code: code.String, Message: msg.String, Details: json.RawMessage(details.String)}
		}
		s.setTurnLocked(r.ID, &r)
		s.turnBaseline[r.ID] = r.Revision
	}
	return rows.Err()
}

func setStoredSubscriptions(record *threads.SessionRecord, policy string, legacyPrompt bool) {
	choices := subscriptions.FromStored(policy, legacyPrompt)
	record.SubscriptionInbox = choices.Inbox == subscriptions.Send
	record.SubscriptionPrompt = choices.Prompt == subscriptions.Send
}

// StoredSubscriptions reads subscription choices while honoring the legacy stored policy.
func StoredSubscriptions(record threads.SessionRecord) subscriptions.Choices {
	choices := subscriptions.Choices{Inbox: subscriptions.None, Prompt: subscriptions.None}
	if record.SubscriptionInbox {
		choices.Inbox = subscriptions.Send
	}
	if record.SubscriptionPrompt {
		choices.Prompt = subscriptions.Send
	}
	return choices
}

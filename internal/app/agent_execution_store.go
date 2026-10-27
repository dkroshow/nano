package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/threads"
	"log"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	presentationShared         = "shared"
	presentationBackground     = "background"
	sidebarPlacementInvoker    = "invoker"
	sidebarPlacementStandalone = "standalone"

	agentSessionCreating        = "creating"
	agentSessionCreationUnknown = "creation_unknown"
	agentSessionDetached        = "detached"

	agentTurnQueued          = "queued"
	agentTurnDeliveryPending = "delivery_pending"
	agentTurnDeliveryUnknown = "delivery_unknown"
	agentTurnRunning         = "running"
	agentTurnWaiting         = "waiting_for_input"
	agentTurnAwaitingSaved   = "awaiting_saved"
	agentTurnCompleted       = "completed"
	agentTurnFailed          = "failed"
	agentTurnInterrupted     = "interrupted"

	maxAgentResultBytes = 8 << 20
	maxAgentTurns       = 100
)

var (
	errClientSessionIdempotencyConflict = errors.New("client session idempotency conflict")
	errAgentTurnIdempotencyConflict     = errors.New("clientActionId was already used with a different prompt")
)

type agentExecutionError = threads.ExecutionError

type agentSessionRecord = threads.SessionRecord

type agentTurnRecord = threads.TurnRecord

// agentExecutionStore is the small durable control-plane record shared by all
// hosted threads. It deliberately stores prompt hashes, never prompt text.
// Final responses are provider output and are bounded explicitly at 8 MiB.
type agentExecutionStore struct {
	mu                 sync.RWMutex
	runID              string
	sessions           map[string]agentSessionRecord
	turns              map[string]agentTurnRecord
	turnsByHosted      map[string]map[string]bool
	dirtyTurnThreads   map[string]bool
	dirtyTurns         map[string]bool
	attentionDirty     map[string]bool
	attentionCache     map[string]bool
	byAction           map[string]string
	byClient           map[string]string
	deletedSessions    map[string]bool
	deletedTurns       map[string]bool
	sessionBaseline    map[string]int64
	turnBaseline       map[string]int64
	pendingTransitions []agentOperationTransition
	changed            chan struct{}
	// changes is the in-memory semantic-transition ring behind the
	// turn-operations changes feed (agent_execution_changes.go). Loss-tolerant
	// by design; the durable records remain the truth.
	changes *agentChangeLog
	// control owns every durable mutation in its SQLite execution snapshot.
	control *controlMetadataStore
	// pruneEligible is the prompt owner's answer to which surplus terminal
	// operations' prompts can never owe a member completion, so their evidence
	// may go. Until it is set, retention keeps every operation.
	pruneEligible       func(ctx context.Context, hostedID string, transportActionIDs []string) (map[string]bool, error)
	savedResultRead     func(context.Context, agentTurnRecord) (string, string, error)
	savedResultJobs     map[string]*savedResultJob
	savedResultFinished func(string)
}

// pruneEligibleReadTimeout bounds each prompt-owner eligibility read made
// while retention selects what to prune, and maxPruneEligibleBatch, the prompt
// owner's own bound, the operations one read asks about.
const (
	pruneEligibleReadTimeout = 2 * time.Second
	maxPruneEligibleBatch    = 32
)

// newControlAgentExecutionStore uses normalized control.sqlite rows.
func newControlAgentExecutionStore(control *controlMetadataStore, runID string) (*agentExecutionStore, error) {
	if control == nil {
		return nil, fmt.Errorf("control execution store unavailable")
	}
	s := &agentExecutionStore{runID: runID, sessions: map[string]agentSessionRecord{}, turns: map[string]agentTurnRecord{}, byAction: map[string]string{}, byClient: map[string]string{}, deletedSessions: map[string]bool{}, deletedTurns: map[string]bool{}, sessionBaseline: map[string]int64{}, turnBaseline: map[string]int64{}, changed: make(chan struct{}), changes: newAgentChangeLog(), control: control}
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
		} else if rec.RunID != "" && rec.RunID != runID && rec.Status != sessionStatusExited {
			rec.Status = agentSessionDetached
			rec.NanoSessionID = ""
			rec.Error = ""
			touchAgentSession(&rec)
			s.sessions[id] = rec
			changed = true
		}
	}
	for _, rec := range s.turns {
		if rec.Status == agentTurnDeliveryPending && rec.RunID != "" && rec.RunID != runID && rec.ProviderTurnID == "" && !serverFeatureAction(rec.ClientActionID) {
			rec.Status = agentTurnDeliveryUnknown
			rec.Error = &agentExecutionError{Source: "delivery", Code: "nano_restarted", Message: "Nano restarted before provider acceptance was confirmed"}
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

func loadControlExecutionRows(s *agentExecutionStore) error {
	rows, err := s.control.db.Query(`SELECT e.hosted_thread_id,COALESCE(e.client_session_id,''),e.request_hash,h.provider,h.presentation_mode,h.sidebar_placement,COALESCE(e.nano_session_id,''),COALESCE(h.current_native_id,''),COALESCE(h.invoker_hosted_id,''),COALESCE(h.invoker_native_id,''),COALESCE(h.invoker_provider,''),h.notify_invoker_on_terminal,h.terminal_notification_policy,COALESCE(h.last_human_prompt_at,''),e.status,COALESCE(e.error,''),e.run_id,e.created_at,e.updated_at,e.revision FROM hosted_execution_sessions e JOIN hosted_threads h ON h.hosted_thread_id=e.hosted_thread_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r agentSessionRecord
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
		var r agentTurnRecord
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
			r.Error = &agentExecutionError{Source: source.String, Code: code.String, Message: msg.String, Details: json.RawMessage(details.String)}
		}
		s.setTurnLocked(r.ID, &r)
		s.turnBaseline[r.ID] = r.Revision
	}
	return rows.Err()
}

func setStoredSubscriptions(record *agentSessionRecord, policy string, legacyPrompt bool) {
	choices := subscriptions.FromStored(policy, legacyPrompt)
	record.SubscriptionInbox = choices.Inbox == subscriptions.Send
	record.SubscriptionPrompt = choices.Prompt == subscriptions.Send
}

func storedSubscriptions(record agentSessionRecord) subscriptions.Choices {
	choices := subscriptions.Choices{Inbox: subscriptions.None, Prompt: subscriptions.None}
	if record.SubscriptionInbox {
		choices.Inbox = subscriptions.Send
	}
	if record.SubscriptionPrompt {
		choices.Prompt = subscriptions.Send
	}
	return choices
}

func normalizePresentationMode(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "", presentationShared:
		return presentationShared, nil
	case presentationBackground:
		return presentationBackground, nil
	default:
		return "", fmt.Errorf("presentationMode must be %q or %q", presentationShared, presentationBackground)
	}
}

func normalizeSidebarPlacement(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "", sidebarPlacementInvoker:
		return sidebarPlacementInvoker, nil
	case sidebarPlacementStandalone:
		return sidebarPlacementStandalone, nil
	default:
		return "", fmt.Errorf("sidebarPlacement must be %q or %q", sidebarPlacementInvoker, sidebarPlacementStandalone)
	}
}

func agentHash(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func agentActionKey(sessionID, actionID string) string { return sessionID + "\x00" + actionID }

func agentOperationID(sessionID, actionID string) string {
	sum := sha256.Sum256([]byte(agentActionKey(sessionID, actionID)))
	return "aop_" + hex.EncodeToString(sum[:12])
}

func agentTransportActionID(sessionID, actionID string) string {
	sum := sha256.Sum256([]byte(agentActionKey(sessionID, actionID)))
	return "agent-" + hex.EncodeToString(sum[:16])
}

func mintHostedThreadID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("mint hosted thread id: %w", err)
	}
	return "nht_" + hex.EncodeToString(value[:]), nil
}

// beginSession idempotently creates one ordinary hosted-thread record. The
// caller id is only an optional creation alias; the server-owned record id is
// what every control surface uses after creation.
func (s *agentExecutionStore) beginSession(clientSessionID, requestHash, provider, presentationMode string) (agentSessionRecord, bool, error) {
	return s.beginSessionWithSubscriptions(clientSessionID, requestHash, provider, presentationMode, agentSessionRecord{}, subscriptions.Choices{Inbox: subscriptions.None, Prompt: subscriptions.None})
}

// beginSessionWithInvoker adds the durable invocation edge used by agent API
// creation. The edge names an ordinary hosted thread, not a provider-native
// parent, so it remains valid when Codex invokes Claude or Grok (and vice
// versa). beginSession remains the zero-edge helper for human-created sessions
// and existing tests.
func (s *agentExecutionStore) beginSessionWithInvoker(clientSessionID, requestHash, provider, presentationMode string, invoker agentSessionRecord, assignment ...*orchestrationAssignment) (agentSessionRecord, bool, error) {
	return s.beginSessionWithSubscriptions(clientSessionID, requestHash, provider, presentationMode, invoker, subscriptions.Choices{Inbox: subscriptions.Send, Prompt: subscriptions.None}, assignment...)
}

func (s *agentExecutionStore) beginSessionWithSubscriptions(clientSessionID, requestHash, provider, presentationMode string, invoker agentSessionRecord, choices subscriptions.Choices, assignment ...*orchestrationAssignment) (agentSessionRecord, bool, error) {
	return s.beginSessionWithSubscriptionsAndPlacement(clientSessionID, requestHash, provider, presentationMode, sidebarPlacementInvoker, invoker, choices, assignment...)
}

func (s *agentExecutionStore) beginSessionWithSubscriptionsAndPlacement(clientSessionID, requestHash, provider, presentationMode, sidebarPlacement string, invoker agentSessionRecord, choices subscriptions.Choices, assignment ...*orchestrationAssignment) (agentSessionRecord, bool, error) {
	return s.beginNamedSessionWithSubscriptionsAndPlacement(clientSessionID, requestHash, provider, presentationMode, sidebarPlacement, "", invoker, choices, assignment...)
}

func (s *agentExecutionStore) beginNamedSessionWithSubscriptionsAndPlacement(clientSessionID, requestHash, provider, presentationMode, sidebarPlacement, creationName string, invoker agentSessionRecord, choices subscriptions.Choices, assignment ...*orchestrationAssignment) (agentSessionRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id := s.byClient[clientSessionID]; clientSessionID != "" && id != "" {
		current := s.sessions[id]
		if current.RequestHash != requestHash {
			return agentSessionRecord{}, false, fmt.Errorf("%w: clientSessionId was already used with different session parameters", errClientSessionIdempotencyConflict)
		}
		if creationName != "" && s.control != nil {
			if err := s.control.recordCreationName(id, creationName); err != nil {
				return agentSessionRecord{}, false, err
			}
		}
		return current, false, nil
	}
	id, err := mintHostedThreadID()
	if err != nil {
		return agentSessionRecord{}, false, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rec := agentSessionRecord{
		ID: id, ClientSessionID: clientSessionID, RequestHash: requestHash,
		Provider: provider, PresentationMode: presentationMode, SidebarPlacement: sidebarPlacement,
		InvokerHostedThreadID: invoker.ID, InvokerNativeThreadID: invoker.NativeThreadID,
		InvokerProvider:    invoker.Provider,
		SubscriptionInbox:  choices.Inbox == subscriptions.Send,
		SubscriptionPrompt: choices.Prompt == subscriptions.Send,
		Status:             agentSessionCreating, RunID: s.runID, CreatedAt: now, UpdatedAt: now, Revision: 1,
	}
	if s.control != nil {
		if provider == "" || provider == "unknown" {
			return agentSessionRecord{}, false, fmt.Errorf("hosted thread provider is required")
		}
		mode, err := normalizePresentationMode(presentationMode)
		if err != nil {
			return agentSessionRecord{}, false, err
		}
		rec.PresentationMode = mode
		placement, err := normalizeSidebarPlacement(sidebarPlacement)
		if err != nil {
			return agentSessionRecord{}, false, err
		}
		rec.SidebarPlacement = placement
		tx, err := s.control.db.Begin()
		if err != nil {
			return agentSessionRecord{}, false, err
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`INSERT INTO hosted_threads(hosted_thread_id,provider,presentation_mode,sidebar_placement,invoker_hosted_id,invoker_native_id,invoker_provider,notify_invoker_on_terminal,terminal_notification_policy,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, rec.ID, rec.Provider, rec.PresentationMode, rec.SidebarPlacement, nullableText(rec.InvokerHostedThreadID), nullableText(rec.InvokerNativeThreadID), nullableText(rec.InvokerProvider), rec.SubscriptionPrompt, subscriptions.StoredPolicy(choices), 1, now, now); err != nil {
			return agentSessionRecord{}, false, err
		}
		if _, err = tx.Exec(`INSERT INTO hosted_execution_sessions(hosted_thread_id,client_session_id,request_hash,nano_session_id,status,error,run_id,created_at,updated_at,revision) VALUES(?,?,?,?,?,?,?,?,?,?)`, rec.ID, nullableText(rec.ClientSessionID), rec.RequestHash, nil, rec.Status, nil, rec.RunID, rec.CreatedAt, rec.UpdatedAt, rec.Revision); err != nil {
			return agentSessionRecord{}, false, err
		}
		if err = subscriptions.InsertInitialCursor(tx, rec.ID, now); err != nil {
			return agentSessionRecord{}, false, err
		}
		assignmentRevision := int64(1)
		if creationName != "" {
			validated, validateErr := validateCreationName(creationName)
			if validateErr != nil {
				return agentSessionRecord{}, false, validateErr
			}
			if _, _, err = threads.RecordCreationNameTx(context.Background(), tx, rec.ID, validated, "creation_request", now); err != nil {
				return agentSessionRecord{}, false, err
			}
			assignmentRevision = 2
		}
		if len(assignment) > 0 && assignment[0] != nil {
			if _, assignmentRevision, err = appendInitialOrchestrationAssignmentTx(tx, rec.ID, assignment[0], now); err != nil {
				return agentSessionRecord{}, false, err
			}
		}
		if err = tx.Commit(); err != nil {
			return agentSessionRecord{}, false, err
		}
		s.sessions[id] = rec
		s.sessionBaseline[id] = rec.Revision
		metadataRevision, changedFields := int64(1), []string{"identity"}
		if creationName != "" {
			metadataRevision = 2
			changedFields = append(changedFields, threads.CreationNameField)
		}
		if len(assignment) > 0 && assignment[0] != nil {
			metadataRevision = assignmentRevision
			changedFields = append(changedFields, orchestrationAssignmentField)
		}
		s.control.publishChange(id, metadataRevision, changedFields...)
		if clientSessionID != "" {
			s.byClient[clientSessionID] = id
		}
		close(s.changed)
		s.changed = make(chan struct{})
		return rec, true, nil
	}
	s.sessions[id] = rec
	if clientSessionID != "" {
		s.byClient[clientSessionID] = id
	}
	if err := s.persistAndNotifyLocked(); err != nil {
		return agentSessionRecord{}, false, err
	}
	return rec, true, nil
}

// ensureSession gives UI-created and resumed sessions the same durable
// execution identity as API-created sessions. It never creates a caller alias
// and therefore carries no creation-idempotency semantics.
func (s *agentExecutionStore) ensureSession(id, provider, presentationMode string) (agentSessionRecord, error) {
	return s.ensureSessionWithIdentity(id, provider, presentationMode, "", "", "")
}

// ensureSessionWithIdentity is the human New Window counterpart to API
// creation. In control mode it creates (or validates) immutable hosted
// identity and its execution-runtime row in one transaction before any
// provider transport can be started.
func (s *agentExecutionStore) ensureSessionWithIdentity(id, provider, presentationMode, invokerHosted, invokerNative, invokerProvider string) (agentSessionRecord, error) {
	return s.ensureNamedSessionWithIdentity(id, provider, presentationMode, invokerHosted, invokerNative, invokerProvider, "")
}

func (s *agentExecutionStore) ensureNamedSessionWithIdentity(id, provider, presentationMode, invokerHosted, invokerNative, invokerProvider, creationName string) (agentSessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "" {
		if rec, ok := s.sessions[id]; ok {
			// Presentation is deliberately absent from this identity check: the
			// mode is mutable (setPresentation) and the canonical record wins,
			// so a resume carrying the spawn-time mode is not a conflict.
			if rec.Provider != "" && rec.Provider != "unknown" && provider != "" && rec.Provider != provider {
				return agentSessionRecord{}, fmt.Errorf("hosted thread %s belongs to provider %s, not %s", id, rec.Provider, provider)
			}
			// Explorer and boot resume requests do not repeat lineage. An entirely
			// omitted invoker tuple means "inherit the canonical identity"; the
			// caller receives rec below and spawnSession copies it into the runtime.
			// Any supplied tuple is still an immutable identity assertion and must
			// match in full.
			invokerSpecified := invokerHosted != "" || invokerNative != "" || invokerProvider != ""
			if invokerSpecified && (rec.InvokerHostedThreadID != invokerHosted || rec.InvokerNativeThreadID != invokerNative || rec.InvokerProvider != invokerProvider) {
				return agentSessionRecord{}, fmt.Errorf("hosted thread %s has a different invoker identity", id)
			}
			if creationName != "" && s.control != nil {
				if err := s.control.recordCreationName(id, creationName); err != nil {
					return agentSessionRecord{}, err
				}
			}
			return rec, nil
		}
	} else {
		var err error
		id, err = mintHostedThreadID()
		if err != nil {
			return agentSessionRecord{}, err
		}
	}
	if s.control != nil {
		if provider == "" || provider == "unknown" {
			return agentSessionRecord{}, fmt.Errorf("hosted thread provider is required")
		}
		mode, err := normalizePresentationMode(presentationMode)
		if err != nil {
			return agentSessionRecord{}, err
		}
		if id == "" {
			id, err = mintHostedThreadID()
			if err != nil {
				return agentSessionRecord{}, err
			}
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		rec := agentSessionRecord{ID: id, Provider: provider, PresentationMode: mode, SidebarPlacement: sidebarPlacementInvoker, InvokerHostedThreadID: invokerHosted, InvokerNativeThreadID: invokerNative, InvokerProvider: invokerProvider, Status: agentSessionCreating, RunID: s.runID, CreatedAt: now, UpdatedAt: now, Revision: 1}
		tx, err := s.control.db.Begin()
		if err != nil {
			return agentSessionRecord{}, err
		}
		defer tx.Rollback()
		var existingProvider, existingNative, existingMode, existingPlacement string
		var existingInvokerHosted, existingInvokerNative, existingInvokerProvider string
		err = tx.QueryRow(`SELECT provider,COALESCE(current_native_id,''),presentation_mode,sidebar_placement,COALESCE(invoker_hosted_id,''),COALESCE(invoker_native_id,''),COALESCE(invoker_provider,'') FROM hosted_threads WHERE hosted_thread_id=?`, id).Scan(&existingProvider, &existingNative, &existingMode, &existingPlacement, &existingInvokerHosted, &existingInvokerNative, &existingInvokerProvider)
		existingIdentity := err == nil
		if err == sql.ErrNoRows {
			if _, err = tx.Exec(`INSERT INTO hosted_threads(hosted_thread_id,provider,presentation_mode,invoker_hosted_id,invoker_native_id,invoker_provider,notify_invoker_on_terminal,terminal_notification_policy,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,0,'none',1,?,?)`, id, provider, mode, nullableText(invokerHosted), nullableText(invokerNative), nullableText(invokerProvider), now, now); err != nil {
				return agentSessionRecord{}, err
			}
		} else {
			if err != nil {
				return agentSessionRecord{}, err
			}
			if existingProvider != provider {
				return agentSessionRecord{}, fmt.Errorf("hosted thread %s belongs to provider %s, not %s", id, existingProvider, provider)
			}
			invokerSpecified := invokerHosted != "" || invokerNative != "" || invokerProvider != ""
			if invokerSpecified && (existingInvokerHosted != invokerHosted || existingInvokerNative != invokerNative || existingInvokerProvider != invokerProvider) {
				return agentSessionRecord{}, fmt.Errorf("hosted thread %s has a different invoker identity", id)
			}
			rec.NativeThreadID = existingNative
			rec.PresentationMode = existingMode
			rec.SidebarPlacement = existingPlacement
			rec.InvokerHostedThreadID = existingInvokerHosted
			rec.InvokerNativeThreadID = existingInvokerNative
			rec.InvokerProvider = existingInvokerProvider
		}
		if _, err = tx.Exec(`INSERT INTO hosted_execution_sessions(hosted_thread_id,client_session_id,request_hash,nano_session_id,status,error,run_id,created_at,updated_at,revision) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(hosted_thread_id) DO NOTHING`, id, nil, "", nil, rec.Status, nil, rec.RunID, rec.CreatedAt, rec.UpdatedAt, rec.Revision); err != nil {
			return agentSessionRecord{}, err
		}
		if creationName != "" {
			validated, validateErr := validateCreationName(creationName)
			if validateErr != nil {
				return agentSessionRecord{}, validateErr
			}
			if _, _, err = threads.RecordCreationNameTx(context.Background(), tx, id, validated, "creation_request", now); err != nil {
				return agentSessionRecord{}, err
			}
		}
		if err = tx.Commit(); err != nil {
			return agentSessionRecord{}, err
		}
		if existingIdentity {
			canonical, ok, err := s.sessionFromCanonicalLocked(id)
			if err != nil {
				return agentSessionRecord{}, err
			}
			if !ok {
				return agentSessionRecord{}, fmt.Errorf("hosted thread %s disappeared during resume", id)
			}
			rec = canonical
		}
		s.sessions[id] = rec
		s.sessionBaseline[id] = rec.Revision
		var metadataRevision int64
		if err := s.control.db.QueryRow(`SELECT revision FROM hosted_threads WHERE hosted_thread_id=?`, id).Scan(&metadataRevision); err != nil {
			return agentSessionRecord{}, err
		}
		changedFields := []string{"identity"}
		if creationName != "" {
			changedFields = append(changedFields, threads.CreationNameField)
		}
		s.control.publishChange(id, metadataRevision, changedFields...)
		return rec, nil
	}
	return agentSessionRecord{}, fmt.Errorf("hosted thread store unavailable")
}

func (s *agentExecutionStore) bindSession(id, nanoSessionID, nativeThreadID, status, presentationMode string) error {
	if s == nil || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.sessions[id]
	if !ok {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		rec = agentSessionRecord{ID: id, Provider: "unknown", PresentationMode: presentationMode, CreatedAt: now, RunID: s.runID}
	}
	if presentationMode == "" {
		presentationMode = presentationShared
	}
	rec.NanoSessionID = nanoSessionID
	if nativeThreadID != "" {
		rec.NativeThreadID = nativeThreadID
		if s.control != nil && rec.Provider != "" && rec.Provider != "unknown" {
			if err := skipOwnedNativeClaim(s.control.claimNativeID(id, rec.Provider, nativeThreadID, nativeThreadIDProvisional(rec.Provider, nativeThreadID))); err != nil {
				return err
			}
		}
	}
	if status != "" {
		rec.Status = status
	}
	// The canonical mode (control.sqlite, mirrored in this cache) wins over the
	// session's spawn-time value: a thread flipped to background while detached
	// must not snap back to shared just because a resume rebinds it.
	if rec.PresentationMode == "" {
		rec.PresentationMode = presentationMode
	}
	rec.RunID = s.runID
	rec.Error = ""
	touchAgentSession(&rec)
	s.sessions[id] = rec
	for turnID := range s.turnsByHosted[id] {
		turn := s.turns[turnID]
		if agentTurnTerminal(turn.Status) {
			continue
		}
		turn.NanoSessionID = nanoSessionID
		if nativeThreadID != "" {
			turn.NativeThreadID = nativeThreadID
		}
		touchAgentTurn(&turn)
		s.setTurnLocked(turn.ID, &turn)
	}
	return s.persistAndNotifyLocked()
}

// setPresentation flips a hosted thread's presentation mode. control.sqlite
// commits first and is the single durable authority; the in-memory record is
// synchronized under the same store lock so no reader can observe the cache
// ahead of the committed row. Same-value calls are no-ops end to end (no
// revision bump, no publish). Returns the updated record, the hosted revision,
// and whether anything changed.
func (s *agentExecutionStore) setPresentation(id, mode string) (agentSessionRecord, int64, bool, error) {
	normalized, err := normalizePresentationMode(mode)
	if err != nil {
		return agentSessionRecord{}, 0, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.sessions[id]
	if !ok {
		return agentSessionRecord{}, 0, false, fmt.Errorf("unknown hosted thread %s", id)
	}
	var revision int64
	changed := rec.PresentationMode != normalized
	if s.control != nil {
		revision, changed, err = s.control.setPresentationMode(id, normalized)
		if err != nil {
			return agentSessionRecord{}, 0, false, err
		}
	}
	if rec.PresentationMode != normalized {
		rec.PresentationMode = normalized
		s.sessions[id] = rec
	}
	return rec, revision, changed, nil
}

// Canonical columns own subscription choices; this cache follows their commit.
func (s *agentExecutionStore) setSubscriptions(id string, choices subscriptions.Choices) (agentSessionRecord, int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.sessions[id]
	if !ok {
		return agentSessionRecord{}, 0, false, fmt.Errorf("unknown hosted thread %s", id)
	}
	revision, changed, err := s.control.setSubscriptions(id, choices)
	if err != nil {
		return agentSessionRecord{}, 0, false, err
	}
	setStoredSubscriptions(&rec, subscriptions.StoredPolicy(choices), choices.Prompt == subscriptions.Send)
	s.sessions[id] = rec
	return rec, revision, changed, nil
}

func (s *agentExecutionStore) setSidebarPlacement(id, placement string) (agentSessionRecord, int64, bool, error) {
	normalized, err := normalizeSidebarPlacement(placement)
	if err != nil {
		return agentSessionRecord{}, 0, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.sessions[id]
	if !ok {
		return agentSessionRecord{}, 0, false, fmt.Errorf("unknown hosted thread %s", id)
	}
	var revision int64
	changed := rec.SidebarPlacement != normalized
	if s.control != nil {
		revision, changed, err = s.control.setSidebarPlacement(id, normalized)
		if err != nil {
			return agentSessionRecord{}, 0, false, err
		}
	}
	if rec.SidebarPlacement != normalized {
		rec.SidebarPlacement = normalized
		s.sessions[id] = rec
	}
	return rec, revision, changed, nil
}

// presentationModeFor reads the canonical mode from the execution cache, which
// mirrors control.sqlite. Detached-runtime projection and boot reattach use it
// instead of trusting attachmentRecord.PresentationMode — the attachment JSON
// is a transport bridge, never presentation authority.
func (s *agentExecutionStore) presentationModeFor(id string) (string, bool) {
	mode, ok := s.presentationModes([]string{id})[id]
	return mode, ok
}

// presentationModes returns the known presentation mode of each id under one
// lock: a caller reading many threads waits for the store once, not once per
// thread behind every write.
func (s *agentExecutionStore) presentationModes(ids []string) map[string]string {
	modes := map[string]string{}
	if s == nil || len(ids) == 0 {
		return modes
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, id := range ids {
		if rec, ok := s.sessions[id]; ok && id != "" && rec.PresentationMode != "" {
			modes[id] = rec.PresentationMode
		}
	}
	return modes
}

func (s *agentExecutionStore) updateSession(id, nanoSessionID, nativeThreadID, status, message string) error {
	if s == nil || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.sessions[id]
	if !ok {
		return nil
	}
	next := rec
	if nanoSessionID != "" {
		next.NanoSessionID = nanoSessionID
	}
	if nativeThreadID != "" {
		next.NativeThreadID = nativeThreadID
	}
	if status != "" {
		next.Status = status
	}
	next.Error = message
	next.RunID = s.runID
	// An update that changes nothing saves nothing: each spawn repeats its
	// status, and every save runs under the lock that boot registrations,
	// spawns and HTTP reads all wait for.
	changed := next.NanoSessionID != rec.NanoSessionID || next.NativeThreadID != rec.NativeThreadID ||
		next.Status != rec.Status || next.Error != rec.Error || next.RunID != rec.RunID
	if changed {
		touchAgentSession(&next)
		s.sessions[id] = next
	}
	for turnID := range s.turnsByHosted[id] {
		turn := s.turns[turnID]
		if agentTurnTerminal(turn.Status) {
			continue
		}
		native := turn.NativeThreadID
		if next.NativeThreadID != "" {
			native = next.NativeThreadID
		}
		if turn.NanoSessionID == next.NanoSessionID && turn.NativeThreadID == native {
			continue
		}
		turn.NanoSessionID, turn.NativeThreadID = next.NanoSessionID, native
		touchAgentTurn(&turn)
		s.setTurnLocked(turn.ID, &turn)
		changed = true
	}
	if !changed {
		return nil
	}
	return s.persistAndNotifyLocked()
}

// hasNonTerminalTurns reports whether any operation on the hosted thread still
// lacks terminal evidence. Detach refuses in that case: closing the attachment
// mid-operation would force Nano to invent turn evidence, and detach's whole
// meaning is that nothing about the work is being asserted.
func (s *agentExecutionStore) hasNonTerminalTurns(sessionID string) bool {
	if s == nil || sessionID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.turns {
		if rec.HostedThreadID == sessionID && !agentTurnTerminal(rec.Status) {
			return true
		}
	}
	return false
}

// removeSession erases the durable control record and every operation it owns
// — the destroy half of the close/delete verb split. persistLocked rebuilds
// the byAction index from the surviving turns.
func (s *agentExecutionStore) removeSession(id string) error {
	if s == nil || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return nil
	}
	delete(s.sessions, id)
	s.deletedSessions[id] = true
	for turnID := range s.turnsByHosted[id] {
		s.setTurnLocked(turnID, nil)
	}
	return s.persistAndNotifyLocked()
}

func (s *agentExecutionStore) session(id string) (agentSessionRecord, bool) {
	if s == nil {
		return agentSessionRecord{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.sessions[id]
	return rec, ok
}

func (s *agentExecutionStore) resolveSession(ref string) (agentSessionRecord, bool) {
	if s == nil {
		return agentSessionRecord{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if rec, ok := s.sessions[ref]; ok {
		return rec, true
	}
	if id := s.byClient[ref]; id != "" {
		rec, ok := s.sessions[id]
		return rec, ok
	}
	var matched agentSessionRecord
	for _, rec := range s.sessions {
		if rec.NativeThreadID != ref {
			continue
		}
		if matched.ID == "" {
			matched = rec
			continue
		}
		if matched.ID == rec.ID {
			continue
		}
		// A currently hosted record wins over an older closed attachment of
		// the same provider-native thread. Two live matches stay ambiguous.
		if matched.Status == sessionStatusExited && rec.Status != sessionStatusExited {
			matched = rec
			continue
		}
		if rec.Status == sessionStatusExited && matched.Status != sessionStatusExited {
			continue
		}
		return agentSessionRecord{}, false
	}
	return matched, matched.ID != ""
}

// sessionForNative returns the exact current owner of a catalog identity.
// Retired history remains available through sessionsForNativeHistory, but cannot
// become current routing authority when the active claim is absent or invalid.
func (s *agentExecutionStore) sessionForNative(provider, nativeThreadID string) (agentSessionRecord, bool) {
	if s == nil || s.control == nil || provider == "" || nativeThreadID == "" {
		return agentSessionRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hostedID, err := s.control.activeNativeOwner(provider, nativeThreadID)
	if err != nil || hostedID == "" {
		return agentSessionRecord{}, false
	}
	record, ok, err := s.sessionFromCanonicalLocked(hostedID)
	return record, ok && err == nil
}

// sessionsForNativeHistory returns every hosted record that has ever claimed
// this exact provider/native pair. The active claim remains unique in SQLite;
// retired rows deliberately stay visible here because a deep link may name an
// older native identity. Callers must reject more than one result rather than
// guessing from timestamps.
func (s *agentExecutionStore) sessionsForNativeHistory(provider, nativeThreadID string) ([]agentSessionRecord, error) {
	if s == nil || provider == "" || nativeThreadID == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.control != nil {
		// Read canonical rows directly instead of treating the process-local
		// execution cache as a second authority. This includes retired native-id
		// claims and survives a store that has not loaded every historical record.
		rows, err := s.control.db.Query(`SELECT h.hosted_thread_id,COALESCE(e.client_session_id,''),COALESCE(e.request_hash,''),h.provider,h.presentation_mode,h.sidebar_placement,COALESCE(e.nano_session_id,''),COALESCE(h.current_native_id,''),COALESCE(h.invoker_hosted_id,''),COALESCE(h.invoker_native_id,''),COALESCE(h.invoker_provider,''),h.notify_invoker_on_terminal,h.terminal_notification_policy,COALESCE(e.status,'detached'),COALESCE(e.error,''),COALESCE(e.run_id,''),COALESCE(e.created_at,h.created_at),COALESCE(e.updated_at,h.updated_at),COALESCE(e.revision,h.revision) FROM hosted_thread_native_ids n JOIN hosted_threads h ON h.hosted_thread_id=n.hosted_thread_id LEFT JOIN hosted_execution_sessions e ON e.hosted_thread_id=h.hosted_thread_id WHERE n.provider=? AND n.native_thread_id=? ORDER BY h.hosted_thread_id`, provider, nativeThreadID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		result := make([]agentSessionRecord, 0)
		for rows.Next() {
			var record agentSessionRecord
			var legacyPrompt bool
			var storedPolicy string
			if err := rows.Scan(&record.ID, &record.ClientSessionID, &record.RequestHash, &record.Provider, &record.PresentationMode, &record.SidebarPlacement, &record.NanoSessionID, &record.NativeThreadID, &record.InvokerHostedThreadID, &record.InvokerNativeThreadID, &record.InvokerProvider, &legacyPrompt, &storedPolicy, &record.Status, &record.Error, &record.RunID, &record.CreatedAt, &record.UpdatedAt, &record.Revision); err != nil {
				return nil, err
			}
			setStoredSubscriptions(&record, storedPolicy, legacyPrompt)
			result = append(result, record)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return result, nil
	}
	result := make([]agentSessionRecord, 0)
	for _, rec := range s.sessions {
		if rec.NativeThreadID == nativeThreadID && rec.Provider == provider {
			result = append(result, rec)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// nativeHistoryProviders exposes only the provider keys for an exact native
// id. Callers use one of those keys with sessionsForNativeHistory: native ids
// are never looked up as provider-less hosted identities.
func (s *agentExecutionStore) nativeHistoryProviders(nativeThreadID string) ([]string, error) {
	if s == nil || nativeThreadID == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	providers := make(map[string]bool)
	if s.control != nil {
		rows, err := s.control.db.Query(`SELECT DISTINCT provider FROM hosted_thread_native_ids WHERE native_thread_id=?`, nativeThreadID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var provider string
			if err := rows.Scan(&provider); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if provider != "" {
				providers[provider] = true
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	} else {
		for _, rec := range s.sessions {
			if rec.NativeThreadID == nativeThreadID && rec.Provider != "" {
				providers[rec.Provider] = true
			}
		}
	}
	result := make([]string, 0, len(providers))
	for provider := range providers {
		result = append(result, provider)
	}
	sort.Strings(result)
	return result, nil
}

// nativeIDsForHosted lists every native id a hosted thread has ever claimed,
// current first. Identity resolve uses this to keep a user-assigned name that
// is still keyed to a predecessor Claude native id.
func (s *agentExecutionStore) nativeIDsForHosted(hostedID string) ([]string, error) {
	if s == nil || hostedID == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.control != nil {
		rows, err := s.control.db.Query(`SELECT native_thread_id,last_seen_at FROM hosted_thread_native_ids WHERE hosted_thread_id=?`, hostedID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		type seenNative struct{ id, lastSeen string }
		natives := make([]seenNative, 0)
		for rows.Next() {
			var native seenNative
			if err := rows.Scan(&native.id, &native.lastSeen); err != nil {
				return nil, err
			}
			natives = append(natives, native)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		// Most recently seen first, by time: SQL would order the text.
		sort.Slice(natives, func(i, j int) bool {
			if a, b := parseIdentityTime(natives[i].lastSeen), parseIdentityTime(natives[j].lastSeen); !a.Equal(b) {
				return b.Before(a)
			}
			return natives[i].id < natives[j].id
		})
		ids := make([]string, 0, len(natives))
		seen := map[string]bool{}
		for _, native := range natives {
			if native.id != "" && !seen[native.id] {
				seen[native.id] = true
				ids = append(ids, native.id)
			}
		}
		return ids, nil
	}
	if rec, ok := s.sessions[hostedID]; ok && rec.NativeThreadID != "" {
		return []string{rec.NativeThreadID}, nil
	}
	return nil, nil
}

// sessionFromCanonical returns one exact hosted record from control.sqlite.
// Unlike session, this remains correct when the in-memory registry contains
// only the currently attached subset of durable hosted history.
func (s *agentExecutionStore) sessionFromCanonical(hostedID string) (agentSessionRecord, bool, error) {
	if s == nil || hostedID == "" {
		return agentSessionRecord{}, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessionFromCanonicalLocked(hostedID)
}

func (s *agentExecutionStore) sessionFromCanonicalLocked(hostedID string) (agentSessionRecord, bool, error) {
	if s.control == nil {
		record, ok := s.sessions[hostedID]
		return record, ok, nil
	}
	var record agentSessionRecord
	var legacyPrompt bool
	var storedPolicy string
	err := s.control.db.QueryRow(`SELECT h.hosted_thread_id,COALESCE(e.client_session_id,''),COALESCE(e.request_hash,''),h.provider,h.presentation_mode,h.sidebar_placement,COALESCE(e.nano_session_id,''),COALESCE(h.current_native_id,''),COALESCE(h.invoker_hosted_id,''),COALESCE(h.invoker_native_id,''),COALESCE(h.invoker_provider,''),h.notify_invoker_on_terminal,h.terminal_notification_policy,COALESCE(e.status,'detached'),COALESCE(e.error,''),COALESCE(e.run_id,''),COALESCE(e.created_at,h.created_at),COALESCE(e.updated_at,h.updated_at),COALESCE(e.revision,h.revision) FROM hosted_threads h LEFT JOIN hosted_execution_sessions e ON e.hosted_thread_id=h.hosted_thread_id WHERE h.hosted_thread_id=?`, hostedID).Scan(&record.ID, &record.ClientSessionID, &record.RequestHash, &record.Provider, &record.PresentationMode, &record.SidebarPlacement, &record.NanoSessionID, &record.NativeThreadID, &record.InvokerHostedThreadID, &record.InvokerNativeThreadID, &record.InvokerProvider, &legacyPrompt, &storedPolicy, &record.Status, &record.Error, &record.RunID, &record.CreatedAt, &record.UpdatedAt, &record.Revision)
	if err == sql.ErrNoRows {
		return agentSessionRecord{}, false, nil
	}
	if err != nil {
		return agentSessionRecord{}, false, err
	}
	setStoredSubscriptions(&record, storedPolicy, legacyPrompt)
	return record, true, nil
}

func (s *agentExecutionStore) listSessions() []agentSessionRecord {
	if s == nil {
		return []agentSessionRecord{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows := make([]agentSessionRecord, 0, len(s.sessions))
	for _, rec := range s.sessions {
		rows = append(rows, rec)
	}
	sort.Slice(rows, func(i, j int) bool {
		if a, b := parseIdentityTime(rows[i].CreatedAt), parseIdentityTime(rows[j].CreatedAt); !a.Equal(b) {
			return a.Before(b)
		}
		return rows[i].ID < rows[j].ID
	})
	return rows
}

// turnAttribution is the origin/sender stamp for a new operation.
type turnAttribution struct {
	Origin, SenderHostedThreadID, SenderNativeThreadID, SenderProvider, SenderName, SenderSubmittedAt string
}

// agentTurnCreationGuard runs while the execution-store mutex is held and,
// for the control-backed store, inside the same SQLite transaction that will
// insert the operation. Every operation creator enters through this boundary,
// so a guard can compare the current durable operation set with a candidate
// without leaving a time-of-check/time-of-use gap before creation. A guard
// must inspect only the supplied snapshot; calling store methods would try to
// reacquire the mutex.
type agentTurnCreationGuard func(existing []agentTurnRecord) error

func (s *agentExecutionStore) beginTurn(sessionID, actionID, promptHash string) (agentTurnRecord, bool, error) {
	return s.beginTurnAttributed(sessionID, actionID, promptHash, turnAttribution{})
}

func (s *agentExecutionStore) beginTurnAttributed(sessionID, actionID, promptHash string, attribution turnAttribution) (agentTurnRecord, bool, error) {
	return s.beginTurnAttributedGuarded(sessionID, actionID, promptHash, attribution, nil)
}

func (s *agentExecutionStore) beginTurnAttributedGuarded(sessionID, actionID, promptHash string, attribution turnAttribution, guard agentTurnCreationGuard) (agentTurnRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessionRec, ok := s.sessions[sessionID]
	if !ok {
		return agentTurnRecord{}, false, fmt.Errorf("unknown hosted thread")
	}
	key := agentActionKey(sessionID, actionID)
	if id := s.byAction[key]; id != "" {
		rec := s.turns[id]
		if rec.PromptHash != promptHash {
			return agentTurnRecord{}, false, errAgentTurnIdempotencyConflict
		}
		return rec, false, nil
	}
	if s.control != nil {
		var existingID, existingHash string
		err := s.control.db.QueryRow(`SELECT operation_id,prompt_hash FROM hosted_turn_operations WHERE hosted_thread_id=? AND client_action_id=?`, sessionID, actionID).Scan(&existingID, &existingHash)
		if err == nil {
			if existingHash != promptHash {
				return agentTurnRecord{}, false, errAgentTurnIdempotencyConflict
			}
			s.resetControlCachesLocked()
			if err := loadControlExecutionRows(s); err != nil {
				return agentTurnRecord{}, false, err
			}
			return s.turns[existingID], false, nil
		}
		if err != sql.ErrNoRows {
			return agentTurnRecord{}, false, err
		}
	}
	var tx *sql.Tx
	if s.control != nil {
		var err error
		tx, err = s.control.db.Begin()
		if err != nil {
			return agentTurnRecord{}, false, err
		}
		defer tx.Rollback()
	}
	if guard != nil {
		existing := make([]agentTurnRecord, 0)
		for _, operation := range s.turns {
			if operation.HostedThreadID == sessionID {
				existing = append(existing, operation)
			}
		}
		sort.Slice(existing, func(i, j int) bool {
			if a, b := parseIdentityTime(existing[i].CreatedAt), parseIdentityTime(existing[j].CreatedAt); !a.Equal(b) {
				return a.Before(b)
			}
			return existing[i].ID < existing[j].ID
		})
		if err := guard(existing); err != nil {
			return agentTurnRecord{}, false, err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rec := agentTurnRecord{
		ID: agentOperationID(sessionID, actionID), HostedThreadID: sessionID, ClientActionID: actionID,
		TransportActionID: agentTransportActionID(sessionID, actionID), PromptHash: promptHash,
		Provider: sessionRec.Provider, NanoSessionID: sessionRec.NanoSessionID,
		NativeThreadID: sessionRec.NativeThreadID, Status: agentTurnDeliveryPending,
		Origin: attribution.Origin, SenderHostedThreadID: attribution.SenderHostedThreadID,
		SenderNativeThreadID: attribution.SenderNativeThreadID, SenderProvider: attribution.SenderProvider,
		SenderName: attribution.SenderName, SenderSubmittedAt: attribution.SenderSubmittedAt,
		RunID: s.runID, CreatedAt: now, UpdatedAt: now, Revision: 1,
	}
	if tx != nil {
		var assignmentID sql.NullInt64
		var assignmentDisposition string
		err := tx.QueryRow(`SELECT desired_event_id,desired_disposition FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, sessionID, orchestrationAssignmentField).Scan(&assignmentID, &assignmentDisposition)
		if err != nil && err != sql.ErrNoRows {
			return agentTurnRecord{}, false, err
		}
		if assignmentID.Valid && assignmentDisposition != metadataDispositionCleared {
			id := assignmentID.Int64
			rec.AssignmentEventID = &id
		}
		if err := insertTurnOperation(tx, rec); err != nil {
			return agentTurnRecord{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return agentTurnRecord{}, false, err
		}
		s.setTurnLocked(rec.ID, &rec)
		s.turnBaseline[rec.ID] = rec.Revision
		s.noteTurnTransitionLocked("", rec)
		s.publishPendingTransitionsLocked()
		return rec, true, nil
	}
	s.setTurnLocked(rec.ID, &rec)
	s.noteTurnTransitionLocked("", rec)
	if err := s.persistAndNotifyLocked(); err != nil {
		if errors.Is(err, errExecutionConflict) {
			if existingID := s.byAction[key]; existingID != "" {
				if existing := s.turns[existingID]; existing.PromptHash == promptHash {
					return existing, false, nil
				}
			}
		}
		return agentTurnRecord{}, false, err
	}
	return rec, true, nil
}

// Every turn mutation invalidates only the affected threads. The turn map
// remains authoritative; rollback reloads rebuild these derived indexes.
func (s *agentExecutionStore) setTurnLocked(id string, rec *agentTurnRecord) {
	if before, ok := s.turns[id]; ok {
		if rec == nil || before.HostedThreadID != rec.HostedThreadID {
			delete(s.turnsByHosted[before.HostedThreadID], id)
			if len(s.turnsByHosted[before.HostedThreadID]) == 0 {
				delete(s.turnsByHosted, before.HostedThreadID)
			}
		}
		if rec == nil || before.HostedThreadID != rec.HostedThreadID || before.ClientActionID != rec.ClientActionID {
			delete(s.byAction, agentActionKey(before.HostedThreadID, before.ClientActionID))
		}
		s.dirtyTurnThreads[before.HostedThreadID] = true
		s.attentionDirty[before.HostedThreadID] = true
	}
	s.dirtyTurns[id] = true
	if rec == nil {
		delete(s.turns, id)
		if s.turnBaseline[id] > 0 {
			s.deletedTurns[id] = true
		}
		return
	}
	s.turns[id] = *rec
	if s.turnsByHosted[rec.HostedThreadID] == nil {
		s.turnsByHosted[rec.HostedThreadID] = map[string]bool{}
	}
	s.turnsByHosted[rec.HostedThreadID][id] = true
	s.byAction[agentActionKey(rec.HostedThreadID, rec.ClientActionID)] = id
	delete(s.deletedTurns, id)
	s.dirtyTurnThreads[rec.HostedThreadID] = true
	s.attentionDirty[rec.HostedThreadID] = true
}

func (s *agentExecutionStore) resetTurnIndexesLocked() {
	s.turnsByHosted = map[string]map[string]bool{}
	s.dirtyTurnThreads, s.dirtyTurns = map[string]bool{}, map[string]bool{}
	s.attentionDirty, s.attentionCache = map[string]bool{}, map[string]bool{}
}

func (s *agentExecutionStore) resetControlCachesLocked() {
	s.sessions, s.turns, s.byAction, s.byClient = map[string]agentSessionRecord{}, map[string]agentTurnRecord{}, map[string]string{}, map[string]string{}
	s.deletedSessions, s.deletedTurns, s.sessionBaseline, s.turnBaseline = map[string]bool{}, map[string]bool{}, map[string]int64{}, map[string]int64{}
	s.pendingTransitions = nil
	s.resetTurnIndexesLocked()
}

func (s *agentExecutionStore) turn(id string) (agentTurnRecord, bool) {
	if s == nil {
		return agentTurnRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.turns[id]
	return rec, ok
}

func (s *agentExecutionStore) turnsForHosted(hostedID string) []agentTurnRecord {
	if s == nil || hostedID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]agentTurnRecord, 0)
	for _, rec := range s.turns {
		if rec.HostedThreadID == hostedID {
			result = append(result, rec)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if a, b := parseIdentityTime(result[i].CreatedAt), parseIdentityTime(result[j].CreatedAt); !a.Equal(b) {
			return a.Before(b)
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func (s *agentExecutionStore) operationIDsForHosted(hostedID string) []string {
	if s == nil || hostedID == "" {
		return []string{}
	}
	if s.control != nil {
		rows, err := s.control.db.Query(`SELECT operation_id FROM hosted_turn_operations WHERE hosted_thread_id=? ORDER BY operation_id`, hostedID)
		if err == nil {
			defer rows.Close()
			ids := []string{}
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			return ids
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0)
	for id, rec := range s.turns {
		if rec.HostedThreadID == hostedID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// lineageForHosted reads the immutable invocation edges held by Nano's
// execution store. It never accepts lineage identifiers from a tool request.
func (s *agentExecutionStore) lineageForHosted(hostedID string) (ancestors, descendants []string) {
	if s == nil || hostedID == "" {
		return []string{}, []string{}
	}
	if s.control != nil {
		current := hostedID
		seen := map[string]bool{hostedID: true}
		for {
			var parent sql.NullString
			err := s.control.db.QueryRow(`SELECT invoker_hosted_id FROM hosted_threads WHERE hosted_thread_id=?`, current).Scan(&parent)
			if err != nil || !parent.Valid || parent.String == "" || seen[parent.String] {
				break
			}
			current, seen[parent.String] = parent.String, true
			ancestors = append(ancestors, current)
		}
		rows, err := s.control.db.Query(`WITH RECURSIVE family(id) AS (
			SELECT hosted_thread_id FROM hosted_threads WHERE invoker_hosted_id=?
			UNION
			SELECT h.hosted_thread_id FROM hosted_threads h JOIN family f ON h.invoker_hosted_id=f.id
		) SELECT id FROM family ORDER BY id`, hostedID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					descendants = append(descendants, id)
				}
			}
		}
		sort.Strings(ancestors)
		return ancestors, descendants
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{hostedID: true}
	current := hostedID
	for {
		record, ok := s.sessions[current]
		if !ok || record.InvokerHostedThreadID == "" || seen[record.InvokerHostedThreadID] {
			break
		}
		current = record.InvokerHostedThreadID
		seen[current] = true
		ancestors = append(ancestors, current)
	}
	queue := []string{hostedID}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, record := range s.sessions {
			if record.InvokerHostedThreadID == parent && !seen[record.ID] {
				seen[record.ID] = true
				descendants = append(descendants, record.ID)
				queue = append(queue, record.ID)
			}
		}
	}
	sort.Strings(ancestors)
	sort.Strings(descendants)
	return ancestors, descendants
}

func (s *agentExecutionStore) turnForAction(sessionID, actionID string) (agentTurnRecord, bool) {
	if s == nil {
		return agentTurnRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.byAction[agentActionKey(sessionID, actionID)]
	rec, ok := s.turns[id]
	return rec, ok
}

// persistedTurnByTransportQuery seeks one hosted thread's operations for one
// transport action through hosted_turn_operations_transport.
const persistedTurnByTransportQuery = `SELECT operation_id,COALESCE(provider_turn_id,''),status,COALESCE(error_code,''),COALESCE(final_item_id,''),COALESCE(origin,''),COALESCE(sender_submitted_at,'')
	FROM hosted_turn_operations WHERE hosted_thread_id=? AND transport_action_id=? LIMIT 2`

// persistedTurnForTransportAction reads, from the committed operation rows
// rather than the cache, the one operation of a hosted thread delivered under
// a transport action: its provider turn, status, error code and final item.
// found is false only when the committed rows hold no such operation; an
// unreadable store, or two operations for one action, is an error, never an
// absence. The read assumes nothing about how the action id was formed.
func (s *agentExecutionStore) persistedTurnForTransportAction(ctx context.Context, hostedID, transportActionID string) (agentTurnRecord, bool, error) {
	if s == nil || s.control == nil || hostedID == "" || transportActionID == "" {
		return agentTurnRecord{}, false, errors.New("persisted operation read needs the control store, a hosted thread and an action")
	}
	rows, err := s.control.db.QueryContext(ctx, persistedTurnByTransportQuery, hostedID, transportActionID)
	if err != nil {
		return agentTurnRecord{}, false, err
	}
	defer rows.Close()
	var found []agentTurnRecord
	for rows.Next() {
		rec := agentTurnRecord{HostedThreadID: hostedID, TransportActionID: transportActionID}
		var errorCode string
		if err := rows.Scan(&rec.ID, &rec.ProviderTurnID, &rec.Status, &errorCode, &rec.FinalItemID, &rec.Origin, &rec.SenderSubmittedAt); err != nil {
			return agentTurnRecord{}, false, err
		}
		if errorCode != "" {
			rec.Error = &agentExecutionError{Code: errorCode}
		}
		found = append(found, rec)
	}
	if err := rows.Err(); err != nil {
		return agentTurnRecord{}, false, err
	}
	switch len(found) {
	case 0:
		return agentTurnRecord{}, false, nil
	case 1:
		return found[0], true, nil
	default:
		return agentTurnRecord{}, false, fmt.Errorf("transport action %s has more than one operation", transportActionID)
	}
}

func (s *agentExecutionStore) turnForTransportAction(actionID string) (agentTurnRecord, bool) {
	if s == nil || actionID == "" {
		return agentTurnRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.turns {
		if rec.TransportActionID == actionID {
			return rec, true
		}
	}
	return agentTurnRecord{}, false
}

// openTurnsForProviderTurn lists the unfinished operations bound to one
// provider turn. Provider turn ids are scoped by the hosted thread and
// provider so a resumed or migrated transport cannot make an unrelated
// operation own the turn. Prompts steered into a running turn share it.
func (s *agentExecutionStore) openTurnsForProviderTurn(hostedThreadID, provider, providerTurnID string) []agentTurnRecord {
	if s == nil || hostedThreadID == "" || providerTurnID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var open []agentTurnRecord
	for _, rec := range s.turns {
		if rec.HostedThreadID == hostedThreadID && rec.ProviderTurnID == providerTurnID &&
			(provider == "" || rec.Provider == provider) && !agentTurnTerminal(rec.Status) {
			open = append(open, rec)
		}
	}
	return open
}

type agentTerminalTurn struct {
	ProviderTurnID     string
	Status             string
	Error              *agentExecutionError
	Response           string
	ItemID             string
	FinishOnSavedProof bool
}

type agentResumedTurnMatch struct {
	openOperations  []agentTurnRecord
	boundOperations []string
	// live: a matching operation is not terminal. Only then can the turn's
	// reply text change an operation.
	live bool
}

// resumedTurnMatches scans the fleet once before prompt-store reads, which
// must happen outside the execution lock. Only the reply's turns are indexed.
func (s *agentExecutionStore) resumedTurnMatches(hostedThreadID, provider, nativeThreadID string, turns []agentTerminalTurn) []agentResumedTurnMatch {
	out := make([]agentResumedTurnMatch, len(turns))
	if s == nil || len(turns) == 0 {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	matches := make(map[string]*agentResumedTurnMatch, len(turns))
	for _, turn := range turns {
		if turn.ProviderTurnID != "" {
			matches[turn.ProviderTurnID] = &agentResumedTurnMatch{}
		}
	}
	for id, rec := range s.turns {
		m := matches[rec.ProviderTurnID]
		if m == nil || rec.Provider != provider {
			continue
		}
		open := hostedThreadID != "" && rec.HostedThreadID == hostedThreadID && !agentTurnTerminal(rec.Status)
		if open {
			m.openOperations = append(m.openOperations, rec)
		}
		bound := boundToTurn(rec, provider, nativeThreadID, rec.ProviderTurnID)
		if bound {
			m.boundOperations = append(m.boundOperations, id)
		}
		if (open || bound) && !agentTurnTerminal(rec.Status) {
			m.live = true
		}
	}
	for i, turn := range turns {
		if m := matches[turn.ProviderTurnID]; m != nil {
			out[i] = *m
		}
	}
	return out
}

// markResumedTurnsTerminal applies reply order without another fleet scan.
// Records may finish or change binding during the unlocked prompt reads, so
// each indexed operation is checked again before it supplies or takes a mark.
func (s *agentExecutionStore) markResumedTurnsTerminal(hostedThreadID, provider, nativeThreadID string, turns []agentTerminalTurn, matches []agentResumedTurnMatch) ([]bool, error) {
	endedWhileDown := make([]bool, len(turns))
	if s == nil || len(turns) == 0 {
		return endedWhileDown, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for i, turn := range turns {
		m := matches[i]
		for _, op := range m.openOperations {
			if rec, found := s.turns[op.ID]; found && rec.HostedThreadID == hostedThreadID && rec.Provider == provider &&
				rec.ProviderTurnID == turn.ProviderTurnID && !agentTurnTerminal(rec.Status) {
				endedWhileDown[i] = true
			}
		}
		chosen, count := "", 0
		for _, id := range m.boundOperations {
			rec, found := s.turns[id]
			if !found || !boundToTurn(rec, provider, nativeThreadID, turn.ProviderTurnID) {
				continue
			}
			chosen, count = id, count+1
			if turn.Status == agentTurnCompleted && s.observeCandidateLocked(id, turn.ItemID, "final_answer", turn.Response) {
				changed = true
			}
		}
		if s.savedResultRead != nil {
			for _, id := range m.boundOperations {
				if rec, ok := s.turns[id]; ok && boundToTurn(rec, provider, nativeThreadID, turn.ProviderTurnID) && s.markTurnTerminalLocked(id, turn.Status, turn.Error, turn.Response, turn.ItemID) {
					changed = true
				}
			}
			continue
		}
		if !turn.FinishOnSavedProof && count == 1 && s.markTurnTerminalLocked(chosen, turn.Status, turn.Error, turn.Response, turn.ItemID) {
			changed = true
		}
	}
	if !changed {
		return endedWhileDown, nil
	}
	return endedWhileDown, s.persistAndNotifyLocked()
}

// Only a thread's latest submitted operation decides attention; a successful
// follow-up clears an earlier failure without a separate acknowledgement.
func (s *agentExecutionStore) refreshAttentionLocked(hostedID string) {
	if !s.attentionDirty[hostedID] {
		return
	}
	latest, latestAt := agentTurnRecord{}, time.Time{}
	for id := range s.turnsByHosted[hostedID] {
		rec := s.turns[id]
		// Compared as times: RFC3339Nano trims trailing zeros, so as text
		// ".2374Z" sorts after the later ".237401Z".
		at := parseIdentityTime(rec.CreatedAt)
		if latest.ID == "" || at.After(latestAt) || (at.Equal(latestAt) && rec.ID > latest.ID) {
			latest, latestAt = rec, at
		}
	}
	delete(s.attentionDirty, hostedID)
	if latest.ID == "" {
		delete(s.attentionCache, hostedID)
	} else {
		s.attentionCache[hostedID] = latest.Status == agentTurnFailed || latest.Status == agentTurnDeliveryUnknown
	}
}

func (s *agentExecutionStore) sessionsNeedingAttention() map[string]bool {
	if s == nil {
		return map[string]bool{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for hostedID := range s.attentionDirty {
		s.refreshAttentionLocked(hostedID)
	}
	return maps.Clone(s.attentionCache)
}

func (s *agentExecutionStore) sessionNeedsAttention(hostedID string) bool {
	if s == nil || hostedID == "" {
		return false
	}
	s.mu.RLock()
	if !s.attentionDirty[hostedID] {
		needs := s.attentionCache[hostedID]
		s.mu.RUnlock()
		return needs
	}
	s.mu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshAttentionLocked(hostedID)
	return s.attentionCache[hostedID]
}

func (s *agentExecutionStore) markTurnDelivery(operationID, providerTurnID, status string, executionErr *agentExecutionError) error {
	if s == nil || operationID == "" {
		return nil
	}
	return s.markTurnDeliveryWith(operationID, providerTurnID, status, executionErr, nil)
}

// markTurnDeliveryWith is markTurnDelivery that also makes with, a write the
// caller owes with it, in the same transaction on the threads database: both
// commit, or neither does. with runs even when the operation keeps its
// outcome.
func (s *agentExecutionStore) markTurnDeliveryWith(operationID, providerTurnID, status string, executionErr *agentExecutionError, with func(*sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.turns[operationID]
	if !ok || agentTurnTerminal(rec.Status) || rec.Status == agentTurnAwaitingSaved {
		if with == nil {
			return nil
		}
		return s.persistAndNotifyWithLocked(with)
	}
	if providerTurnID != "" {
		rec.ProviderTurnID = providerTurnID
	}
	oldStatus := rec.Status
	rec.Status = status
	rec.Error = executionErr
	rec.RunID = s.runID
	touchAgentTurn(&rec)
	s.setTurnLocked(rec.ID, &rec)
	s.noteTurnTransitionLocked(oldStatus, rec)
	return s.persistAndNotifyWithLocked(with)
}

func (s *agentExecutionStore) observeCandidate(provider, nativeThreadID, providerTurnID, itemID, phase, response string) error {
	if s == nil || providerTurnID == "" || response == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	// Every operation sharing the turn reads the same reply.
	for id, rec := range s.turns {
		if boundToTurn(rec, provider, nativeThreadID, providerTurnID) && s.observeCandidateLocked(id, itemID, phase, response) {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.persistAndNotifyLocked()
}

func (s *agentExecutionStore) observeCandidateLocked(id, itemID, phase, response string) bool {
	rec, found := s.turns[id]
	if !found || response == "" || agentTurnTerminal(rec.Status) || rec.Status == agentTurnAwaitingSaved || rec.CandidatePhase == "final_answer" && phase != "final_answer" {
		return false
	}
	rec.CandidateItemID = itemID
	rec.CandidatePhase = phase
	rec.CandidateResponse, rec.CandidateBytes, rec.CandidateTruncated = boundAgentResult(response)
	touchAgentTurn(&rec)
	s.setTurnLocked(rec.ID, &rec)
	return true
}

type agentResultMeta struct {
	Bytes     int
	Truncated bool
}

func (s *agentExecutionStore) markTurnTerminal(provider, nativeThreadID, providerTurnID, status string, executionErr *agentExecutionError, response, itemID string, resultMeta ...agentResultMeta) error {
	if s == nil || providerTurnID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.savedResultRead != nil {
		changed := false
		for id, rec := range s.turns {
			if boundToTurn(rec, provider, nativeThreadID, providerTurnID) && s.markTurnTerminalLocked(id, status, executionErr, response, itemID, resultMeta...) {
				changed = true
			}
		}
		if changed {
			return s.persistAndNotifyLocked()
		}
		return nil
	}
	id := s.operationForTurnLocked(provider, nativeThreadID, providerTurnID)
	if !s.markTurnTerminalLocked(id, status, executionErr, response, itemID, resultMeta...) {
		return nil
	}
	return s.persistAndNotifyLocked()
}

func (s *agentExecutionStore) markTurnTerminalLocked(id, status string, executionErr *agentExecutionError, response, itemID string, resultMeta ...agentResultMeta) bool {
	rec, found := s.turns[id]
	if !found || agentTurnTerminal(rec.Status) {
		return false
	}
	meta := agentResultMeta{}
	if len(resultMeta) > 0 {
		meta = resultMeta[0]
	}
	if response == "" {
		// The turn's one operation observed its reply on the wire.
		response = rec.CandidateResponse
		if itemID == "" {
			itemID = rec.CandidateItemID
		}
		meta = agentResultMeta{Bytes: rec.CandidateBytes, Truncated: rec.CandidateTruncated}
	}
	return s.applyTurnFinishLocked(id, status, executionErr, response, itemID, meta)
}

// finishJoinedTurn finishes an operation whose own prompt the saved
// transcript proved part of a provider turn, with the turn's recorded outcome
// and the one final answer resolved for that turn, exactly as given.
func (s *agentExecutionStore) finishJoinedTurn(operationID, providerTurnID, outcome string, executionErr *agentExecutionError, response, itemID string, meta agentResultMeta) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.turns[operationID]; !ok || rec.ProviderTurnID != providerTurnID {
		return nil
	}
	return s.finishTurnLocked(operationID, outcome, executionErr, response, itemID, meta)
}

// finishResolvedTurn finishes an operation with an answer already read from
// its own saved turn, so no saved-result wait follows.
func (s *agentExecutionStore) finishResolvedTurn(operationID, providerTurnID, outcome string, executionErr *agentExecutionError, response, itemID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.turns[operationID]; !ok || rec.ProviderTurnID != providerTurnID {
		return nil
	}
	if !s.applyResolvedTurnFinishLocked(operationID, outcome, executionErr, response, itemID, agentResultMeta{}) {
		return nil
	}
	return s.persistAndNotifyLocked()
}

func (s *agentExecutionStore) finishTurnLocked(id, status string, executionErr *agentExecutionError, response, itemID string, meta agentResultMeta) error {
	if !s.applyTurnFinishLocked(id, status, executionErr, response, itemID, meta) {
		return nil
	}
	return s.persistAndNotifyLocked()
}

func (s *agentExecutionStore) applyTurnFinishLocked(id, status string, executionErr *agentExecutionError, response, itemID string, meta agentResultMeta) bool {
	if s.savedResultRead != nil && normalizeAgentTerminalStatus(status) == agentTurnCompleted {
		return s.awaitSavedResultLocked(id, itemID)
	}
	return s.applyResolvedTurnFinishLocked(id, status, executionErr, response, itemID, meta)
}

func (s *agentExecutionStore) applyResolvedTurnFinishLocked(id, status string, executionErr *agentExecutionError, response, itemID string, meta agentResultMeta) bool {
	rec, found := s.turns[id]
	if !found || agentTurnTerminal(rec.Status) && !(answerNotSaved(rec) && status == agentTurnCompleted) {
		return false
	}
	oldStatus := rec.Status
	rec.Status = normalizeAgentTerminalStatus(status)
	rec.Error = executionErr
	if rec.Status == agentTurnCompleted {
		if meta.Truncated {
			rec.FinalResponse, _, _ = boundAgentResult(response)
			rec.FinalResponseBytes = meta.Bytes
			rec.FinalResponseTruncated = true
		} else {
			rec.FinalResponse, rec.FinalResponseBytes, rec.FinalResponseTruncated = boundAgentResult(response)
		}
		rec.FinalItemID = itemID
	}
	rec.CandidateItemID, rec.CandidatePhase, rec.CandidateResponse = "", "", ""
	rec.CandidateBytes, rec.CandidateTruncated = 0, false
	touchAgentTurn(&rec)
	s.setTurnLocked(rec.ID, &rec)
	s.noteTurnTransitionLocked(oldStatus, rec)
	return true
}

func (s *agentExecutionStore) markWaiting(sessionID, providerTurnID string, waiting bool) error {
	if s == nil || sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var chosen string
	for id, rec := range s.turns {
		if rec.HostedThreadID != sessionID || agentTurnTerminal(rec.Status) || rec.Status == agentTurnAwaitingSaved {
			continue
		}
		if providerTurnID != "" && rec.ProviderTurnID != providerTurnID {
			continue
		}
		if chosen == "" || timestampBefore(s.turns[chosen].CreatedAt, rec.CreatedAt) {
			chosen = id
		}
	}
	if chosen == "" {
		return nil
	}
	rec := s.turns[chosen]
	oldStatus := rec.Status
	if waiting {
		rec.Status = agentTurnWaiting
	} else if rec.Status == agentTurnWaiting {
		rec.Status = agentTurnRunning
	}
	touchAgentTurn(&rec)
	s.setTurnLocked(rec.ID, &rec)
	s.noteTurnTransitionLocked(oldStatus, rec)
	return s.persistAndNotifyLocked()
}

func (s *agentExecutionStore) markSessionTurns(sessionID, status string, executionErr *agentExecutionError) error {
	if s == nil || sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, rec := range s.turns {
		if rec.HostedThreadID != sessionID || agentTurnTerminal(rec.Status) || rec.Status == agentTurnAwaitingSaved {
			continue
		}
		oldStatus := rec.Status
		rec.Status = status
		rec.Error = executionErr
		if agentTurnTerminal(status) {
			rec.CandidateItemID, rec.CandidatePhase, rec.CandidateResponse = "", "", ""
			rec.CandidateBytes, rec.CandidateTruncated = 0, false
		}
		touchAgentTurn(&rec)
		s.setTurnLocked(rec.ID, &rec)
		s.noteTurnTransitionLocked(oldStatus, rec)
		changed = true
	}
	if !changed {
		return nil
	}
	return s.persistAndNotifyLocked()
}

// sharedTurnResult finds a provider turn's final answer by its exact item id
// among the operations bound to that turn in that native thread: one already
// finished with it, or one that observed it on the wire. Unlike boundToTurn,
// a record without a native thread never supplies another member's answer.
func (s *agentExecutionStore) sharedTurnResult(hostedThreadID, provider, nativeThreadID, providerTurnID, finalItemID string) (string, agentResultMeta, bool) {
	if s == nil || finalItemID == "" || nativeThreadID == "" {
		return "", agentResultMeta{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.turns {
		if rec.HostedThreadID != hostedThreadID || rec.Provider != provider || rec.NativeThreadID != nativeThreadID || rec.ProviderTurnID != providerTurnID {
			continue
		}
		if rec.Status == agentTurnCompleted && rec.FinalItemID == finalItemID {
			return rec.FinalResponse, agentResultMeta{Bytes: rec.FinalResponseBytes, Truncated: rec.FinalResponseTruncated}, true
		}
		if rec.CandidateItemID == finalItemID {
			return rec.CandidateResponse, agentResultMeta{Bytes: rec.CandidateBytes, Truncated: rec.CandidateTruncated}, true
		}
	}
	return "", agentResultMeta{}, false
}

// operationForTurnLocked finds the one operation a provider turn belongs to.
// A turn that several operations share has no single owner: each of them
// finishes only when the saved transcript proves its own prompt joined it.
func (s *agentExecutionStore) operationForTurnLocked(provider, nativeThreadID, providerTurnID string) string {
	found := ""
	for id, rec := range s.turns {
		if !boundToTurn(rec, provider, nativeThreadID, providerTurnID) {
			continue
		}
		if found != "" {
			return ""
		}
		found = id
	}
	return found
}

func boundToTurn(rec agentTurnRecord, provider, nativeThreadID, providerTurnID string) bool {
	return rec.Provider == provider && rec.ProviderTurnID == providerTurnID &&
		(nativeThreadID == "" || rec.NativeThreadID == "" || rec.NativeThreadID == nativeThreadID)
}

func (s *agentExecutionStore) waitTurn(ctx context.Context, id string, afterRevision int64, wait time.Duration) (agentTurnRecord, bool) {
	deadline := time.NewTimer(wait)
	if wait <= 0 {
		deadline.Stop()
	}
	for {
		s.mu.RLock()
		rec, ok := s.turns[id]
		ch := s.changed
		s.mu.RUnlock()
		if !ok || rec.Revision > afterRevision || wait <= 0 {
			return rec, ok
		}
		select {
		case <-ctx.Done():
			return rec, true
		case <-deadline.C:
			return rec, true
		case <-ch:
		}
	}
}

func normalizeAgentTerminalStatus(status string) string {
	switch status {
	case agentTurnCompleted:
		return agentTurnCompleted
	case agentTurnInterrupted, "aborted", "cancelled", "canceled":
		return agentTurnInterrupted
	default:
		return agentTurnFailed
	}
}

func agentTurnTerminal(status string) bool {
	return status == agentTurnCompleted || status == agentTurnFailed || status == agentTurnInterrupted
}

func boundAgentResult(value string) (string, int, bool) {
	original := len(value)
	if original <= maxAgentResultBytes {
		return value, original, false
	}
	return truncateUTF8Live(value, maxAgentResultBytes), original, true
}

func touchAgentSession(rec *agentSessionRecord) {
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	rec.Revision++
}

func touchAgentTurn(rec *agentTurnRecord) {
	// Metadata reattachment cannot extend a saved-result deadline. The
	// awaiting transition sets its timestamp once, before this status is set.
	if rec.Status != agentTurnAwaitingSaved {
		rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	rec.Revision++
}

func (s *agentExecutionStore) persistAndNotifyLocked() error {
	return s.persistAndNotifyWithLocked(nil)
}

// persistAndNotifyWithLocked commits the changed rows and with, a write owed
// with them, in one transaction.
func (s *agentExecutionStore) persistAndNotifyWithLocked(with func(*sql.Tx) error) error {
	if err := s.persistLocked(with); err != nil {
		s.pendingTransitions = nil
		if s.control != nil {
			// Callers update their working maps before asking the repository to
			// commit. Restore from committed rows on failure so no speculative
			// session, action index, or turn leaks into later reads.
			s.resetControlCachesLocked()
			_ = loadControlExecutionRows(s)
		}
		return err
	}
	for _, transition := range s.pendingTransitions {
		if transition.To == agentTurnAwaitingSaved {
			s.startSavedResultLocked(transition.OperationID)
		}
	}
	s.publishPendingTransitionsLocked()
	return nil
}

func (s *agentExecutionStore) publishPendingTransitionsLocked() {
	if s.changes != nil {
		for _, transition := range s.pendingTransitions {
			s.changes.append(transition)
		}
	}
	s.pendingTransitions = nil
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *agentExecutionStore) persistLocked(with func(*sql.Tx) error) error {
	if s.control == nil {
		return fmt.Errorf("control execution store unavailable")
	}
	return s.persistControlLocked(with)
}

// persistControlLocked applies only rows whose revision changed and deletes
// only rows intentionally absent from the in-memory retention set. It never
// serializes the live control plane outside SQLite.
func (s *agentExecutionStore) persistControlLocked(with func(*sql.Tx) error) error {
	if s.deletedSessions == nil {
		s.deletedSessions = map[string]bool{}
	}
	if s.deletedTurns == nil {
		s.deletedTurns = map[string]bool{}
	}
	// Only changed threads can shed surplus, and only when the prompt owner
	// proves that their prompts can never owe a member completion.
	for hostedID := range s.dirtyTurnThreads {
		if len(s.turnsByHosted[hostedID]) <= maxAgentTurns || s.pruneEligible == nil {
			continue
		}
		records := make([]agentTurnRecord, 0, len(s.turnsByHosted[hostedID]))
		changed := false
		for id := range s.turnsByHosted[hostedID] {
			rec := s.turns[id]
			if rec.Revision != s.turnBaseline[id] {
				changed = true
			}
			if agentTurnTerminal(rec.Status) {
				records = append(records, rec)
			}
		}
		if !changed || len(records) <= maxAgentTurns {
			continue
		}
		sort.Slice(records, func(i, j int) bool { return timestampBefore(records[j].UpdatedAt, records[i].UpdatedAt) })
		surplus := records[maxAgentTurns:]
		for start := 0; start < len(surplus); start += maxPruneEligibleBatch {
			chunk := surplus[start:min(start+maxPruneEligibleBatch, len(surplus))]
			actions := make([]string, len(chunk))
			for i, rec := range chunk {
				actions[i] = rec.TransportActionID
			}
			ctx, cancel := context.WithTimeout(context.Background(), pruneEligibleReadTimeout)
			eligible, err := s.pruneEligible(ctx, hostedID, actions)
			cancel()
			if err != nil {
				log.Printf("retention of %s keeps %d operations whose prompts are unknown: %v", hostedID, len(chunk), err)
				continue
			}
			for _, rec := range chunk {
				if eligible[rec.TransportActionID] {
					s.setTurnLocked(rec.ID, nil)
				}
			}
		}
	}
	tx, err := s.control.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, rec := range s.sessions {
		expected := s.sessionBaseline[id]
		if rec.Revision == expected {
			continue
		}
		result, err := tx.Exec(`INSERT INTO hosted_execution_sessions(hosted_thread_id,client_session_id,request_hash,nano_session_id,status,error,run_id,created_at,updated_at,revision) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(hosted_thread_id) DO UPDATE SET client_session_id=excluded.client_session_id,request_hash=excluded.request_hash,nano_session_id=excluded.nano_session_id,status=excluded.status,error=excluded.error,run_id=excluded.run_id,updated_at=excluded.updated_at,revision=excluded.revision WHERE hosted_execution_sessions.revision=?`, rec.ID, nullableText(rec.ClientSessionID), rec.RequestHash, nullableText(rec.NanoSessionID), rec.Status, nullableText(rec.Error), rec.RunID, rec.CreatedAt, rec.UpdatedAt, rec.Revision, expected)
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return errExecutionConflict
		}
	}
	for id := range s.deletedSessions {
		result, err := tx.Exec(`DELETE FROM hosted_execution_sessions WHERE hosted_thread_id=? AND revision=?`, id, s.sessionBaseline[id])
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return errExecutionConflict
		}
	}
	for id := range s.dirtyTurns {
		rec, ok := s.turns[id]
		if !ok {
			continue
		}
		expected := s.turnBaseline[id]
		if rec.Revision == expected {
			continue
		}
		if err := upsertTurnOperationCAS(tx, rec, expected); err != nil {
			return err
		}
	}
	for id := range s.deletedTurns {
		result, err := tx.Exec(`DELETE FROM hosted_turn_operations WHERE operation_id=? AND revision=?`, id, s.turnBaseline[id])
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return errExecutionConflict
		}
	}
	if with != nil {
		if err := with(tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for id, rec := range s.sessions {
		s.sessionBaseline[id] = rec.Revision
	}
	for id := range s.dirtyTurns {
		if rec, ok := s.turns[id]; ok {
			s.turnBaseline[id] = rec.Revision
		} else {
			delete(s.turnBaseline, id)
		}
	}
	for id := range s.deletedSessions {
		delete(s.sessionBaseline, id)
	}
	s.dirtyTurnThreads, s.dirtyTurns = map[string]bool{}, map[string]bool{}
	s.deletedSessions, s.deletedTurns = map[string]bool{}, map[string]bool{}
	return nil
}

func insertTurnOperation(tx *sql.Tx, rec agentTurnRecord) error {
	return upsertTurnOperation(tx, rec)
}

func upsertTurnOperation(tx *sql.Tx, rec agentTurnRecord) error {
	return writeTurnOperation(tx, rec, nil)
}

func upsertTurnOperationCAS(tx *sql.Tx, rec agentTurnRecord, expected int64) error {
	return writeTurnOperation(tx, rec, &expected)
}

func writeTurnOperation(tx *sql.Tx, rec agentTurnRecord, expected *int64) error {
	var source, code, message, details any
	if rec.Error != nil {
		source, code, message = nullableText(rec.Error.Source), nullableText(rec.Error.Code), nullableText(rec.Error.Message)
		if len(rec.Error.Details) > 0 {
			details = string(rec.Error.Details)
		}
	}
	query := `INSERT INTO hosted_turn_operations(operation_id,hosted_thread_id,client_action_id,transport_action_id,prompt_hash,assignment_event_id,provider_turn_id,status,error_source,error_code,error_message,error_details_json,final_item_id,final_response,final_response_truncated,final_response_bytes,candidate_item_id,candidate_phase,candidate_response,candidate_bytes,candidate_truncated,origin,sender_hosted_id,sender_native_id,sender_provider,sender_name,sender_submitted_at,run_id,created_at,updated_at,revision) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(operation_id) DO UPDATE SET provider_turn_id=excluded.provider_turn_id,status=excluded.status,error_source=excluded.error_source,error_code=excluded.error_code,error_message=excluded.error_message,error_details_json=excluded.error_details_json,final_item_id=excluded.final_item_id,final_response=excluded.final_response,final_response_truncated=excluded.final_response_truncated,final_response_bytes=excluded.final_response_bytes,candidate_item_id=excluded.candidate_item_id,candidate_phase=excluded.candidate_phase,candidate_response=excluded.candidate_response,candidate_bytes=excluded.candidate_bytes,candidate_truncated=excluded.candidate_truncated,run_id=excluded.run_id,updated_at=excluded.updated_at,revision=excluded.revision`
	args := []any{rec.ID, rec.HostedThreadID, rec.ClientActionID, rec.TransportActionID, rec.PromptHash, nullableInt64(rec.AssignmentEventID), nullableText(rec.ProviderTurnID), rec.Status, source, code, message, details, nullableText(rec.FinalItemID), nullableText(rec.FinalResponse), rec.FinalResponseTruncated, rec.FinalResponseBytes, nullableText(rec.CandidateItemID), nullableText(rec.CandidatePhase), nullableText(rec.CandidateResponse), rec.CandidateBytes, rec.CandidateTruncated, nullableText(rec.Origin), nullableText(rec.SenderHostedThreadID), nullableText(rec.SenderNativeThreadID), nullableText(rec.SenderProvider), nullableText(rec.SenderName), nullableText(rec.SenderSubmittedAt), rec.RunID, rec.CreatedAt, rec.UpdatedAt, rec.Revision}
	if expected != nil {
		query += ` WHERE hosted_turn_operations.revision=?`
		args = append(args, *expected)
	}
	result, err := tx.Exec(query, args...)
	if err != nil {
		return err
	}
	if expected != nil {
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return errExecutionConflict
		}
	}
	return nil
}

var errExecutionConflict = errors.New("execution control row changed concurrently")

var errAgentExecutionUnavailable = errors.New("agent execution store unavailable")

// noteHumanPrompt persists the newest human-origin prompt time for a hosted
// thread (hosted_threads.last_human_prompt_at). Monotonic: an older time never
// overwrites a newer one. No-op without the control store.
func (s *agentExecutionStore) noteHumanPrompt(hostedThreadID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.sessions[hostedThreadID]
	if !ok {
		return fmt.Errorf("unknown hosted thread")
	}
	// Compared as times: RFC3339Nano trims trailing zeros, so as text ".2374Z"
	// sorts after the later ".237401Z". This store is the column's only writer
	// and loads it at start, so the record here decides.
	if rec.LastHumanPromptAt != "" && !parseIdentityTime(rec.LastHumanPromptAt).Before(at) {
		return nil
	}
	stamp := at.UTC().Format(time.RFC3339Nano)
	if s.control != nil {
		if _, err := s.control.db.Exec(`UPDATE hosted_threads SET last_human_prompt_at=? WHERE hosted_thread_id=?`, stamp, hostedThreadID); err != nil {
			return err
		}
	}
	rec.LastHumanPromptAt = stamp
	s.sessions[hostedThreadID] = rec
	return nil
}

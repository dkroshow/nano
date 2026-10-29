package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/threads"
)

// NormalizePresentationMode validates a presentation mode and applies its default.
func NormalizePresentationMode(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "", presentationShared:
		return presentationShared, nil
	case presentationBackground:
		return presentationBackground, nil
	default:
		return "", fmt.Errorf("presentationMode must be %q or %q", presentationShared, presentationBackground)
	}
}

// NormalizeSidebarPlacement validates sidebar placement and applies its default.
func NormalizeSidebarPlacement(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "", sidebarPlacementInvoker:
		return sidebarPlacementInvoker, nil
	case sidebarPlacementStandalone:
		return sidebarPlacementStandalone, nil
	default:
		return "", fmt.Errorf("sidebarPlacement must be %q or %q", sidebarPlacementInvoker, sidebarPlacementStandalone)
	}
}

// AgentHash returns the stable request hash used for idempotency.
func AgentHash(value []byte) string {
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

// BeginNamedSessionWithSubscriptionsAndPlacement atomically admits hosted identity, creation name, subscriptions and placement.
func (s *ExecutionStore) BeginNamedSessionWithSubscriptionsAndPlacement(clientSessionID, requestHash, provider, presentationMode, sidebarPlacement, creationName string, invoker agentSessionRecord, choices subscriptions.Choices, assignment ...*Assignment) (agentSessionRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id := s.byClient[clientSessionID]; clientSessionID != "" && id != "" {
		current := s.sessions[id]
		if current.RequestHash != requestHash {
			return agentSessionRecord{}, false, fmt.Errorf("%w: clientSessionId was already used with different session parameters", ErrClientSessionIdempotencyConflict)
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
		mode, err := NormalizePresentationMode(presentationMode)
		if err != nil {
			return agentSessionRecord{}, false, err
		}
		rec.PresentationMode = mode
		placement, err := NormalizeSidebarPlacement(sidebarPlacement)
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
			changedFields = append(changedFields, AssignmentField)
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

// EnsureNamedSessionWithIdentity creates or validates the durable identity needed by session attachment.
func (s *ExecutionStore) EnsureNamedSessionWithIdentity(id, provider, presentationMode, invokerHosted, invokerNative, invokerProvider, creationName string) (agentSessionRecord, error) {
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
		mode, err := NormalizePresentationMode(presentationMode)
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

// BindSession binds the current runtime attachment to its hosted record.
func (s *ExecutionStore) BindSession(id, nanoSessionID, nativeThreadID, status, presentationMode string) error {
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
			if err := SkipOwnedNativeClaim(s.control.ClaimNativeID(id, rec.Provider, nativeThreadID, NativeThreadIDProvisional(rec.Provider, nativeThreadID))); err != nil {
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
		if threads.TurnTerminal(turn.Status) {
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

// SetPresentation flips a hosted thread's presentation mode. control.sqlite
// commits first and is the single durable authority; the in-memory record is
// synchronized under the same store lock so no reader can observe the cache
// ahead of the committed row. Same-value calls are no-ops end to end (no
// revision bump, no publish). Returns the updated record, the hosted revision,
// and whether anything changed.
func (s *ExecutionStore) SetPresentation(id, mode string) (agentSessionRecord, int64, bool, error) {
	normalized, err := NormalizePresentationMode(mode)
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
func (s *ExecutionStore) SetSubscriptions(id string, choices subscriptions.Choices) (agentSessionRecord, int64, bool, error) {
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

// SetSidebarPlacement persists a hosted thread’s current sidebar placement.
func (s *ExecutionStore) SetSidebarPlacement(id, placement string) (agentSessionRecord, int64, bool, error) {
	normalized, err := NormalizeSidebarPlacement(placement)
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

// PresentationModeFor reads the canonical mode from the execution cache, which
// mirrors control.sqlite. Detached-runtime projection and boot reattach use it
// instead of trusting attachmentRecord.PresentationMode — the attachment JSON
// is a transport bridge, never presentation authority.
func (s *ExecutionStore) PresentationModeFor(id string) (string, bool) {
	mode, ok := s.PresentationModes([]string{id})[id]
	return mode, ok
}

// PresentationModes returns the known presentation mode of each id under one
// lock: a caller reading many threads waits for the store once, not once per
// thread behind every write.
func (s *ExecutionStore) PresentationModes(ids []string) map[string]string {
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

// UpdateSession records the attachment’s current runtime state.
func (s *ExecutionStore) UpdateSession(id, nanoSessionID, nativeThreadID, status, message string) error {
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
		if threads.TurnTerminal(turn.Status) {
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

// HasNonTerminalTurns reports whether any operation on the hosted thread still
// lacks terminal evidence. Detach refuses in that case: closing the attachment
// mid-operation would force Nano to invent turn evidence, and detach's whole
// meaning is that nothing about the work is being asserted.
func (s *ExecutionStore) HasNonTerminalTurns(sessionID string) bool {
	if s == nil || sessionID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.turns {
		if rec.HostedThreadID == sessionID && !threads.TurnTerminal(rec.Status) {
			return true
		}
	}
	return false
}

// RemoveSession erases the durable control record and every operation it owns
// — the destroy half of the close/delete verb split. persistLocked rebuilds
// the byAction index from the surviving turns.
func (s *ExecutionStore) RemoveSession(id string) error {
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

// Session reads a cached hosted execution record by its durable ID.
func (s *ExecutionStore) Session(id string) (agentSessionRecord, bool) {
	if s == nil {
		return agentSessionRecord{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.sessions[id]
	return rec, ok
}

// ResolveSession resolves a hosted execution ID or its client creation alias.
func (s *ExecutionStore) ResolveSession(ref string) (agentSessionRecord, bool) {
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
		if matched.Status == StatusExited && rec.Status != StatusExited {
			matched = rec
			continue
		}
		if rec.Status == StatusExited && matched.Status != StatusExited {
			continue
		}
		return agentSessionRecord{}, false
	}
	return matched, matched.ID != ""
}

// SessionForNative returns the exact current owner of a catalog identity.
// Retired history remains available through sessionsForNativeHistory, but cannot
// become current routing authority when the active claim is absent or invalid.
func (s *ExecutionStore) SessionForNative(provider, nativeThreadID string) (agentSessionRecord, bool) {
	if s == nil || s.control == nil || provider == "" || nativeThreadID == "" {
		return agentSessionRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hostedID, err := s.control.ActiveNativeOwner(provider, nativeThreadID)
	if err != nil || hostedID == "" {
		return agentSessionRecord{}, false
	}
	record, ok, err := s.sessionFromCanonicalLocked(hostedID)
	return record, ok && err == nil
}

// SessionsForNativeHistory returns every hosted record that has ever claimed
// this exact provider/native pair. The active claim remains unique in SQLite;
// retired rows deliberately stay visible here because a deep link may name an
// older native identity. Callers must reject more than one result rather than
// guessing from timestamps.
func (s *ExecutionStore) SessionsForNativeHistory(provider, nativeThreadID string) ([]agentSessionRecord, error) {
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

// NativeHistoryProviders exposes only the provider keys for an exact native
// id. Callers use one of those keys with sessionsForNativeHistory: native ids
// are never looked up as provider-less hosted identities.
func (s *ExecutionStore) NativeHistoryProviders(nativeThreadID string) ([]string, error) {
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

// NativeIDsForHosted lists every native id a hosted thread has ever claimed,
// current first. Identity resolve uses this to keep a user-assigned name that
// is still keyed to a predecessor Claude native id.
func (s *ExecutionStore) NativeIDsForHosted(hostedID string) ([]string, error) {
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
			if a, b := threads.ParseTimestamp(natives[i].lastSeen), threads.ParseTimestamp(natives[j].lastSeen); !a.Equal(b) {
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

// SessionFromCanonical returns one exact hosted record from control.sqlite.
// Unlike session, this remains correct when the in-memory registry contains
// only the currently attached subset of durable hosted history.
func (s *ExecutionStore) SessionFromCanonical(hostedID string) (agentSessionRecord, bool, error) {
	if s == nil || hostedID == "" {
		return agentSessionRecord{}, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessionFromCanonicalLocked(hostedID)
}

func (s *ExecutionStore) sessionFromCanonicalLocked(hostedID string) (agentSessionRecord, bool, error) {
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

// ListSessions returns cached hosted execution records in creation order.
func (s *ExecutionStore) ListSessions() []agentSessionRecord {
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
		if a, b := threads.ParseTimestamp(rows[i].CreatedAt), threads.ParseTimestamp(rows[j].CreatedAt); !a.Equal(b) {
			return a.Before(b)
		}
		return rows[i].ID < rows[j].ID
	})
	return rows
}

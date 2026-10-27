package app

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
	"time"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// These helpers write schema-20 rows only for migration and compatibility
// tests. Production queuedTurnStore and sendAttemptStore are read proxies;
// internal/core/prompts.Store is the only shipped writer.
func seedAcceptedTransactionalTurn(t *testing.T, store *turns.Store, hosted string, provider model.Provider, native, action, providerTurnID, prompt string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := store.Submit(ctx, turns.Submission{
		HostedThreadID: hosted, Provider: provider, NativeThreadID: native, ActionID: action,
		ProviderText: prompt, PresentationText: prompt, TextHash: sendTextHash(prompt), RequestHash: sendTextHash(prompt), Origin: turns.HumanOrigin,
	}); err != nil {
		t.Fatal(err)
	}
	token := "fixture-claim-" + action
	record, claimed, err := store.Claim(ctx, turns.ClaimRequest{HostedThreadID: hosted, Token: token, AttachmentEpoch: 1, Ready: true})
	if err != nil || !claimed || record.Outbox.ActionID != action {
		t.Fatalf("claim fixture %s: record=%+v claimed=%v err=%v", action, record, claimed, err)
	}
	binding := turns.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: provider, NativeThreadID: &native, NativeTurnID: &providerTurnID}}
	if provider == model.ProviderClaude {
		// Claude's acceptance records the submission id stamped on its line.
		binding.EventID = submissionEventID(hosted, action)
	}
	if _, err := store.Accepted(ctx, turns.Acceptance{ActionID: action, ClaimToken: token, AttachmentEpoch: 1, Binding: binding}); err != nil {
		t.Fatal(err)
	}
}

// maxQueuedTurns is the schema-20 writer's queue bound, kept by the seeders.
const maxQueuedTurns = 1000

// testPromptStore opens the transactional store the compatibility views read
// through, adopting any schema-20 rows a fixture already wrote.
func testPromptStore(t *testing.T, db *sql.DB) *turns.Store {
	t.Helper()
	store, err := turns.OpenStore(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdoptLegacy(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func (s *queuedTurnStore) snapshot() []queuedTurnRecord {
	if s == nil {
		return nil
	}
	records, err := s.turns.List(context.Background(), "")
	if err != nil {
		return nil
	}
	result := make([]queuedTurnRecord, 0, len(records))
	for _, record := range records {
		result = append(result, queuedTurnRecordFromTransactional(record))
	}
	return result
}

// The schema-20 seeders below rewrite the whole row family on disk, as the
// legacy writer did; the views then read the rows back through the store.
func (s *queuedTurnStore) legacyRows() ([]queuedTurnRecord, map[string]int, error) {
	records, err := turns.LoadQueuedTurns(context.Background(), s.db)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]int, len(records))
	for i, rec := range records {
		byID[rec.ClientActionID] = i
	}
	return records, byID, nil
}

func (s *queuedTurnStore) saveLegacyRows(records []queuedTurnRecord) error {
	return turns.SaveQueuedTurns(context.Background(), s.db, records)
}

func (s *queuedTurnStore) putSubmission(rec queuedTurnRecord) (queuedTurnRecord, bool, error) {
	if rec.State == "" {
		rec.State = submissionDelivering
	}
	return s.enqueue(rec)
}

func (s *queuedTurnStore) enqueue(rec queuedTurnRecord) (queuedTurnRecord, bool, error) {
	if s == nil {
		return queuedTurnRecord{}, false, fmt.Errorf("durable turn queue is unavailable")
	}
	records, byID, err := s.legacyRows()
	if err != nil {
		return queuedTurnRecord{}, false, err
	}
	if index, ok := byID[rec.ClientActionID]; ok {
		current := records[index]
		if rec.RequestHash == "" {
			rec.RequestHash = rec.TextHash
		}
		if current.RequestHash == "" {
			current.RequestHash = current.TextHash
		}
		if current.Provider != rec.Provider || current.TextHash != rec.TextHash || current.RequestHash != rec.RequestHash ||
			(current.HostedThreadID != "" && rec.HostedThreadID != "" && current.HostedThreadID != rec.HostedThreadID) ||
			(current.NativeThreadID != "" && rec.NativeThreadID != "" && current.NativeThreadID != rec.NativeThreadID) {
			return queuedTurnRecord{}, false, fmt.Errorf("clientActionId does not match the queued message")
		}
		changed := false
		if current.HostedThreadID == "" && rec.HostedThreadID != "" {
			current.HostedThreadID = rec.HostedThreadID
			changed = true
		}
		if current.NativeThreadID == "" && rec.NativeThreadID != "" {
			current.NativeThreadID = rec.NativeThreadID
			changed = true
		}
		if changed {
			records[index] = current
			if err := s.saveLegacyRows(records); err != nil {
				return queuedTurnRecord{}, false, err
			}
		}
		return current, false, nil
	}
	if len(records) >= maxQueuedTurns {
		return queuedTurnRecord{}, false, fmt.Errorf("durable turn queue is full")
	}
	if rec.CreatedAt == "" {
		rec.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if rec.State == "" {
		rec.State = submissionQueued
	}
	if rec.RequestHash == "" {
		rec.RequestHash = rec.TextHash
	}
	if err := s.saveLegacyRows(append(records, rec)); err != nil {
		return queuedTurnRecord{}, false, err
	}
	return rec, true, s.seedAttempt(rec)
}

// The schema-20 sender wrote a journal row beside every prompt, and the store
// reads an action only as that pair.
func (s *queuedTurnStore) seedAttempt(rec queuedTurnRecord) error {
	attempts, err := turns.LoadSendAttempts(context.Background(), s.db)
	if err != nil {
		return err
	}
	status := map[string]string{submissionQueued: sendAttemptQueued, submissionDelivering: sendAttemptSending, submissionAccepted: sendAttemptAccepted}[rec.State]
	if status == "" {
		status = rec.State
	}
	for i, attempt := range attempts {
		if attempt.ClientActionID == rec.ClientActionID {
			attempts[i].Status, attempts[i].ProviderTurnID = status, rec.ProviderTurnID
			return turns.SaveSendAttempts(context.Background(), s.db, attempts)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	attempts = append(attempts, sendAttemptRecord{ClientActionID: rec.ClientActionID, Provider: rec.Provider, NativeThreadID: rec.NativeThreadID, TextHash: rec.TextHash, Status: status, ProviderTurnID: rec.ProviderTurnID, CreatedAt: now, UpdatedAt: now})
	return turns.SaveSendAttempts(context.Background(), s.db, attempts)
}

func (s *queuedTurnStore) setState(actionID, state string) error {
	if s == nil || actionID == "" || state == "" {
		return nil
	}
	records, byID, err := s.legacyRows()
	if err != nil {
		return err
	}
	index, ok := byID[actionID]
	if !ok || records[index].State == state {
		return nil
	}
	records[index].State = state
	if err := s.saveLegacyRows(records); err != nil {
		return err
	}
	return s.seedAttempt(records[index])
}

func (s *queuedTurnStore) bindProviderTurn(actionID, turnID string) error {
	if s == nil || actionID == "" {
		return fmt.Errorf("queued turn binding is unavailable")
	}
	if turnID == "" {
		return fmt.Errorf("provider turn id is required")
	}
	records, byID, err := s.legacyRows()
	if err != nil {
		return err
	}
	index, ok := byID[actionID]
	if !ok {
		return fmt.Errorf("queued turn %s not found", actionID)
	}
	records[index].ProviderTurnID = turnID
	records[index].State = submissionAccepted
	if err := s.saveLegacyRows(records); err != nil {
		return err
	}
	return s.seedAttempt(records[index])
}

func (s *queuedTurnStore) remove(actionID string) (queuedTurnRecord, bool, error) {
	if s == nil {
		return queuedTurnRecord{}, false, nil
	}
	records, byID, err := s.legacyRows()
	if err != nil {
		return queuedTurnRecord{}, false, err
	}
	index, ok := byID[actionID]
	if !ok {
		return queuedTurnRecord{}, false, nil
	}
	rec := records[index]
	if err := s.saveLegacyRows(append(records[:index:index], records[index+1:]...)); err != nil {
		return queuedTurnRecord{}, false, err
	}
	return rec, true, nil
}

func (s *queuedTurnStore) migrateThread(provider, fromNative, toNative string) error {
	if s == nil || fromNative == "" || toNative == "" || fromNative == toNative {
		return nil
	}
	records, _, err := s.legacyRows()
	if err != nil {
		return err
	}
	changed := false
	for i := range records {
		if records[i].Provider == provider && records[i].NativeThreadID == fromNative {
			records[i].NativeThreadID = toNative
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.saveLegacyRows(records)
}

func (s *sendAttemptStore) snapshot() []sendAttemptRecord {
	if s == nil {
		return nil
	}
	attempts, err := s.turns.ListAttempts(context.Background())
	if err != nil {
		return nil
	}
	result := make([]sendAttemptRecord, 0, len(attempts))
	for _, attempt := range attempts {
		record, _, _ := s.turns.Get(context.Background(), attempt.ActionID)
		result = append(result, sendAttemptRecordFromTransactional(record, attempt))
	}
	return result
}

func sendAttemptTerminal(status string) bool {
	switch status {
	case sendAttemptQueued, sendAttemptSending, sendAttemptTimeout, "unknown", "parked":
		return false
	default:
		return true
	}
}

func (s *sendAttemptStore) legacyRows() (map[string]sendAttemptRecord, error) {
	records, err := turns.LoadSendAttempts(context.Background(), s.db)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]sendAttemptRecord, len(records))
	for _, rec := range records {
		if rec.CreatedAt == "" {
			rec.CreatedAt = rec.UpdatedAt
		}
		byID[rec.ClientActionID] = rec
	}
	return byID, nil
}

func (s *sendAttemptStore) put(rec sendAttemptRecord) error {
	if s == nil {
		return nil
	}
	byID, err := s.legacyRows()
	if err != nil {
		return err
	}
	rec.RunID = s.runID
	now := time.Now().UTC().Format(time.RFC3339Nano)
	previous, existed := byID[rec.ClientActionID]
	if existed {
		if rec.CreatedAt == "" {
			rec.CreatedAt = previous.CreatedAt
		}
		if !rec.Confirmed && previous.Confirmed {
			rec.Confirmed, rec.ConfirmedAt = previous.Confirmed, previous.ConfirmedAt
		}
		if previous.ProviderTurnID != "" && rec.ProviderTurnID != "" && previous.ProviderTurnID != rec.ProviderTurnID {
			rec.ProviderTurnID = previous.ProviderTurnID
			rec.IdentityConflict = "action_bound_to_multiple_provider_turns"
		}
		if rec.CanonicalMessageID == "" {
			rec.CanonicalMessageID = previous.CanonicalMessageID
		}
		if rec.IdentityConflict == "" {
			rec.IdentityConflict = previous.IdentityConflict
		}
	}
	if rec.Confirmed {
		rec.Status = sendAttemptAccepted
		rec.Reason = ""
	}
	if rec.CreatedAt == "" {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	byID[rec.ClientActionID] = rec
	return s.saveLegacyRows(byID)
}

func (s *sendAttemptStore) bindCanonicalMessage(actionID, canonicalID string) error {
	if s == nil || actionID == "" || canonicalID == "" {
		return nil
	}
	byID, err := s.legacyRows()
	if err != nil {
		return err
	}
	rec, ok := byID[actionID]
	if !ok {
		return nil
	}
	if rec.CanonicalMessageID != "" && rec.CanonicalMessageID != canonicalID {
		rec.IdentityConflict = "action_bound_to_multiple_canonical_messages"
	} else {
		rec.CanonicalMessageID = canonicalID
	}
	rec.RunID = s.runID
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	byID[actionID] = rec
	return s.saveLegacyRows(byID)
}

func (s *sendAttemptStore) confirm(actionID string) error {
	_, err := s.confirmOnce(actionID)
	return err
}

func (s *sendAttemptStore) confirmOnce(actionID string) (bool, error) {
	if s == nil || actionID == "" {
		return false, nil
	}
	byID, err := s.legacyRows()
	if err != nil {
		return false, err
	}
	rec, ok := byID[actionID]
	if !ok || sendAttemptTerminal(rec.Status) && rec.Status != sendAttemptAccepted {
		return false, nil
	}
	if rec.Provider == "claude" {
		return false, fmt.Errorf("Claude confirmation requires exact completed durable-turn evidence")
	}
	return s.confirmRecord(byID, rec)
}

func (s *sendAttemptStore) confirmClaudeExactCompleted(actionID, nativeThreadID, providerTurnID string) (bool, error) {
	if s == nil || actionID == "" || nativeThreadID == "" || providerTurnID == "" {
		return false, fmt.Errorf("Claude confirmation identity is incomplete")
	}
	byID, err := s.legacyRows()
	if err != nil {
		return false, err
	}
	rec, ok := byID[actionID]
	if !ok {
		return false, fmt.Errorf("Claude send attempt is missing")
	}
	if rec.Provider != "claude" || rec.NativeThreadID != nativeThreadID || rec.ProviderTurnID != providerTurnID || rec.IdentityConflict != "" {
		return false, fmt.Errorf("Claude send attempt identity does not match exact completed turn")
	}
	if rec.Status != sendAttemptAccepted {
		return false, fmt.Errorf("Claude send attempt status %q is not accepted", rec.Status)
	}
	return s.confirmRecord(byID, rec)
}

func (s *sendAttemptStore) confirmRecord(byID map[string]sendAttemptRecord, rec sendAttemptRecord) (bool, error) {
	newlyConfirmed := !rec.Confirmed
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rec.Status = sendAttemptAccepted
	rec.Reason = ""
	rec.RunID = s.runID
	if !rec.Confirmed {
		rec.Confirmed, rec.ConfirmedAt = true, now
	}
	if rec.CreatedAt == "" {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	byID[rec.ClientActionID] = rec
	if err := s.saveLegacyRows(byID); err != nil {
		return false, err
	}
	return newlyConfirmed, nil
}

// saveLegacyRows keeps the schema-20 journal's retention: every non-terminal
// row, and the newest maxSendAttempts terminal rows per provider thread.
func (s *sendAttemptStore) saveLegacyRows(byID map[string]sendAttemptRecord) error {
	nonTerminal := make([]sendAttemptRecord, 0, len(byID))
	terminalByThread := make(map[string][]sendAttemptRecord)
	for _, rec := range byID {
		if sendAttemptTerminal(rec.Status) {
			key := rec.Provider + "\x00" + rec.NativeThreadID
			terminalByThread[key] = append(terminalByThread[key], rec)
		} else {
			nonTerminal = append(nonTerminal, rec)
		}
	}
	sort.Slice(nonTerminal, func(i, j int) bool { return nonTerminal[i].UpdatedAt > nonTerminal[j].UpdatedAt })
	terminal := make([]sendAttemptRecord, 0, len(byID))
	for _, records := range terminalByThread {
		sort.Slice(records, func(i, j int) bool { return records[i].UpdatedAt > records[j].UpdatedAt })
		if len(records) > maxSendAttempts {
			records = records[:maxSendAttempts]
		}
		terminal = append(terminal, records...)
	}
	return turns.SaveSendAttempts(context.Background(), s.db, append(nonTerminal, terminal...))
}

func (s *sendAttemptStore) migrateThread(provider, fromNative, toNative string) error {
	if s == nil || fromNative == "" || toNative == "" || fromNative == toNative {
		return nil
	}
	byID, err := s.legacyRows()
	if err != nil {
		return err
	}
	changed := false
	for id, rec := range byID {
		if rec.Provider == provider && rec.NativeThreadID == fromNative {
			rec.NativeThreadID = toNative
			byID[id] = rec
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.saveLegacyRows(byID)
}

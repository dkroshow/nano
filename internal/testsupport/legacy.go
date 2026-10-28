package testsupport

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
	"time"

	turns "github.com/dkroshow/nano/internal/core/prompts"
)

// MaxQueuedTurns is the schema-20 writer's queue bound, kept by the seeders.
const MaxQueuedTurns = 1000

// MaxLegacySendAttempts preserves the schema-20 fixture retention limit.
const MaxLegacySendAttempts = 100

// PromptStore opens the transactional store the compatibility views read
// through, adopting any schema-20 rows a fixture already wrote.
func PromptStore(t testing.TB, db *sql.DB) *turns.Store {
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

func QueueSnapshot(s *turns.LegacyQueue) []turns.QueuedTurn {
	if s == nil {
		return nil
	}
	records, err := s.TransactionStore().List(context.Background(), "")
	if err != nil {
		return nil
	}
	result := make([]turns.QueuedTurn, 0, len(records))
	for _, record := range records {
		result = append(result, turns.LegacyQueuedTurn(record))
	}
	return result
}

// The schema-20 seeders below rewrite the whole row family on disk, as the
// legacy writer did; the views then read the rows back through the store.
func QueueLegacyRows(s *turns.LegacyQueue) ([]turns.QueuedTurn, map[string]int, error) {
	records, err := turns.LoadQueuedTurns(context.Background(), s.Database())
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]int, len(records))
	for i, rec := range records {
		byID[rec.ClientActionID] = i
	}
	return records, byID, nil
}

func QueueSaveLegacyRows(s *turns.LegacyQueue, records []turns.QueuedTurn) error {
	return turns.SaveQueuedTurns(context.Background(), s.Database(), records)
}

func QueuePutSubmission(s *turns.LegacyQueue, rec turns.QueuedTurn) (turns.QueuedTurn, bool, error) {
	if rec.State == "" {
		rec.State = string(turns.Delivering)
	}
	return QueueEnqueue(s, rec)
}

func QueueEnqueue(s *turns.LegacyQueue, rec turns.QueuedTurn) (turns.QueuedTurn, bool, error) {
	if s == nil {
		return turns.QueuedTurn{}, false, fmt.Errorf("durable turn queue is unavailable")
	}
	records, byID, err := QueueLegacyRows(s)
	if err != nil {
		return turns.QueuedTurn{}, false, err
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
			return turns.QueuedTurn{}, false, fmt.Errorf("clientActionId does not match the queued message")
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
			if err := QueueSaveLegacyRows(s, records); err != nil {
				return turns.QueuedTurn{}, false, err
			}
		}
		return current, false, nil
	}
	if len(records) >= MaxQueuedTurns {
		return turns.QueuedTurn{}, false, fmt.Errorf("durable turn queue is full")
	}
	if rec.CreatedAt == "" {
		rec.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if rec.State == "" {
		rec.State = string(turns.Queued)
	}
	if rec.RequestHash == "" {
		rec.RequestHash = rec.TextHash
	}
	if err := QueueSaveLegacyRows(s, append(records, rec)); err != nil {
		return turns.QueuedTurn{}, false, err
	}
	return rec, true, QueueSeedAttempt(s, rec)
}

// The schema-20 sender wrote a journal row beside every prompt, and the store
// reads an action only as that pair.
func QueueSeedAttempt(s *turns.LegacyQueue, rec turns.QueuedTurn) error {
	attempts, err := turns.LoadSendAttempts(context.Background(), s.Database())
	if err != nil {
		return err
	}
	status := map[string]string{string(turns.Queued): string(turns.JournalQueued), string(turns.Delivering): string(turns.JournalSending), string(turns.Accepted): string(turns.JournalAccepted)}[rec.State]
	if status == "" {
		status = rec.State
	}
	for i, attempt := range attempts {
		if attempt.ClientActionID == rec.ClientActionID {
			attempts[i].Status, attempts[i].ProviderTurnID = status, rec.ProviderTurnID
			return turns.SaveSendAttempts(context.Background(), s.Database(), attempts)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	attempts = append(attempts, turns.SendAttempt{ClientActionID: rec.ClientActionID, Provider: rec.Provider, NativeThreadID: rec.NativeThreadID, TextHash: rec.TextHash, Status: status, ProviderTurnID: rec.ProviderTurnID, CreatedAt: now, UpdatedAt: now})
	return turns.SaveSendAttempts(context.Background(), s.Database(), attempts)
}

func QueueSetState(s *turns.LegacyQueue, actionID, state string) error {
	if s == nil || actionID == "" || state == "" {
		return nil
	}
	records, byID, err := QueueLegacyRows(s)
	if err != nil {
		return err
	}
	index, ok := byID[actionID]
	if !ok || records[index].State == state {
		return nil
	}
	records[index].State = state
	if err := QueueSaveLegacyRows(s, records); err != nil {
		return err
	}
	return QueueSeedAttempt(s, records[index])
}

func QueueBindProviderTurn(s *turns.LegacyQueue, actionID, turnID string) error {
	if s == nil || actionID == "" {
		return fmt.Errorf("queued turn binding is unavailable")
	}
	if turnID == "" {
		return fmt.Errorf("provider turn id is required")
	}
	records, byID, err := QueueLegacyRows(s)
	if err != nil {
		return err
	}
	index, ok := byID[actionID]
	if !ok {
		return fmt.Errorf("queued turn %s not found", actionID)
	}
	records[index].ProviderTurnID = turnID
	records[index].State = string(turns.Accepted)
	if err := QueueSaveLegacyRows(s, records); err != nil {
		return err
	}
	return QueueSeedAttempt(s, records[index])
}

func QueueRemove(s *turns.LegacyQueue, actionID string) (turns.QueuedTurn, bool, error) {
	if s == nil {
		return turns.QueuedTurn{}, false, nil
	}
	records, byID, err := QueueLegacyRows(s)
	if err != nil {
		return turns.QueuedTurn{}, false, err
	}
	index, ok := byID[actionID]
	if !ok {
		return turns.QueuedTurn{}, false, nil
	}
	rec := records[index]
	if err := QueueSaveLegacyRows(s, append(records[:index:index], records[index+1:]...)); err != nil {
		return turns.QueuedTurn{}, false, err
	}
	return rec, true, nil
}

func QueueMigrateThread(s *turns.LegacyQueue, provider, fromNative, toNative string) error {
	if s == nil || fromNative == "" || toNative == "" || fromNative == toNative {
		return nil
	}
	records, _, err := QueueLegacyRows(s)
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
	return QueueSaveLegacyRows(s, records)
}

func JournalSnapshot(s *turns.LegacyJournal) []turns.SendAttempt {
	if s == nil {
		return nil
	}
	attempts, err := s.TransactionStore().ListAttempts(context.Background())
	if err != nil {
		return nil
	}
	result := make([]turns.SendAttempt, 0, len(attempts))
	for _, attempt := range attempts {
		record, _, _ := s.TransactionStore().Get(context.Background(), attempt.ActionID)
		result = append(result, turns.LegacySendAttempt(record, attempt))
	}
	return result
}

func sendAttemptTerminal(status string) bool {
	switch status {
	case string(turns.JournalQueued), string(turns.JournalSending), "timeout", "unknown", "parked":
		return false
	default:
		return true
	}
}

func JournalLegacyRows(s *turns.LegacyJournal) (map[string]turns.SendAttempt, error) {
	records, err := turns.LoadSendAttempts(context.Background(), s.Database())
	if err != nil {
		return nil, err
	}
	byID := make(map[string]turns.SendAttempt, len(records))
	for _, rec := range records {
		if rec.CreatedAt == "" {
			rec.CreatedAt = rec.UpdatedAt
		}
		byID[rec.ClientActionID] = rec
	}
	return byID, nil
}

func JournalPut(s *turns.LegacyJournal, rec turns.SendAttempt) error {
	if s == nil {
		return nil
	}
	byID, err := JournalLegacyRows(s)
	if err != nil {
		return err
	}
	rec.RunID = s.RunIdentity()
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
		rec.Status = string(turns.JournalAccepted)
		rec.Reason = ""
	}
	if rec.CreatedAt == "" {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	byID[rec.ClientActionID] = rec
	return JournalSaveLegacyRows(s, byID)
}

func JournalBindCanonicalMessage(s *turns.LegacyJournal, actionID, canonicalID string) error {
	if s == nil || actionID == "" || canonicalID == "" {
		return nil
	}
	byID, err := JournalLegacyRows(s)
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
	rec.RunID = s.RunIdentity()
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	byID[actionID] = rec
	return JournalSaveLegacyRows(s, byID)
}

func JournalConfirm(s *turns.LegacyJournal, actionID string) error {
	_, err := JournalConfirmOnce(s, actionID)
	return err
}

func JournalConfirmOnce(s *turns.LegacyJournal, actionID string) (bool, error) {
	if s == nil || actionID == "" {
		return false, nil
	}
	byID, err := JournalLegacyRows(s)
	if err != nil {
		return false, err
	}
	rec, ok := byID[actionID]
	if !ok || sendAttemptTerminal(rec.Status) && rec.Status != string(turns.JournalAccepted) {
		return false, nil
	}
	if rec.Provider == "claude" {
		return false, fmt.Errorf("Claude confirmation requires exact completed durable-turn evidence")
	}
	return JournalConfirmRecord(s, byID, rec)
}

func JournalConfirmClaudeExactCompleted(s *turns.LegacyJournal, actionID, nativeThreadID, providerTurnID string) (bool, error) {
	if s == nil || actionID == "" || nativeThreadID == "" || providerTurnID == "" {
		return false, fmt.Errorf("Claude confirmation identity is incomplete")
	}
	byID, err := JournalLegacyRows(s)
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
	if rec.Status != string(turns.JournalAccepted) {
		return false, fmt.Errorf("Claude send attempt status %q is not accepted", rec.Status)
	}
	return JournalConfirmRecord(s, byID, rec)
}

func JournalConfirmRecord(s *turns.LegacyJournal, byID map[string]turns.SendAttempt, rec turns.SendAttempt) (bool, error) {
	newlyConfirmed := !rec.Confirmed
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rec.Status = string(turns.JournalAccepted)
	rec.Reason = ""
	rec.RunID = s.RunIdentity()
	if !rec.Confirmed {
		rec.Confirmed, rec.ConfirmedAt = true, now
	}
	if rec.CreatedAt == "" {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	byID[rec.ClientActionID] = rec
	if err := JournalSaveLegacyRows(s, byID); err != nil {
		return false, err
	}
	return newlyConfirmed, nil
}

// saveLegacyRows keeps the schema-20 journal's retention: every non-terminal
// row, and the newest MaxLegacySendAttempts terminal rows per provider thread.
func JournalSaveLegacyRows(s *turns.LegacyJournal, byID map[string]turns.SendAttempt) error {
	nonTerminal := make([]turns.SendAttempt, 0, len(byID))
	terminalByThread := make(map[string][]turns.SendAttempt)
	for _, rec := range byID {
		if sendAttemptTerminal(rec.Status) {
			key := rec.Provider + "\x00" + rec.NativeThreadID
			terminalByThread[key] = append(terminalByThread[key], rec)
		} else {
			nonTerminal = append(nonTerminal, rec)
		}
	}
	sort.Slice(nonTerminal, func(i, j int) bool { return nonTerminal[i].UpdatedAt > nonTerminal[j].UpdatedAt })
	terminal := make([]turns.SendAttempt, 0, len(byID))
	for _, records := range terminalByThread {
		sort.Slice(records, func(i, j int) bool { return records[i].UpdatedAt > records[j].UpdatedAt })
		if len(records) > MaxLegacySendAttempts {
			records = records[:MaxLegacySendAttempts]
		}
		terminal = append(terminal, records...)
	}
	return turns.SaveSendAttempts(context.Background(), s.Database(), append(nonTerminal, terminal...))
}

func JournalMigrateThread(s *turns.LegacyJournal, provider, fromNative, toNative string) error {
	if s == nil || fromNative == "" || toNative == "" || fromNative == toNative {
		return nil
	}
	byID, err := JournalLegacyRows(s)
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
	return JournalSaveLegacyRows(s, byID)
}

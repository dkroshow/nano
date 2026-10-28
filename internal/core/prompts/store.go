package prompts

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

var (
	ErrActionConflict  = errors.New("prompts: action id conflicts with submitted request")
	ErrClaimHeld       = errors.New("prompts: thread already has a dispatch claim")
	ErrClaimMismatch   = errors.New("prompts: dispatch claim does not match")
	ErrBindingConflict = errors.New("prompts: exact binding conflicts with retained evidence")

	errUnreadableBinding = errors.New("prompts: unreadable binding")
)

const maximumSequence = int64(1<<63 - 1)

// ReasonNextTurn is the queued journal reason of a prompt held for the next
// turn, by its sender or by a turn that ended before it could join. It is why
// the prompt waits and keeps it out of a running turn. It means nothing once
// the prompt leaves the queue: acceptance clears it, and every other outcome
// replaces it with its own reason. The caller's original choice stays in the
// request hash.
const ReasonNextTurn = "next_turn"

// ReasonStopped is the journal reason of a prompt held for the person because
// the operator stopped the turn that dropped it.
const ReasonStopped = "stopped"

// ReasonTurnEnded is the journal reason of a prompt held for the person
// because the saved transcript shows its turn ended without its row.
const ReasonTurnEnded = "turn_ended"

// Store owns prompt delivery transactions on the threads database. The caller
// publishes returned values only after each method has committed successfully.
type Store struct{ db *sql.DB }

func OpenStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("prompts: threads database is required")
	}
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return nil, err
	}
	// Settlement records each Claude member's owed completion, which only the
	// current schema stores.
	if version != TurnSchemaVersion {
		return nil, fmt.Errorf("prompts: schema %d is not the supported schema %d", version, TurnSchemaVersion)
	}
	return &Store{db: db}, nil
}

// AdoptLegacy promotes only exact prompt/journal pairs created by schema 20.
// It runs before provider attachment. A delivery that was in flight, or an
// accepted row without an exact provider turn, becomes uncertain and is never
// retried automatically. No row is deleted and no prompt text is scrubbed.
func (s *Store) AdoptLegacy(ctx context.Context) (AdoptionReport, error) {
	var report AdoptionReport
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, recordSelect+` WHERE q.revision=0 ORDER BY q.position`)
	if err != nil {
		return report, err
	}
	var legacy []Record
	for rows.Next() {
		record, scanErr := scanRecord(rows)
		if scanErr != nil {
			rows.Close()
			return report, scanErr
		}
		legacy = append(legacy, record)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return report, err
	}
	for _, record := range legacy {
		attempt, loadErr := loadAttempt(ctx, tx, record.Outbox.ActionID)
		if errors.Is(loadErr, sql.ErrNoRows) {
			report.Unmanaged++
			report.Diagnostics = append(report.Diagnostics, model.Diagnostic{Code: "legacy_attempt_missing", Severity: "warning", Message: "legacy outbox row has no matching delivery journal"})
			continue
		}
		if loadErr != nil {
			return report, loadErr
		}
		if attempt.Revision != 0 || record.Outbox.HostedThreadID == "" ||
			(attempt.HostedThreadID != "" && attempt.HostedThreadID != record.Outbox.HostedThreadID) ||
			attempt.Provider != record.Provider || attempt.ActionID != record.Outbox.ActionID ||
			(record.ProviderTurnID != "" && attempt.ProviderTurnID != "" && record.ProviderTurnID != attempt.ProviderTurnID) {
			report.Unmanaged++
			report.Diagnostics = append(report.Diagnostics, model.Diagnostic{Code: "legacy_pair_conflict", Severity: "warning", Message: "legacy outbox and journal identity do not form one exact prompt"})
			continue
		}
		state := record.Outbox.State
		if state == "" {
			state = Queued
		}
		journal := attempt.State
		if state == Delivering || journal == JournalSending || state == Accepted && (record.NativeThreadID == "" || attempt.ProviderTurnID == "") {
			state, journal = Uncertain, JournalUnknown
			report.Uncertain++
		}
		bindingJSON, bindingKey := "", ""
		providerTurnID := firstValue(record.ProviderTurnID, attempt.ProviderTurnID)
		if providerTurnID != "" && record.NativeThreadID != "" {
			turnID, nativeID := providerTurnID, record.NativeThreadID
			binding := Binding{Scope: model.Scope{
				Kind: model.ScopeTurn, Provider: record.Provider,
				NativeThreadID: &nativeID, NativeTurnID: &turnID,
			}}
			bindingJSON, bindingKey, _, err = qualifyBinding(record.Outbox.HostedThreadID, binding)
			if err != nil {
				return report, err
			}
		}
		result, updateErr := tx.ExecContext(ctx, `UPDATE queued_turns SET state=?,revision=1,delivery_rank=?,binding_json=?,binding_key=?,claim_token='',claim_epoch=0
			WHERE client_action_id=? AND revision=0`, state, originRank(record.Origin), bindingJSON, bindingKey, record.Outbox.ActionID)
		if updateErr != nil {
			if errors.Is(mapBindingError(updateErr), ErrBindingConflict) {
				report.Unmanaged++
				report.Diagnostics = append(report.Diagnostics, model.Diagnostic{Code: "legacy_binding_conflict", Severity: "warning", Message: "legacy exact turn is already claimed by another action"})
				continue
			}
			return report, updateErr
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return report, errors.New("prompts: legacy outbox changed during adoption")
		}
		result, updateErr = tx.ExecContext(ctx, `UPDATE send_attempts SET hosted_thread_id=?,status=?,revision=1,binding_json=?,claim_token=''
			WHERE client_action_id=? AND revision=0`, record.Outbox.HostedThreadID, journal, bindingJSON, record.Outbox.ActionID)
		if updateErr != nil {
			return report, updateErr
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return report, errors.New("prompts: legacy journal changed during adoption")
		}
		report.Adopted++
	}
	if err := tx.Commit(); err != nil {
		return AdoptionReport{}, err
	}
	return report, nil
}

// ParkIncompleteAtBoot retains malformed outbox evidence while making every
// nonterminal malformed row permanently ineligible for dispatch. It also
// returns previously parked malformed rows so their diagnostic survives later
// restarts without a second mutation.
func (s *Store) ParkIncompleteAtBoot(ctx context.Context) ([]BootPark, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	type candidate struct {
		park     BootPark
		state    DeliveryState
		position int64
		revision uint64
	}
	// Terminal rows cannot regress to parked and confirmed rows intentionally
	// have no text, so they are left on disk rather than scanned.
	rows, err := tx.QueryContext(ctx, `SELECT client_action_id,provider,hosted_thread_id,native_thread_id,text,state,position,revision FROM queued_turns WHERE state NOT IN ('confirmed','refused','expired') ORDER BY position`)
	if err != nil {
		return nil, err
	}
	var candidates []candidate
	for rows.Next() {
		var row candidate
		var provider, text, state string
		var revision int64
		if err := rows.Scan(&row.park.ActionID, &provider, &row.park.HostedThreadID, &row.park.NativeThreadID, &text, &state, &row.position, &revision); err != nil {
			rows.Close()
			return nil, err
		}
		if revision < 0 {
			rows.Close()
			return nil, errors.New("prompts: invalid persisted queue revision")
		}
		row.park.Provider = model.Provider(provider)
		row.state, row.revision = DeliveryState(state), uint64(revision)
		row.park.Missing = incompleteOutboxFields(row.park, text)
		if len(row.park.Missing) != 0 {
			candidates = append(candidates, row)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}

	var parked []BootPark
	for _, row := range candidates {
		switch row.state {
		case Confirmed, Refused, Expired:
			// Confirmed rows intentionally have no text. Terminal states are
			// already non-deliverable and cannot regress to parked.
			continue
		case Parked:
			parked = append(parked, row.park)
			continue
		}
		reason := "boot_incomplete_outbox:" + strings.Join(row.park.Missing, ",")
		updatedAt := eventTime("")
		result, err := tx.ExecContext(ctx, `UPDATE queued_turns SET state='parked',claim_token='',revision=? WHERE position=? AND revision=?`, row.revision+1, row.position, row.revision)
		if err != nil {
			return nil, err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return nil, errors.New("prompts: incomplete boot row changed during parking")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE send_attempts SET status='unknown',reason=?,updated_at=?,claim_token='',revision=revision+1 WHERE client_action_id=?`, reason, updatedAt, row.park.ActionID); err != nil {
			return nil, err
		}
		parked = append(parked, row.park)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return parked, nil
}

func incompleteOutboxFields(row BootPark, text string) []string {
	var missing []string
	if strings.TrimSpace(row.ActionID) == "" {
		missing = append(missing, "action_id")
	}
	if strings.TrimSpace(string(row.Provider)) == "" {
		missing = append(missing, "provider")
	}
	if strings.TrimSpace(row.HostedThreadID) == "" && strings.TrimSpace(row.NativeThreadID) == "" {
		missing = append(missing, "thread_identity")
	}
	if text == "" {
		missing = append(missing, "text")
	}
	return missing
}

// List is ordered by immutable submission sequence. An empty owner lists all
// records for migration and diagnostics callers.
func (s *Store) List(ctx context.Context, hostedThreadID string) ([]Record, error) {
	return list(ctx, s.db, hostedThreadID)
}

func list(ctx context.Context, db rowsQueryer, hostedThreadID string) ([]Record, error) {
	if hostedThreadID == "" {
		return recordsFrom(ctx, db, ` ORDER BY q.position`)
	}
	return recordsFrom(ctx, db, ` WHERE q.hosted_thread_id=? ORDER BY q.position`, hostedThreadID)
}

// Journal runs the store's two listing reads, List and ListThreadAttempts,
// through another handle: a read-only pool, for a caller that only displays
// the rows (the browser's delivery journal), so those reads do not queue on
// the writer connection. A read that informs a write stays on Store.
type Journal struct{ db rowsQueryer }

func NewJournal(db *sql.DB) Journal { return Journal{db: db} }

func (j Journal) List(ctx context.Context, hostedThreadID string) ([]Record, error) {
	return list(ctx, j.db, hostedThreadID)
}

func (j Journal) ListThreadAttempts(ctx context.Context, provider model.Provider, nativeThreadID string) ([]AttemptStatus, error) {
	return listThreadAttempts(ctx, j.db, provider, nativeThreadID)
}

// InFlight reports whether the thread has a prompt still on its way or in a
// provider turn not known to have ended: queued, delivering, uncertain, or
// accepted with no terminal turn status.
func (s *Store) InFlight(ctx context.Context, hostedThreadID string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM queued_turns WHERE hosted_thread_id=? AND (state IN (?,?,?) OR (state=? AND COALESCE(provider_terminal_status,'')='')) LIMIT 1`,
		hostedThreadID, Queued, Delivering, Uncertain, Accepted).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Queued lists the thread's queued prompts in delivery order: by delivery
// class, then submission order. It reads only those rows.
func (s *Store) Queued(ctx context.Context, hostedThreadID string) ([]Record, error) {
	return s.records(ctx, ` WHERE q.hosted_thread_id=? AND q.state='queued' ORDER BY q.delivery_rank,q.position`, hostedThreadID)
}

// ProviderTurn lists, in submission order and at most two, the thread's
// prompts accepted into one provider turn, so a caller can refuse an
// ambiguous turn. A prompt adopted from a legacy journal keeps its turn only
// in its binding. An empty native thread matches any.
func (s *Store) ProviderTurn(ctx context.Context, hostedThreadID string, provider model.Provider, nativeThreadID, turnID string) ([]Record, error) {
	return s.records(ctx, providerTurnClause+` LIMIT 2`, hostedThreadID, provider, nativeThreadID, nativeThreadID, turnID, turnID)
}

// providerTurnClause selects one thread's prompts bound to one provider turn,
// in delivery order.
const providerTurnClause = ` WHERE q.hosted_thread_id=? AND q.provider=? AND (?='' OR q.native_thread_id=?)
		AND (q.provider_turn_id=? OR q.provider_turn_id='' AND q.binding_json<>'' AND json_extract(q.binding_json,'$.scope.nativeTurnId')=?)
		ORDER BY q.position`

// TurnEvents lists the exact event ids of the prompts bound to one provider
// turn of one native thread. Prompts steered into a running turn share it, and
// each keeps its own event; a prompt bound only by the turn contributes none.
// An id counts only when its binding names that provider, thread and turn at
// the root, not a child agent's call. It reads binding evidence only, never
// prompt text, and a binding it cannot read is an error, not an absent claim.
func (s *Store) TurnEvents(ctx context.Context, hostedThreadID string, provider model.Provider, nativeThreadID, turnID string) ([]string, error) {
	if hostedThreadID == "" || provider == "" || nativeThreadID == "" || turnID == "" {
		return nil, errors.New("prompts: turn events need an exact hosted thread, provider, native thread and turn")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT q.binding_json FROM queued_turns q`+providerTurnClause,
		hostedThreadID, provider, nativeThreadID, nativeThreadID, turnID, turnID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if raw == "" {
			continue
		}
		var binding Binding
		if err := json.Unmarshal([]byte(raw), &binding); err != nil {
			return nil, fmt.Errorf("prompts: unreadable binding in turn %s: %w", turnID, err)
		}
		scope := binding.Scope
		if binding.EventID == "" || scope.Provider != provider || scope.NativeParentCallID != nil ||
			scope.NativeThreadID == nil || *scope.NativeThreadID != nativeThreadID ||
			scope.NativeTurnID == nil || *scope.NativeTurnID != turnID {
			continue
		}
		events = append(events, binding.EventID)
	}
	return events, rows.Err()
}

func (s *Store) records(ctx context.Context, clause string, args ...any) ([]Record, error) {
	return recordsFrom(ctx, s.db, clause, args...)
}

func recordsFrom(ctx context.Context, db rowsQueryer, clause string, args ...any) ([]Record, error) {
	rows, err := db.QueryContext(ctx, recordSelect+clause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Record
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

// ListAttempts is a read-only compatibility view ordered by retained journal
// position, including orphaned legacy evidence.
func (s *Store) ListAttempts(ctx context.Context) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, attemptSelect+` ORDER BY position`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Attempt
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, attempt)
	}
	return result, rows.Err()
}

type rowsQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// ListThreadAttempts reads only one provider thread's journal rows. The left
// join retains legacy attempts that have no matching outbox row.
func (s *Store) ListThreadAttempts(ctx context.Context, provider model.Provider, nativeThreadID string) ([]AttemptStatus, error) {
	return listThreadAttempts(ctx, s.db, provider, nativeThreadID)
}

func listThreadAttempts(ctx context.Context, db rowsQueryer, provider model.Provider, nativeThreadID string) ([]AttemptStatus, error) {
	rows, err := db.QueryContext(ctx, threadAttemptSelect, provider, nativeThreadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AttemptStatus
	for rows.Next() {
		entry, err := scanAttemptStatus(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

func (s *Store) Get(ctx context.Context, actionID string) (Record, Attempt, error) {
	if actionID == "" {
		return Record{}, Attempt{}, errors.New("prompts: action id is required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Record{}, Attempt{}, err
	}
	defer tx.Rollback()
	record, attempt, err := loadAction(ctx, tx, actionID)
	if err != nil {
		return Record{}, Attempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, Attempt{}, err
	}
	return record, attempt, nil
}

// GetAttempt reads journal evidence even when a schema-20 attempt had no
// matching outbox row and therefore could not be adopted for delivery.
func (s *Store) GetAttempt(ctx context.Context, actionID string) (Attempt, error) {
	if actionID == "" {
		return Attempt{}, errors.New("prompts: action id is required")
	}
	return scanAttempt(s.db.QueryRowContext(ctx, attemptSelect+` WHERE client_action_id=?`, actionID))
}

// RebindNativeThread moves queued rows to a provider-declared successor. An
// accepted row's native thread and provider turn identity are permanent.
func (s *Store) RebindNativeThread(ctx context.Context, hostedThreadID string, provider model.Provider, fromNative, toNative string) ([]OutboxEntry, error) {
	if hostedThreadID == "" || provider == "" || fromNative == "" || toNative == "" {
		return nil, errors.New("prompts: native thread rebind requires owner, provider, predecessor, and successor")
	}
	if fromNative == toNative {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, recordSelect+` WHERE q.hosted_thread_id=? AND q.provider=? AND q.native_thread_id=? AND q.state='queued' AND q.revision>0 ORDER BY q.position`, hostedThreadID, provider, fromNative)
	if err != nil {
		return nil, err
	}
	var records []Record
	for rows.Next() {
		record, scanErr := scanRecord(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		records = append(records, record)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	outbox := make([]OutboxEntry, 0, len(records))
	for _, record := range records {
		attempt, err := loadAttempt(ctx, tx, record.Outbox.ActionID)
		if err != nil {
			return nil, err
		}
		if attempt.HostedThreadID != hostedThreadID || attempt.Provider != provider || attempt.NativeThreadID != fromNative || attempt.Revision != record.Revision {
			return nil, errors.New("prompts: native thread rebind found an unpaired action")
		}
		raw, key := "", ""
		if record.Outbox.Binding != nil {
			binding := *record.Outbox.Binding
			if binding.Scope.NativeThreadID == nil || *binding.Scope.NativeThreadID != fromNative {
				return nil, ErrBindingConflict
			}
			native := toNative
			binding.Scope.NativeThreadID = &native
			raw, key, _, err = qualifyBinding(hostedThreadID, binding)
			if err != nil {
				return nil, err
			}
		}
		newRevision := record.Revision + 1
		result, err := tx.ExecContext(ctx, `UPDATE queued_turns SET native_thread_id=?,binding_json=?,binding_key=?,revision=?
			WHERE client_action_id=? AND revision=?`, toNative, raw, key, newRevision, record.Outbox.ActionID, record.Revision)
		if err != nil {
			return nil, mapBindingError(err)
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return nil, errors.New("prompts: native thread rebind queue revision mismatch")
		}
		result, err = tx.ExecContext(ctx, `UPDATE send_attempts SET native_thread_id=?,binding_json=?,revision=?
			WHERE client_action_id=? AND revision=?`, toNative, raw, newRevision, record.Outbox.ActionID, record.Revision)
		if err != nil {
			return nil, err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return nil, errors.New("prompts: native thread rebind journal revision mismatch")
		}
		rebound, err := loadRecord(ctx, tx, record.Outbox.ActionID)
		if err != nil {
			return nil, err
		}
		outbox = append(outbox, rebound.Outbox)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return outbox, nil
}

func (s *Store) Submit(ctx context.Context, request Submission) (Record, bool, error) {
	request.Origin = NormalizeOrigin(request.Origin)
	if request.HostedThreadID == "" || request.Provider == "" || request.ActionID == "" || request.ProviderText == "" || request.TextHash == "" || request.RequestHash == "" {
		return Record{}, false, errors.New("prompts: submission requires owner, provider, action, text, and hashes")
	}
	if request.PresentationText == "" {
		request.PresentationText = request.ProviderText
	}
	request.CreatedAt = eventTime(request.CreatedAt)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, false, err
	}
	defer tx.Rollback()
	prior, err := loadRecord(ctx, tx, request.ActionID)
	if err == nil {
		if !sameAdmission(prior, request) {
			return Record{}, false, ErrActionConflict
		}
		return prior, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, err
	}
	if err := insertSubmission(ctx, tx, request); err != nil {
		return Record{}, false, err
	}
	result, err := loadRecord(ctx, tx, request.ActionID)
	if err != nil {
		return Record{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, false, err
	}
	return result, true, nil
}

// insertSubmission adds a new queued prompt and its journal row at the end of
// the delivery order.
func insertSubmission(ctx context.Context, tx *sql.Tx, request Submission) error {
	var sequence, attemptPosition int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),-1)+1 FROM queued_turns`).Scan(&sequence); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),-1)+1 FROM send_attempts`).Scan(&attemptPosition); err != nil {
		return err
	}
	if sequence < 0 || sequence == maximumSequence || attemptPosition < 0 || attemptPosition == maximumSequence {
		return errors.New("prompts: delivery sequence exhausted")
	}
	rank := originRank(request.Origin)
	reason := ""
	if request.NextTurn && request.Origin != SystemOrigin {
		reason = ReasonNextTurn
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO queued_turns(
		client_action_id,provider,hosted_thread_id,native_thread_id,text,presentation_text,text_hash,request_hash,state,origin,created_at,
		provider_turn_id,provider_terminal_status,provider_terminal_at,position,revision,delivery_rank,binding_json,binding_key,claim_token,claim_epoch)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, request.ActionID, request.Provider, request.HostedThreadID, request.NativeThreadID,
		request.ProviderText, request.PresentationText, request.TextHash, request.RequestHash, Queued, request.Origin, request.CreatedAt,
		"", "", "", sequence, 1, rank, "", "", "", 0); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO send_attempts(
		client_action_id,provider,native_thread_id,text_hash,status,run_id,reason,created_at,updated_at,confirmed,confirmed_at,
		provider_turn_id,provider_terminal_status,provider_terminal_at,canonical_message_id,identity_conflict,position,
		hosted_thread_id,revision,binding_json,claim_token)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, request.ActionID, request.Provider, request.NativeThreadID, request.RequestHash,
		JournalQueued, "", reason, request.CreatedAt, request.CreatedAt, 0, "", "", "", "", "", "", attemptPosition,
		request.HostedThreadID, 1, "", ""); err != nil {
		return err
	}
	return nil
}

func (s *Store) Claim(ctx context.Context, request ClaimRequest) (Record, bool, error) {
	if request.HostedThreadID == "" || request.Token == "" || request.AttachmentEpoch == 0 || !request.Ready {
		return Record{}, false, errors.New("prompts: claim requires a ready owner, token, and attachment epoch")
	}
	request.At = eventTime(request.At)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, false, err
	}
	defer tx.Rollback()
	var action, token string
	var epoch int64
	err = tx.QueryRowContext(ctx, `SELECT client_action_id,claim_token,claim_epoch FROM queued_turns WHERE hosted_thread_id=? AND claim_token<>'' AND state='delivering'`, request.HostedThreadID).Scan(&action, &token, &epoch)
	if err == nil {
		record, loadErr := loadRecord(ctx, tx, action)
		if loadErr != nil {
			return Record{}, false, loadErr
		}
		if token != request.Token || epoch != int64(request.AttachmentEpoch) {
			return record, false, ErrClaimHeld
		}
		return record, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, err
	}
	var revision int64
	runningTurn := ""
	if request.IntoRunningTurn {
		runningTurn = ` AND a.status='queued' AND queued_turns.origin<>'system' AND a.reason<>'` + ReasonNextTurn + `'`
	}
	err = tx.QueryRowContext(ctx, `SELECT client_action_id,revision FROM queued_turns
		WHERE hosted_thread_id=? AND state='queued' AND claim_token='' AND revision>0
		AND EXISTS(SELECT 1 FROM send_attempts a WHERE a.client_action_id=queued_turns.client_action_id
			AND a.hosted_thread_id=queued_turns.hosted_thread_id AND a.revision=queued_turns.revision`+runningTurn+`)
		ORDER BY delivery_rank,position LIMIT 1`, request.HostedThreadID).Scan(&action, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, tx.Commit()
	}
	if err != nil {
		return Record{}, false, err
	}
	newRevision := revision + 1
	result, err := tx.ExecContext(ctx, `UPDATE queued_turns SET state='delivering',claim_token=?,claim_epoch=?,revision=?
		WHERE client_action_id=? AND revision=? AND state='queued' AND claim_token=''`, request.Token, request.AttachmentEpoch, newRevision, action, revision)
	if err != nil {
		return Record{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Record{}, false, ErrClaimHeld
	}
	result, err = tx.ExecContext(ctx, `UPDATE send_attempts SET status='sending',updated_at=?,revision=?,claim_token=?
		WHERE client_action_id=? AND revision=?`, request.At, newRevision, request.Token, action, revision)
	if err != nil {
		return Record{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Record{}, false, errors.New("prompts: claim journal revision mismatch")
	}
	record, err := loadRecord(ctx, tx, action)
	if err != nil {
		return Record{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, false, err
	}
	return record, true, nil
}

func (s *Store) Accepted(ctx context.Context, accepted Acceptance) (Record, error) {
	if accepted.ActionID == "" || accepted.ClaimToken == "" || accepted.AttachmentEpoch == 0 {
		return Record{}, ErrClaimMismatch
	}
	accepted.At = eventTime(accepted.At)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback()
	record, attempt, err := loadAction(ctx, tx, accepted.ActionID)
	if err != nil {
		return Record{}, err
	}
	if !claimMatches(record, attempt, accepted.ClaimToken, accepted.AttachmentEpoch) {
		return Record{}, ErrClaimMismatch
	}
	binding, raw, key, turnID, err := mergeAndQualifyBinding(record.Outbox.Binding, accepted.Binding, record)
	if err != nil {
		return Record{}, err
	}
	state := record.Outbox.State
	if state != Confirmed && state != Refused && state != Parked && state != Expired {
		state = Accepted
	}
	newRevision := record.Revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE queued_turns SET state=?,native_thread_id=?,provider_turn_id=?,binding_json=?,binding_key=?,claim_token='',revision=? WHERE client_action_id=? AND revision=?`,
		state, *binding.Scope.NativeThreadID, turnID, raw, key, newRevision, accepted.ActionID, record.Revision); err != nil {
		return Record{}, mapBindingError(err)
	}
	journalState := JournalAccepted
	if attempt.Confirmed {
		journalState = JournalConfirmed
	}
	if _, err := tx.ExecContext(ctx, `UPDATE send_attempts SET native_thread_id=?,status=?,updated_at=?,provider_turn_id=?,binding_json=?,revision=?,claim_token=?,
		reason=CASE reason WHEN ? THEN '' ELSE reason END WHERE client_action_id=?`,
		*binding.Scope.NativeThreadID, journalState, accepted.At, turnID, raw, newRevision, accepted.ClaimToken, ReasonNextTurn, accepted.ActionID); err != nil {
		return Record{}, err
	}
	record, err = loadRecord(ctx, tx, accepted.ActionID)
	if err != nil {
		return Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (s *Store) Uncertain(ctx context.Context, uncertainty Uncertainty) (Record, error) {
	// open: no-answer send not hardened; see ledger decision 14
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback()
	record, attempt, err := loadAction(ctx, tx, uncertainty.ActionID)
	if err != nil {
		return Record{}, err
	}
	if !claimMatches(record, attempt, uncertainty.ClaimToken, uncertainty.AttachmentEpoch) {
		return Record{}, ErrClaimMismatch
	}
	if record.Outbox.State == Confirmed || record.Outbox.State == Refused || record.Outbox.State == Parked || record.Outbox.State == Expired {
		return record, tx.Commit()
	}
	newRevision := record.Revision + 1
	uncertainty.At = eventTime(uncertainty.At)
	// The dispatch claim is released, but its epoch stays, as on acceptance:
	// the provider may still answer, and its late receipt binds the prompt
	// through Accepted for this claim.
	if _, err := tx.ExecContext(ctx, `UPDATE queued_turns SET state='uncertain',claim_token='',revision=? WHERE client_action_id=?`, newRevision, uncertainty.ActionID); err != nil {
		return Record{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE send_attempts SET status='unknown',reason=?,updated_at=?,revision=? WHERE client_action_id=?`, uncertainty.Reason, uncertainty.At, newRevision, uncertainty.ActionID); err != nil {
		return Record{}, err
	}
	record, err = loadRecord(ctx, tx, uncertainty.ActionID)
	if err != nil {
		return Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, err
	}
	return record, nil
}

// Settle records that the provider's saved transcript holds the prompt's user
// row, proven by the caller through exact provider identity. Only an accepted
// or uncertain prompt with no dispatch claim settles. The prompt keeps its
// text; a settled prompt is terminal and Restore no longer returns it. It
// reports false, and changes nothing, for any other state or a concurrent
// change.
func (s *Store) Settle(ctx context.Context, actionID string) (Record, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, false, err
	}
	defer tx.Rollback()
	record, err := loadRecord(ctx, tx, actionID)
	if err != nil {
		return Record{}, false, err
	}
	if record.Outbox.State != Accepted && record.Outbox.State != Uncertain || record.ClaimToken != "" {
		return record, false, tx.Commit()
	}
	newRevision := record.Revision + 1
	// A Claude prompt's saved proof can come before its own durable member
	// completion, so confirming it records that completion as owed, whatever
	// its turn identity; only the member step clears it.
	result, err := tx.ExecContext(ctx, `UPDATE queued_turns SET state='confirmed',revision=?,member_owed=CASE WHEN provider='claude' THEN 1 ELSE member_owed END
		WHERE client_action_id=? AND revision=? AND claim_token=''`, newRevision, actionID, record.Revision)
	if err != nil {
		return Record{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return record, false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE send_attempts SET status='confirmed',confirmed=1,confirmed_at=?,revision=? WHERE client_action_id=?`, eventTime(""), newRevision, actionID); err != nil {
		return Record{}, false, err
	}
	record, err = loadRecord(ctx, tx, actionID)
	if err != nil {
		return Record{}, false, err
	}
	return record, true, tx.Commit()
}

// owedClause selects one hosted thread's prompts whose own member completion
// is still owed, in delivery order from a sequence, through the partial owed
// index: current work only.
const owedClause = ` WHERE q.hosted_thread_id=? AND q.member_owed=1 AND q.position>=? ORDER BY q.position LIMIT ?`

// Owed lists at most limit of one hosted thread's prompts that still owe their
// member completion, starting at sequence from (0 for the first).
func (s *Store) Owed(ctx context.Context, hostedThreadID string, from uint64, limit int) ([]Record, error) {
	if hostedThreadID == "" || limit <= 0 {
		return nil, errors.New("prompts: owed read requires an owner and a positive limit")
	}
	return s.records(ctx, owedClause, hostedThreadID, from, limit)
}

// OwedHosts names the hosted threads with prompts of one provider that owe
// their member completion in one native thread, directly or through a
// provisional handle resolved to it. It reads only owed rows.
func (s *Store) OwedHosts(ctx context.Context, provider model.Provider, nativeThreadID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT q.hosted_thread_id FROM queued_turns q INDEXED BY queued_turns_member_owed
		LEFT JOIN hosted_thread_native_ids n ON n.hosted_thread_id=q.hosted_thread_id AND n.provider=q.provider AND n.native_thread_id=q.native_thread_id
		WHERE q.member_owed=1 AND q.provider=? AND (q.native_thread_id=? OR n.resolved_native_id=?)`, string(provider), nativeThreadID, nativeThreadID)
	if err != nil {
		return nil, err
	}
	var hosts []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			rows.Close()
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, errors.Join(rows.Err(), rows.Close())
}

// ClearOwed records one prompt's member completion as done, only while the
// prompt is still the revision its completion was decided against.
func (s *Store) ClearOwed(ctx context.Context, actionID string, revision uint64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE queued_turns SET member_owed=0 WHERE client_action_id=? AND revision=? AND member_owed=1`, actionID, revision)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

// Owing reports which of the given prompts of one hosted thread still owe
// their member completion. A prompt not reported owes nothing: it was
// confirmed before the obligation existed, is not confirmed yet, or finished.
func (s *Store) Owing(ctx context.Context, hostedThreadID string, actionIDs []string) (map[string]bool, error) {
	owing := map[string]bool{}
	if hostedThreadID == "" || len(actionIDs) == 0 {
		return owing, nil
	}
	args := []any{hostedThreadID}
	for _, actionID := range actionIDs {
		args = append(args, actionID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT client_action_id FROM queued_turns WHERE hosted_thread_id=? AND member_owed=1 AND client_action_id IN (?`+strings.Repeat(",?", len(actionIDs)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var actionID string
		if err := rows.Scan(&actionID); err != nil {
			return nil, err
		}
		owing[actionID] = true
	}
	return owing, rows.Err()
}

// PruneEligibleBatch bounds one PruneEligible read.
const PruneEligibleBatch = 32

// PruneEligible reports which of the given prompts of one hosted thread are
// in a state from which they can never owe a member completion: a non-Claude
// prompt, a confirmed prompt that owes nothing, or an expired one. Settlement
// only confirms accepted or uncertain prompts, an obligation only clears, and
// nothing settles an expired prompt, so the answer stays true after it is
// read. A prompt with no row, or in any other state, is not reported.
func (s *Store) PruneEligible(ctx context.Context, hostedThreadID string, actionIDs []string) (map[string]bool, error) {
	eligible := map[string]bool{}
	if hostedThreadID == "" || len(actionIDs) == 0 {
		return eligible, nil
	}
	if len(actionIDs) > PruneEligibleBatch {
		return nil, fmt.Errorf("prompts: prune eligibility asks %d prompts, more than %d", len(actionIDs), PruneEligibleBatch)
	}
	args := []any{hostedThreadID}
	for _, actionID := range actionIDs {
		args = append(args, actionID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT client_action_id FROM queued_turns WHERE hosted_thread_id=? AND client_action_id IN (?`+strings.Repeat(",?", len(actionIDs)-1)+`)
		AND (provider<>'claude' OR state='confirmed' AND member_owed=0 OR state='expired')`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var actionID string
		if err := rows.Scan(&actionID); err != nil {
			return nil, err
		}
		eligible[actionID] = true
	}
	return eligible, rows.Err()
}

// eventClause selects one hosted thread's Claude prompts whose own binding
// names one native event id, through the event index.
const eventClause = ` WHERE q.hosted_thread_id=? AND q.provider='claude' AND q.binding_json<>'' AND json_extract(q.binding_json, '$.eventId')=? ORDER BY q.position LIMIT 2`

// ByEvent returns the Claude prompt whose own binding carries eventID. Two
// results mean the event is not one prompt's alone.
func (s *Store) ByEvent(ctx context.Context, hostedThreadID, eventID string) ([]Record, error) {
	if hostedThreadID == "" || eventID == "" {
		return nil, errors.New("prompts: event read requires an owner and an event")
	}
	return s.records(ctx, eventClause, hostedThreadID, eventID)
}

// Stale lists the delivered prompts submitted before cutoff that no
// settlement has resolved and no delivery holds: accepted or uncertain, with
// no dispatch claim. A record whose submission time cannot be read is left
// out. It reads the whole store, for operator maintenance only.
func (s *Store) Stale(ctx context.Context, cutoff time.Time) ([]Record, error) {
	records, err := s.records(ctx, ` WHERE q.state IN ('accepted','uncertain') AND q.claim_token='' ORDER BY q.position`)
	if err != nil {
		return nil, err
	}
	var stale []Record
	for _, record := range records {
		if submitted, err := time.Parse(time.RFC3339Nano, record.CreatedAt); err == nil && submitted.Before(cutoff) {
			stale = append(stale, record)
		}
	}
	return stale, nil
}

// Expire hides one stale prompt for good: an accepted, uncertain or parked
// prompt, with no dispatch claim, submitted before cutoff. It keeps the
// prompt's text and journal, and is terminal. It reports false, and changes
// nothing, for any other prompt or a concurrent change.
func (s *Store) Expire(ctx context.Context, actionID string, cutoff time.Time) (Record, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, false, err
	}
	defer tx.Rollback()
	record, expired, err := ExpireIn(ctx, tx, actionID, cutoff)
	if err != nil {
		return Record{}, false, err
	}
	return record, expired, tx.Commit()
}

// ExpireIn is Expire within the caller's transaction on the threads
// database, for a write that must commit with it or not at all.
func ExpireIn(ctx context.Context, tx *sql.Tx, actionID string, cutoff time.Time) (Record, bool, error) {
	record, err := loadRecord(ctx, tx, actionID)
	if err != nil {
		return Record{}, false, err
	}
	submitted, timeErr := time.Parse(time.RFC3339Nano, record.CreatedAt)
	unsettled := record.Outbox.State == Accepted || record.Outbox.State == Uncertain || record.Outbox.State == Parked
	if !unsettled || record.ClaimToken != "" || timeErr != nil || !submitted.Before(cutoff) {
		return record, false, nil
	}
	newRevision := record.Revision + 1
	result, err := tx.ExecContext(ctx, `UPDATE queued_turns SET state='expired',revision=? WHERE client_action_id=? AND revision=? AND claim_token=''`, newRevision, actionID, record.Revision)
	if err != nil {
		return Record{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return record, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE send_attempts SET revision=? WHERE client_action_id=?`, newRevision, actionID); err != nil {
		return Record{}, false, err
	}
	record, err = loadRecord(ctx, tx, actionID)
	if err != nil {
		return Record{}, false, err
	}
	return record, true, nil
}

// Refused records a definite provider answer while preserving the prompt text.
// Parked is reserved for a provider thread that cannot accept more input.
func (s *Store) Refused(ctx context.Context, refusal Refusal) (Record, error) {
	if refusal.ActionID == "" || refusal.ClaimToken == "" || refusal.AttachmentEpoch == 0 ||
		refusal.State != Refused && refusal.State != Parked {
		return Record{}, errors.New("prompts: refusal requires a claimed action and refused or parked state")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback()
	record, attempt, err := loadAction(ctx, tx, refusal.ActionID)
	if err != nil {
		return Record{}, err
	}
	if record.Outbox.State != Delivering || !claimMatches(record, attempt, refusal.ClaimToken, refusal.AttachmentEpoch) {
		return Record{}, ErrClaimMismatch
	}
	newRevision := record.Revision + 1
	refusal.At = eventTime(refusal.At)
	journalState := JournalRefused
	if refusal.State == Parked {
		journalState = JournalUnknown
	}
	result, err := tx.ExecContext(ctx, `UPDATE queued_turns SET state=?,claim_token='',claim_epoch=0,revision=?
		WHERE client_action_id=? AND state='delivering' AND revision=?`, refusal.State, newRevision, refusal.ActionID, record.Revision)
	if err != nil {
		return Record{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Record{}, ErrClaimMismatch
	}
	result, err = tx.ExecContext(ctx, `UPDATE send_attempts SET status=?,reason=?,updated_at=?,claim_token='',revision=?
		WHERE client_action_id=? AND revision=?`, journalState, refusal.Reason, refusal.At, newRevision, refusal.ActionID, attempt.Revision)
	if err != nil {
		return Record{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Record{}, ErrClaimMismatch
	}
	record, err = loadRecord(ctx, tx, refusal.ActionID)
	if err != nil {
		return Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, err
	}
	return record, nil
}

// Requeue releases a claim the provider definitely refused because the turn
// it targeted had ended. The same action waits again in its original place,
// now for the next turn, so it cannot join a different running turn; nothing
// reached the provider, so no second operation or resend exists.
func (s *Store) Requeue(ctx context.Context, requeue Requeue) (Record, error) {
	if requeue.ActionID == "" || requeue.ClaimToken == "" || requeue.AttachmentEpoch == 0 {
		return Record{}, errors.New("prompts: requeue requires a claimed action")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback()
	record, attempt, err := loadAction(ctx, tx, requeue.ActionID)
	if err != nil {
		return Record{}, err
	}
	if record.Outbox.State != Delivering || !claimMatches(record, attempt, requeue.ClaimToken, requeue.AttachmentEpoch) {
		return Record{}, ErrClaimMismatch
	}
	newRevision := record.Revision + 1
	result, err := tx.ExecContext(ctx, `UPDATE queued_turns SET state=?,claim_token='',claim_epoch=0,revision=?
		WHERE client_action_id=? AND state='delivering' AND revision=?`, Queued, newRevision, requeue.ActionID, record.Revision)
	if err != nil {
		return Record{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Record{}, ErrClaimMismatch
	}
	result, err = tx.ExecContext(ctx, `UPDATE send_attempts SET status=?,reason=?,updated_at=?,claim_token='',revision=?
		WHERE client_action_id=? AND revision=?`, JournalQueued, ReasonNextTurn, eventTime(requeue.At), newRevision, requeue.ActionID, attempt.Revision)
	if err != nil {
		return Record{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Record{}, ErrClaimMismatch
	}
	record, err = loadRecord(ctx, tx, requeue.ActionID)
	if err != nil {
		return Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, err
	}
	return record, nil
}

// ResendActionID is the action id of the prompt a Resend of actionID makes:
// derived, so the same Resend asked twice makes one prompt.
func ResendActionID(actionID string) string {
	sum := sha256.Sum256([]byte(actionID))
	return "resend-" + hex.EncodeToString(sum[:16])
}

// Resend retires an unsettled prompt (accepted, uncertain or parked, with no
// dispatch claim) and queues its exact text again as a new prompt, in one
// transaction. The caller decides from the record at revision that it may be
// resent: Nano's own recovery for an accepted prompt the provider is proven
// never to have taken (for the next turn), or the person's Resend. The retired
// prompt is hidden for good, keeping its text and journal; the new one, named
// by ResendActionID, is delivered, bound and saved as any prompt is, so the
// retired prompt's binding is never reused. It reports false, and changes
// nothing, for any other prompt or a concurrent change.
func (s *Store) Resend(ctx context.Context, actionID string, revision uint64, nextTurn bool) (retired, resent Record, ok bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, Record{}, false, err
	}
	defer tx.Rollback()
	record, err := loadRecord(ctx, tx, actionID)
	if err != nil {
		return Record{}, Record{}, false, err
	}
	unsettled := record.Outbox.State == Accepted || record.Outbox.State == Uncertain || record.Outbox.State == Parked
	if !unsettled || record.ClaimToken != "" || record.Revision != revision {
		return record, Record{}, false, tx.Commit()
	}
	newRevision := record.Revision + 1
	result, err := tx.ExecContext(ctx, `UPDATE queued_turns SET state='expired',revision=? WHERE client_action_id=? AND state=? AND claim_token='' AND revision=?`, newRevision, actionID, record.Outbox.State, record.Revision)
	if err != nil {
		return Record{}, Record{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return record, Record{}, false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE send_attempts SET revision=? WHERE client_action_id=?`, newRevision, actionID); err != nil {
		return Record{}, Record{}, false, err
	}
	request := Submission{
		HostedThreadID: record.Outbox.HostedThreadID, Provider: record.Provider, NativeThreadID: record.NativeThreadID,
		ActionID: ResendActionID(actionID), ProviderText: record.ProviderText, PresentationText: record.Outbox.PresentationText,
		TextHash: record.TextHash, RequestHash: record.RequestHash, Origin: record.Origin, NextTurn: nextTurn,
	}
	request.CreatedAt = eventTime("")
	if err := insertSubmission(ctx, tx, request); err != nil {
		return Record{}, Record{}, false, err
	}
	if retired, err = loadRecord(ctx, tx, actionID); err != nil {
		return Record{}, Record{}, false, err
	}
	if resent, err = loadRecord(ctx, tx, request.ActionID); err != nil {
		return Record{}, Record{}, false, err
	}
	return retired, resent, true, tx.Commit()
}

// HoldUntaken holds for the person an accepted prompt its provider is not
// known to have taken: one proven dropped by a turn the operator stopped
// (ReasonStopped; Stop means it is not sent again by itself), or one whose
// turn the saved transcript shows ended without it (ReasonTurnEnded). It
// becomes uncertain, which nothing resends, with reason in its journal, and
// waits for the person's Resend or Discard; a saved row still settles it. Its
// journal status stays accepted, so once settled or discarded it answers as
// any delivered prompt. It reports false, and changes nothing, for any other
// prompt or a concurrent change.
func (s *Store) HoldUntaken(ctx context.Context, actionID string, revision uint64, reason string) (Record, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, false, err
	}
	defer tx.Rollback()
	record, err := loadRecord(ctx, tx, actionID)
	if err != nil {
		return Record{}, false, err
	}
	if record.Outbox.State != Accepted || record.ClaimToken != "" || record.Revision != revision {
		return record, false, tx.Commit()
	}
	newRevision := record.Revision + 1
	result, err := tx.ExecContext(ctx, `UPDATE queued_turns SET state='uncertain',revision=? WHERE client_action_id=? AND state='accepted' AND claim_token='' AND revision=?`, newRevision, actionID, record.Revision)
	if err != nil {
		return Record{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return record, false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE send_attempts SET reason=?,updated_at=?,revision=? WHERE client_action_id=?`, reason, eventTime(""), newRevision, actionID); err != nil {
		return Record{}, false, err
	}
	if record, err = loadRecord(ctx, tx, actionID); err != nil {
		return Record{}, false, err
	}
	return record, true, tx.Commit()
}

// Cancel refuses only a prompt that has not been claimed for delivery.
func (s *Store) Cancel(ctx context.Context, actionID string) (Record, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback()
	record, err := CancelIn(ctx, tx, actionID)
	if err != nil {
		return Record{}, err
	}
	return record, tx.Commit()
}

// CancelIn is Cancel within the caller's transaction on the threads
// database, for a write that must commit with it or not at all.
func CancelIn(ctx context.Context, tx *sql.Tx, actionID string) (Record, error) {
	if actionID == "" {
		return Record{}, errors.New("prompts: action id is required")
	}
	record, attempt, err := loadAction(ctx, tx, actionID)
	if err != nil {
		return Record{}, err
	}
	if record.Outbox.State != Queued || record.ClaimToken != "" {
		return Record{}, errors.New("prompts: only an unclaimed queued prompt can be canceled")
	}
	newRevision := record.Revision + 1
	at := eventTime("")
	result, err := tx.ExecContext(ctx, `UPDATE queued_turns SET state='refused',revision=? WHERE client_action_id=? AND state='queued' AND claim_token='' AND revision=?`, newRevision, actionID, record.Revision)
	if err != nil {
		return Record{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Record{}, errors.New("prompts: queued prompt changed during cancellation")
	}
	result, err = tx.ExecContext(ctx, `UPDATE send_attempts SET status='refused',reason='canceled',updated_at=?,revision=? WHERE client_action_id=? AND revision=?`, at, newRevision, actionID, attempt.Revision)
	if err != nil {
		return Record{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Record{}, errors.New("prompts: prompt journal changed during cancellation")
	}
	return loadRecord(ctx, tx, actionID)
}

// CancelQueuedBatch atomically refuses a fixed set of unclaimed queued
// prompts. Parked prompts are already non-deliverable and are returned
// unchanged so callers can preserve the existing Discard behavior. Validation
// happens before the first update: a delivering, accepted, uncertain, missing,
// or duplicate action rejects the whole batch.
func (s *Store) CancelQueuedBatch(ctx context.Context, actionIDs []string) ([]Record, error) {
	if len(actionIDs) == 0 {
		return []Record{}, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	type candidate struct {
		record  Record
		attempt Attempt
	}
	candidates := make([]candidate, 0, len(actionIDs))
	seen := make(map[string]bool, len(actionIDs))
	for _, actionID := range actionIDs {
		if actionID == "" || seen[actionID] {
			return nil, errors.New("prompts: cancellation batch requires unique action ids")
		}
		seen[actionID] = true
		record, attempt, loadErr := loadAction(ctx, tx, actionID)
		if loadErr != nil {
			return nil, loadErr
		}
		if record.Outbox.State != Queued && record.Outbox.State != Parked {
			return nil, errors.New("prompts: only queued or parked prompts can be discarded")
		}
		if record.Outbox.State == Queued && record.ClaimToken != "" {
			return nil, errors.New("prompts: claimed queued prompt cannot be discarded")
		}
		candidates = append(candidates, candidate{record: record, attempt: attempt})
	}
	at := eventTime("")
	result := make([]Record, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.record.Outbox.State == Parked {
			result = append(result, candidate.record)
			continue
		}
		newRevision := candidate.record.Revision + 1
		updated, updateErr := tx.ExecContext(ctx, `UPDATE queued_turns SET state='refused',revision=? WHERE client_action_id=? AND state='queued' AND claim_token='' AND revision=?`, newRevision, candidate.record.Outbox.ActionID, candidate.record.Revision)
		if updateErr != nil {
			return nil, updateErr
		}
		if changed, _ := updated.RowsAffected(); changed != 1 {
			return nil, errors.New("prompts: queued prompt changed during batch cancellation")
		}
		updated, updateErr = tx.ExecContext(ctx, `UPDATE send_attempts SET status='refused',reason='canceled',updated_at=?,revision=? WHERE client_action_id=? AND revision=?`, at, newRevision, candidate.record.Outbox.ActionID, candidate.attempt.Revision)
		if updateErr != nil {
			return nil, updateErr
		}
		if changed, _ := updated.RowsAffected(); changed != 1 {
			return nil, errors.New("prompts: prompt journal changed during batch cancellation")
		}
		candidate.record.Outbox.State = Refused
		candidate.record.Revision = newRevision
		result = append(result, candidate.record)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) Restore(ctx context.Context, hostedThreadID string, currentEpoch uint64) (Restored, error) {
	if hostedThreadID == "" || currentEpoch == 0 {
		return Restored{}, errors.New("prompts: restore requires owner and current attachment epoch")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Restored{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT client_action_id,revision,state FROM queued_turns
		WHERE hosted_thread_id=? AND claim_token<>''
		AND (state IN ('accepted','uncertain') OR (state='delivering' AND claim_epoch<>?))`, hostedThreadID, currentEpoch)
	if err != nil {
		return Restored{}, err
	}
	type staleClaim struct {
		action   string
		revision uint64
		state    DeliveryState
	}
	var stale []staleClaim
	for rows.Next() {
		var claim staleClaim
		if err := rows.Scan(&claim.action, &claim.revision, &claim.state); err != nil {
			rows.Close()
			return Restored{}, err
		}
		stale = append(stale, claim)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return Restored{}, err
	}
	var restored Restored
	for _, claim := range stale {
		newRevision := claim.revision + 1
		if claim.state == Accepted || claim.state == Uncertain {
			result, err := tx.ExecContext(ctx, `UPDATE queued_turns SET claim_token='',claim_epoch=0,revision=? WHERE client_action_id=? AND revision=? AND state=?`, newRevision, claim.action, claim.revision, claim.state)
			if err != nil {
				return Restored{}, err
			}
			if changed, _ := result.RowsAffected(); changed != 1 {
				return Restored{}, fmt.Errorf("prompts: %s claim release revision mismatch", claim.state)
			}
			result, err = tx.ExecContext(ctx, `UPDATE send_attempts SET revision=? WHERE client_action_id=? AND revision=?`, newRevision, claim.action, claim.revision)
			if err != nil {
				return Restored{}, err
			}
			if changed, _ := result.RowsAffected(); changed != 1 {
				return Restored{}, fmt.Errorf("prompts: %s claim journal revision mismatch", claim.state)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE queued_turns SET state='uncertain',claim_token='',claim_epoch=0,revision=? WHERE client_action_id=?`, newRevision, claim.action); err != nil {
			return Restored{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE send_attempts SET status='unknown',reason='unknown_after_restart',revision=? WHERE client_action_id=?`, newRevision, claim.action); err != nil {
			return Restored{}, err
		}
		restored.Diagnostics = append(restored.Diagnostics, model.Diagnostic{Code: "stale_claim_uncertain", Severity: "warning", Message: "a prior attachment claim is uncertain and will not retry automatically"})
	}
	if restored.Outbox, err = listUnsettled(ctx, tx, hostedThreadID); err != nil {
		return Restored{}, err
	}
	attemptRows, err := tx.QueryContext(ctx, attemptSelect+` WHERE hosted_thread_id=? AND NOT EXISTS (
		SELECT 1 FROM queued_turns q WHERE q.client_action_id=send_attempts.client_action_id AND q.state IN ('confirmed','expired')) ORDER BY position`, hostedThreadID)
	if err != nil {
		return Restored{}, err
	}
	for attemptRows.Next() {
		attempt, err := scanAttempt(attemptRows)
		if err != nil {
			attemptRows.Close()
			return Restored{}, err
		}
		restored.Attempts = append(restored.Attempts, attempt)
	}
	if err := errors.Join(attemptRows.Err(), attemptRows.Close()); err != nil {
		return Restored{}, err
	}
	if err := tx.Commit(); err != nil {
		return Restored{}, err
	}
	return restored, nil
}

// TailCandidates lists the prompts whose saved rows a tail page starting at
// since may hold: every unsettled prompt, and every prompt that settled or
// expired at or after since (the provider writes a prompt's row before Nano
// settles it, on the same clock). It may list more; the caller matches each
// one's exact key against the page's rows. A prompt whose binding cannot be
// read has no exact key and is left out, as the live tick leaves out a turn
// whose claims cannot be read.
func (s *Store) TailCandidates(ctx context.Context, hostedThreadID string, since time.Time) ([]OutboxEntry, error) {
	bound := since.Add(-time.Second).UTC().Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, recordSelect+` WHERE q.hosted_thread_id=? AND (q.state NOT IN ('confirmed','expired')
		OR q.state='confirmed' AND a.confirmed_at>=? OR q.state='expired' AND a.updated_at>=?) ORDER BY q.position`, hostedThreadID, bound, bound)
	if err != nil {
		return nil, err
	}
	var outbox []OutboxEntry
	for rows.Next() {
		record, err := scanRecord(rows)
		if errors.Is(err, errUnreadableBinding) {
			continue
		}
		if err != nil {
			rows.Close()
			return nil, err
		}
		outbox = append(outbox, record.Outbox)
	}
	return outbox, errors.Join(rows.Err(), rows.Close())
}

// Unsettled lists, in submission order, the thread's prompts that publication
// shows: every prompt the provider's saved transcript has not settled,
// however old, unless it expired.
func (s *Store) Unsettled(ctx context.Context, hostedThreadID string) ([]OutboxEntry, error) {
	return listUnsettled(ctx, s.db, hostedThreadID)
}

func listUnsettled(ctx context.Context, db rowsQueryer, hostedThreadID string) ([]OutboxEntry, error) {
	rows, err := db.QueryContext(ctx, recordSelect+` WHERE q.hosted_thread_id=? AND q.state NOT IN ('confirmed','expired') ORDER BY q.position`, hostedThreadID)
	if err != nil {
		return nil, err
	}
	var outbox []OutboxEntry
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		outbox = append(outbox, record.Outbox)
	}
	return outbox, errors.Join(rows.Err(), rows.Close())
}

const recordSelect = `SELECT q.client_action_id,q.provider,q.hosted_thread_id,q.native_thread_id,q.text,q.presentation_text,q.text_hash,q.request_hash,q.state,q.origin,q.created_at,q.position,q.revision,q.delivery_rank,q.binding_json,q.claim_token,q.claim_epoch,q.provider_turn_id,q.provider_terminal_status,q.provider_terminal_at,COALESCE(a.reason,''),COALESCE(a.updated_at,'') FROM queued_turns q LEFT JOIN send_attempts a ON a.client_action_id=q.client_action_id`

func loadRecord(ctx context.Context, tx *sql.Tx, actionID string) (Record, error) {
	return scanRecord(tx.QueryRowContext(ctx, recordSelect+` WHERE q.client_action_id=?`, actionID))
}

type scanner interface{ Scan(...any) error }

func scanRecord(row scanner) (Record, error) {
	var result Record
	var provider, state, origin, bindingJSON string
	var sequence, revision, epoch int64
	err := row.Scan(&result.Outbox.ActionID, &provider, &result.Outbox.HostedThreadID, &result.NativeThreadID, &result.ProviderText,
		&result.Outbox.PresentationText, &result.TextHash, &result.RequestHash, &state, &origin, &result.CreatedAt,
		&sequence, &revision, &result.DeliveryRank, &bindingJSON, &result.ClaimToken, &epoch,
		&result.ProviderTurnID, &result.ProviderTerminal, &result.ProviderTerminalAt, &result.Outbox.Reason, &result.Outbox.UpdatedAt)
	if err != nil {
		return Record{}, err
	}
	if sequence < 0 || sequence == maximumSequence || revision < 0 || epoch < 0 {
		return Record{}, errors.New("prompts: invalid persisted queue counters")
	}
	result.Provider, result.Outbox.State, result.Origin = model.Provider(provider), DeliveryState(state), Origin(origin)
	result.Outbox.Sequence, result.Revision, result.ClaimEpoch = uint64(sequence)+1, uint64(revision), uint64(epoch)
	if bindingJSON != "" {
		var binding Binding
		if err := json.Unmarshal([]byte(bindingJSON), &binding); err != nil {
			return Record{}, fmt.Errorf("%w: %v", errUnreadableBinding, err)
		}
		result.Outbox.Binding = &binding
	}
	return result, nil
}

const attemptSelect = `SELECT hosted_thread_id,client_action_id,provider,native_thread_id,text_hash,status,reason,run_id,created_at,updated_at,revision,binding_json,claim_token,confirmed,confirmed_at,provider_turn_id,provider_terminal_status,provider_terminal_at,canonical_message_id,identity_conflict FROM send_attempts`

const threadAttemptSelect = `SELECT a.hosted_thread_id,a.client_action_id,a.provider,a.native_thread_id,a.text_hash,a.status,a.reason,a.run_id,a.created_at,a.updated_at,a.revision,a.binding_json,a.claim_token,a.confirmed,a.confirmed_at,a.provider_turn_id,a.provider_terminal_status,a.provider_terminal_at,a.canonical_message_id,a.identity_conflict,COALESCE(q.state,'')
	FROM send_attempts a LEFT JOIN queued_turns q ON q.client_action_id=a.client_action_id
	WHERE a.provider=? AND a.native_thread_id=? ORDER BY a.position`

func loadAttempt(ctx context.Context, tx *sql.Tx, actionID string) (Attempt, error) {
	return scanAttempt(tx.QueryRowContext(ctx, attemptSelect+` WHERE client_action_id=?`, actionID))
}

func scanAttempt(row scanner) (Attempt, error) {
	var values attemptScan
	if err := row.Scan(values.targets()...); err != nil {
		return Attempt{}, err
	}
	return values.decode()
}

func scanAttemptStatus(row scanner) (AttemptStatus, error) {
	var values attemptScan
	var outboxState string
	targets := append(values.targets(), &outboxState)
	if err := row.Scan(targets...); err != nil {
		return AttemptStatus{}, err
	}
	attempt, err := values.decode()
	if err != nil {
		return AttemptStatus{}, err
	}
	return AttemptStatus{Attempt: attempt, OutboxState: DeliveryState(outboxState)}, nil
}

type attemptScan struct {
	attempt                      Attempt
	provider, state, bindingJSON string
	revision                     int64
	confirmed                    int
}

func (s *attemptScan) targets() []any {
	return []any{
		&s.attempt.HostedThreadID, &s.attempt.ActionID, &s.provider, &s.attempt.NativeThreadID, &s.attempt.TextHash, &s.state,
		&s.attempt.Reason, &s.attempt.RunID, &s.attempt.CreatedAt, &s.attempt.UpdatedAt, &s.revision, &s.bindingJSON,
		&s.attempt.ClaimToken, &s.confirmed, &s.attempt.ConfirmedAt, &s.attempt.ProviderTurnID, &s.attempt.ProviderTerminal,
		&s.attempt.ProviderTerminalAt, &s.attempt.CanonicalItemID, &s.attempt.IdentityConflict,
	}
}

func (s *attemptScan) decode() (Attempt, error) {
	result := s.attempt
	if s.revision < 0 {
		return Attempt{}, errors.New("prompts: invalid persisted attempt revision")
	}
	result.Provider, result.State, result.Revision, result.Confirmed = model.Provider(s.provider), JournalState(s.state), uint64(s.revision), s.confirmed != 0
	if s.bindingJSON != "" {
		var binding Binding
		if err := json.Unmarshal([]byte(s.bindingJSON), &binding); err != nil {
			return Attempt{}, err
		}
		result.Binding = &binding
	}
	return result, nil
}

func loadAction(ctx context.Context, tx *sql.Tx, actionID string) (Record, Attempt, error) {
	record, err := loadRecord(ctx, tx, actionID)
	if err != nil {
		return Record{}, Attempt{}, err
	}
	attempt, err := loadAttempt(ctx, tx, actionID)
	return record, attempt, err
}

func sameAdmission(record Record, request Submission) bool {
	textMatches := record.ProviderText == request.ProviderText && record.Outbox.PresentationText == request.PresentationText
	if record.Outbox.State == Confirmed && record.ProviderText == "" && record.Outbox.PresentationText == "" {
		textMatches = true
	}
	return record.Outbox.HostedThreadID == request.HostedThreadID && record.Provider == request.Provider &&
		record.NativeThreadID == request.NativeThreadID && textMatches && record.TextHash == request.TextHash &&
		record.RequestHash == request.RequestHash && record.Origin == request.Origin
}

// NormalizeOrigin defaults an unrecognized origin to a human submission.
func NormalizeOrigin(origin Origin) Origin {
	switch origin {
	case AgentOrigin, SystemOrigin:
		return origin
	default:
		return HumanOrigin
	}
}

func originRank(origin Origin) int {
	switch origin {
	case AgentOrigin:
		return 1
	case SystemOrigin:
		return 2
	default:
		return 0
	}
}

func claimMatches(record Record, attempt Attempt, token string, epoch uint64) bool {
	return token != "" && epoch != 0 && record.ClaimEpoch == epoch &&
		(record.ClaimToken == token || record.ClaimToken == "" && attempt.ClaimToken == token)
}

func mergeAndQualifyBinding(prior *Binding, next Binding, record Record) (Binding, string, string, string, error) {
	if prior != nil {
		if !compatibleBindings(*prior, next) {
			return Binding{}, "", "", "", ErrBindingConflict
		}
		next = mergeBindings(*prior, next)
	}
	raw, key, turnID, err := qualifyBinding(record.Outbox.HostedThreadID, next)
	if err != nil {
		return Binding{}, "", "", "", err
	}
	firstAcceptance := record.Outbox.State == Delivering && prior == nil
	// Claude can reveal its durable native session id only when it accepts the
	// first prompt, so that first exact binding may replace the provisional id.
	if next.Scope.Provider != record.Provider || record.NativeThreadID != "" && *next.Scope.NativeThreadID != record.NativeThreadID && !firstAcceptance {
		return Binding{}, "", "", "", ErrBindingConflict
	}
	return next, raw, key, turnID, nil
}

func qualifyBinding(hostedThreadID string, binding Binding) (string, string, string, error) {
	if hostedThreadID == "" || binding.Scope.Provider == "" || binding.Scope.NativeThreadID == nil || *binding.Scope.NativeThreadID == "" {
		return "", "", "", errors.New("prompts: binding lacks exact owner or native thread")
	}
	parent := stringValue(binding.Scope.NativeParentCallID)
	var qualifier []any
	turnID := ""
	if binding.Scope.NativeTurnID != nil && *binding.Scope.NativeTurnID != "" {
		turnID = *binding.Scope.NativeTurnID
		qualifier = []any{"native", turnID}
	} else {
		turnID = binding.Scope.TurnKey
		if turnID == "" || binding.Scope.Source == "" || binding.Scope.Incarnation == "" {
			return "", "", "", errors.New("prompts: local turn binding is not source-qualified")
		}
		qualifier = []any{"local", binding.Scope.Source, binding.Scope.Incarnation, binding.Scope.ConnectionID, turnID}
	}
	if binding.EventID != "" {
		// Prompts steered into one running turn share it; each keeps its own
		// native event, so only a repeat of the same event conflicts.
		qualifier = append(qualifier, "event", binding.EventID)
	}
	tuple := append([]any{hostedThreadID, binding.Scope.Provider, *binding.Scope.NativeThreadID, parent}, qualifier...)
	keyBytes, _ := json.Marshal(tuple)
	rawBytes, err := json.Marshal(binding)
	if err != nil {
		return "", "", "", err
	}
	return string(rawBytes), string(keyBytes), turnID, nil
}

func compatibleBindings(a, b Binding) bool {
	return (a.Scope.Provider == "" || b.Scope.Provider == "" || a.Scope.Provider == b.Scope.Provider) && compatibleString(a.Scope.NativeThreadID, b.Scope.NativeThreadID) &&
		compatibleString(a.Scope.NativeTurnID, b.Scope.NativeTurnID) && compatibleString(a.Scope.NativeParentCallID, b.Scope.NativeParentCallID) &&
		(a.Scope.TurnKey == "" || b.Scope.TurnKey == "" || a.Scope.TurnKey == b.Scope.TurnKey) &&
		(a.Scope.Source == "" || b.Scope.Source == "" || a.Scope.Source == b.Scope.Source) &&
		(a.Scope.Incarnation == "" || b.Scope.Incarnation == "" || a.Scope.Incarnation == b.Scope.Incarnation) &&
		(a.Scope.ConnectionID == "" || b.Scope.ConnectionID == "" || a.Scope.ConnectionID == b.Scope.ConnectionID) &&
		(a.NativeItemID == "" || b.NativeItemID == "" || a.NativeItemID == b.NativeItemID) &&
		(a.EventID == "" || b.EventID == "" || a.EventID == b.EventID)
}

func mergeBindings(prior, next Binding) Binding {
	if next.Scope.NativeThreadID == nil {
		next.Scope.NativeThreadID = prior.Scope.NativeThreadID
	}
	if next.Scope.NativeTurnID == nil {
		next.Scope.NativeTurnID = prior.Scope.NativeTurnID
	}
	if next.Scope.NativeParentCallID == nil {
		next.Scope.NativeParentCallID = prior.Scope.NativeParentCallID
	}
	if next.Scope.Provider == "" {
		next.Scope.Provider = prior.Scope.Provider
	}
	if next.Scope.TurnKey == "" {
		next.Scope.TurnKey = prior.Scope.TurnKey
	}
	if next.Scope.Source == "" {
		next.Scope.Source = prior.Scope.Source
	}
	if next.Scope.Incarnation == "" {
		next.Scope.Incarnation = prior.Scope.Incarnation
	}
	if next.Scope.ConnectionID == "" {
		next.Scope.ConnectionID = prior.Scope.ConnectionID
	}
	if next.NativeItemID == "" {
		next.NativeItemID = prior.NativeItemID
	}
	if next.EventID == "" {
		next.EventID = prior.EventID
	}
	return next
}

func compatibleString(a, b *string) bool { return a == nil || b == nil || *a == *b }

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func firstValue(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func mapBindingError(err error) error {
	if err != nil && (strings.Contains(err.Error(), "queued_turns_binding_claim") || strings.Contains(err.Error(), "completed_turns_binding") || strings.Contains(err.Error(), ".binding_key")) {
		return ErrBindingConflict
	}
	return err
}

func terminalOutcome(outcome model.Outcome) bool {
	switch outcome {
	case model.OutcomeCompleted, model.OutcomeFailed, model.OutcomeInterrupted, model.OutcomeTruncated, model.OutcomeDeclined, model.OutcomeCancelled, model.OutcomeUnknown:
		return true
	default:
		return false
	}
}

func eventTime(value string) string {
	if value != "" {
		return value
	}
	return time.Now().UTC().Format(time.RFC3339Nano)
}

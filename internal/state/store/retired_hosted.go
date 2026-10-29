package statestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"reflect"
	"strings"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/threads"
)

type RetiredHostedRecord struct {
	HostedThreadID      string `json:"hostedThreadId"`
	Provider            string `json:"provider"`
	NativeThreadID      string `json:"nativeThreadId"`
	OwnerHostedThreadID string `json:"ownerHostedThreadId"`
}
type RetirementRow struct {
	RowID  int64          `json:"rowId"`
	Values map[string]any `json:"values"`
}
type RetirementReference struct {
	Table       string        `json:"table"`
	Column      string        `json:"column"`
	Row         RetirementRow `json:"row"`
	From        string        `json:"from"`
	To          string        `json:"to"`
	Disposition string        `json:"disposition"`
}
type RetirementCounts struct {
	Records    int `json:"records"`
	Claims     int `json:"claims"`
	Dependents int `json:"dependents"`
	Lineage    int `json:"lineage"`
	Outbox     int `json:"outbox"`
}
type RetirementPreview struct {
	Records     []RetiredHostedRecord      `json:"records"`
	DeleteRows  map[string][]RetirementRow `json:"deleteRows"`
	Lineage     []RetirementReference      `json:"lineage"`
	Outbox      []RetirementReference      `json:"outbox"`
	Counts      RetirementCounts           `json:"counts"`
	Fingerprint string                     `json:"fingerprint"`
}
type RetirementRequest struct {
	Apply               bool              `json:"apply"`
	OutboxDisposition   string            `json:"outboxDisposition"`
	ExpectedFingerprint string            `json:"expectedFingerprint,omitempty"`
	ExpectedCounts      *RetirementCounts `json:"expectedCounts,omitempty"`
}
type RetirementReceipt struct {
	Applied      bool              `json:"applied"`
	Preview      RetirementPreview `json:"preview"`
	Before       map[string]int    `json:"before"`
	After        map[string]int    `json:"after,omitempty"`
	BackupPath   string            `json:"backupPath,omitempty"`
	BackupSHA256 string            `json:"backupSha256,omitempty"`
}

// RetireHostedRecords owns one preview/apply transaction over the server's
// serialized writer connection. The caller holds the execution and outbox locks,
// checks runtime exclusions, and supplies identity-resolver verification. No
// cache may change until this function reports a successful commit.
func RetireHostedRecords(ctx context.Context, db *sql.DB, request RetirementRequest, backupPath string, check func(RetirementPreview) error, verify func(*sql.Tx, []RetiredHostedRecord) error) (receipt RetirementReceipt, err error) {
	if db == nil || db.Stats().MaxOpenConnections != 1 {
		return receipt, errors.New("threads: retirement requires the serialized store writer")
	}
	if request.OutboxDisposition != "repoint" && request.OutboxDisposition != "delete" {
		return receipt, errors.New("threads: outboxDisposition must be repoint or delete")
	}
	if request.Apply && (request.ExpectedFingerprint == "" || backupPath == "" || verify == nil) {
		return receipt, errors.New("threads: apply requires the preview fingerprint, backup destination and identity verification")
	}
	connection, err := db.Conn(ctx)
	if err != nil {
		return receipt, err
	}
	defer connection.Close()
	var version int
	if err = connection.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return receipt, err
	}
	if version != turns.TurnSchemaVersion {
		return receipt, fmt.Errorf("threads: retirement requires canonical schema %d", turns.TurnSchemaVersion)
	}
	tx, err := connection.BeginTx(ctx, nil)
	if err != nil {
		return receipt, err
	}
	before, err := previewRetirement(ctx, tx, request.OutboxDisposition)
	rollbackErr := tx.Rollback()
	receipt.Preview, receipt.Before = before.preview, before.counts
	if err != nil {
		return receipt, err
	}
	if rollbackErr != nil {
		return receipt, rollbackErr
	}
	if err = guardRetirement(request, before.preview); err != nil {
		return receipt, err
	}
	if check != nil {
		if err = check(before.preview); err != nil {
			return receipt, err
		}
	}
	if !request.Apply {
		return receipt, nil
	}
	if err = threads.BackupConnection(ctx, connection, backupPath); err != nil {
		return receipt, fmt.Errorf("threads: retirement backup: %w", err)
	}
	receipt.BackupPath = backupPath
	file, err := os.Open(backupPath)
	if err != nil {
		return receipt, err
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	err = errors.Join(copyErr, file.Close())
	if err != nil {
		return receipt, err
	}
	receipt.BackupSHA256 = hex.EncodeToString(digest.Sum(nil))
	tx, err = connection.BeginTx(ctx, nil)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	// The server writer has stayed reserved. A second snapshot also catches a
	// foreign writer that changed the approved set during the online backup.
	before, err = previewRetirement(ctx, tx, request.OutboxDisposition)
	if err != nil {
		return receipt, err
	}
	receipt.Preview, receipt.Before = before.preview, before.counts
	if err = guardRetirement(request, before.preview); err != nil {
		return receipt, err
	}
	if check != nil {
		if err = check(before.preview); err != nil {
			return receipt, err
		}
	}
	if _, err = tx.ExecContext(ctx, `PRAGMA defer_foreign_keys=ON`); err != nil {
		return receipt, err
	}
	for _, ref := range append(append([]RetirementReference{}, before.preview.Lineage...), before.preview.Outbox...) {
		var result sql.Result
		if ref.Disposition == "delete" {
			result, err = tx.ExecContext(ctx, `DELETE FROM `+quoteRetirementSQL(ref.Table)+` WHERE rowid=?`, ref.Row.RowID)
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE `+quoteRetirementSQL(ref.Table)+` SET `+quoteRetirementSQL(ref.Column)+`=? WHERE rowid=? AND `+quoteRetirementSQL(ref.Column)+`=?`, ref.To, ref.Row.RowID, ref.From)
		}
		if err = retirementAffected(result, err); err != nil {
			return receipt, err
		}
	}
	for _, table := range retirementDeleteTables {
		for _, row := range before.preview.DeleteRows[table] {
			result, execErr := tx.ExecContext(ctx, `DELETE FROM `+quoteRetirementSQL(table)+` WHERE rowid=?`, row.RowID)
			if err = retirementAffected(result, execErr); err != nil {
				return receipt, err
			}
		}
	}
	after, err := scanRetirement(ctx, tx, nil, request.OutboxDisposition)
	if err != nil {
		return receipt, err
	}
	if !reflect.DeepEqual(before.expected, after.actual) {
		return receipt, errors.New("threads: retirement changed rows outside the exact preview")
	}
	violations, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return receipt, err
	}
	hasViolation := violations.Next()
	err = errors.Join(violations.Err(), violations.Close())
	if err != nil {
		return receipt, err
	}
	if hasViolation {
		return receipt, errors.New("threads: retirement left a foreign-key violation")
	}
	if err = verify(tx, before.preview.Records); err != nil {
		return receipt, fmt.Errorf("threads: retirement resolver proof: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return receipt, err
	}
	receipt.After = after.counts
	receipt.Applied = true
	return receipt, nil
}

var retirementDeleteTables = []string{"thread_metadata_current", "thread_metadata_events", "hosted_turn_operations", "hosted_attachment_generations", "hosted_execution_sessions", "subscription_completed_cursors", "hosted_thread_native_ids", "hosted_threads"}

func retirementAffected(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("threads: retirement affected %d rows, expected one", n)
	}
	return nil
}
func guardRetirement(request RetirementRequest, preview RetirementPreview) error {
	if request.ExpectedFingerprint != "" && request.ExpectedFingerprint != preview.Fingerprint {
		return errors.New("threads: retirement preview fingerprint changed")
	}
	if request.ExpectedCounts != nil && *request.ExpectedCounts != preview.Counts {
		return errors.New("threads: retirement expected counts changed")
	}
	return nil
}

type retirementScan struct {
	preview          RetirementPreview
	counts           map[string]int
	actual, expected map[string]string
}

func previewRetirement(ctx context.Context, tx *sql.Tx, disposition string) (retirementScan, error) {
	rows, err := tx.QueryContext(ctx, `SELECT h.hosted_thread_id,h.provider,h.current_native_id,n.hosted_thread_id
 FROM hosted_threads h JOIN hosted_thread_native_ids r ON r.hosted_thread_id=h.hosted_thread_id AND r.provider=h.provider AND r.native_thread_id=h.current_native_id
 JOIN hosted_thread_native_ids n ON n.provider=r.provider AND n.native_thread_id=r.native_thread_id AND n.retired_at IS NULL AND n.hosted_thread_id<>h.hosted_thread_id
 WHERE r.retired_at IS NOT NULL ORDER BY h.hosted_thread_id`)
	if err != nil {
		return retirementScan{}, err
	}
	targets := []RetiredHostedRecord{}
	for rows.Next() {
		var target RetiredHostedRecord
		if err := rows.Scan(&target.HostedThreadID, &target.Provider, &target.NativeThreadID, &target.OwnerHostedThreadID); err != nil {
			rows.Close()
			return retirementScan{}, err
		}
		targets = append(targets, target)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return retirementScan{}, err
	}
	seen := map[string]bool{}
	for _, target := range targets {
		if seen[target.HostedThreadID] {
			return retirementScan{}, errors.New("threads: retired record has ambiguous surviving owners")
		}
		seen[target.HostedThreadID] = true
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM hosted_thread_native_ids WHERE hosted_thread_id=? AND retired_at IS NULL`, target.HostedThreadID).Scan(&active); err != nil {
			return retirementScan{}, err
		}
		if active != 0 {
			return retirementScan{}, fmt.Errorf("threads: retired record %s still has an unretired claim", target.HostedThreadID)
		}
		var valid int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM hosted_threads h JOIN hosted_thread_native_ids n ON n.hosted_thread_id=h.hosted_thread_id AND n.provider=h.provider AND n.native_thread_id=h.current_native_id WHERE h.hosted_thread_id=? AND h.provider=? AND h.current_native_id=? AND n.retired_at IS NULL AND n.provisional=0`, target.OwnerHostedThreadID, target.Provider, target.NativeThreadID).Scan(&valid); err != nil {
			return retirementScan{}, err
		}
		if valid != 1 {
			return retirementScan{}, errors.New("threads: surviving owner does not own the same current native conversation")
		}
	}
	scan, err := scanRetirement(ctx, tx, targets, disposition)
	if err != nil {
		return scan, err
	}
	scan.preview.Counts = RetirementCounts{Records: len(targets), Claims: len(scan.preview.DeleteRows["hosted_thread_native_ids"]), Lineage: len(scan.preview.Lineage), Outbox: len(scan.preview.Outbox)}
	for _, table := range retirementDeleteTables[:6] {
		scan.preview.Counts.Dependents += len(scan.preview.DeleteRows[table])
	}
	encoded, err := json.Marshal(scan.preview)
	if err != nil {
		return scan, err
	}
	sum := sha256.Sum256(encoded)
	scan.preview.Fingerprint = hex.EncodeToString(sum[:])
	return scan, nil
}

// Scan all table values, including columns without foreign keys. Unknown exact
// hosted references refuse the operation instead of silently becoming orphans.
// Hashing the expected post-operation rows also detects trigger/cascade changes
// to unrelated values, even when table counts would still match.
func scanRetirement(ctx context.Context, tx *sql.Tx, targets []RetiredHostedRecord, disposition string) (retirementScan, error) {
	scan := retirementScan{preview: RetirementPreview{Records: targets, DeleteRows: map[string][]RetirementRow{}, Lineage: []RetirementReference{}, Outbox: []RetirementReference{}}, counts: map[string]int{}, actual: map[string]string{}, expected: map[string]string{}}
	replacements := map[string]RetiredHostedRecord{}
	for _, target := range targets {
		replacements[target.HostedThreadID] = target
	}
	allowed := map[string]bool{}
	for _, table := range retirementDeleteTables {
		allowed[table] = true
		scan.preview.DeleteRows[table] = []RetirementRow{}
	}
	tables, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name`)
	if err != nil {
		return scan, err
	}
	names := []string{}
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			tables.Close()
			return scan, err
		}
		names = append(names, name)
	}
	err = errors.Join(tables.Err(), tables.Close())
	if err != nil {
		return scan, err
	}
	for _, table := range names {
		actual, expected := sha256.New(), sha256.New()
		scan.counts[table] = 0
		rows, err := tx.QueryContext(ctx, `SELECT rowid,* FROM `+quoteRetirementSQL(table)+` ORDER BY rowid`)
		if err != nil {
			return scan, err
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			return scan, err
		}
		for rows.Next() {
			values := make([]any, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return scan, err
			}
			row := RetirementRow{RowID: values[0].(int64), Values: map[string]any{}}
			for i, column := range columns[1:] {
				row.Values[column] = values[i+1]
			}
			scan.counts[table]++
			if err := hashRetirementRow(actual, row); err != nil {
				rows.Close()
				return scan, err
			}
			owned, _ := row.Values["hosted_thread_id"].(string)
			_, retired := replacements[owned]
			deleted := retired && allowed[table]
			if deleted {
				scan.preview.DeleteRows[table] = append(scan.preview.DeleteRows[table], row)
			}
			updated := RetirementRow{RowID: row.RowID, Values: map[string]any{}}
			for column, value := range row.Values {
				updated.Values[column] = value
			}
			// Iterate schema order, never Go map order: preview fingerprints are stable.
			for _, column := range columns[1:] {
				value, _ := row.Values[column].(string)
				target, found := replacements[value]
				if !found {
					continue
				}
				lineage := table == "hosted_threads" && column == "invoker_hosted_id" || table == "hosted_turn_operations" && column == "sender_hosted_id"
				if deleted && !lineage {
					continue
				}
				ref := RetirementReference{Table: table, Column: column, Row: row, From: value, To: target.OwnerHostedThreadID, Disposition: "repoint"}
				switch {
				case table == "hosted_threads" && column == "invoker_hosted_id", table == "hosted_turn_operations" && column == "sender_hosted_id":
					nativeColumn, providerColumn := "invoker_native_id", "invoker_provider"
					if table == "hosted_turn_operations" {
						nativeColumn, providerColumn = "sender_native_id", "sender_provider"
					}
					native, _ := row.Values[nativeColumn].(string)
					provider, _ := row.Values[providerColumn].(string)
					if (native != "" && native != target.NativeThreadID) || (provider != "" && provider != target.Provider) {
						rows.Close()
						return scan, fmt.Errorf("threads: %s row %d has conflicting lineage identity", table, row.RowID)
					}
					scan.preview.Lineage = append(scan.preview.Lineage, ref)
					updated.Values[column] = ref.To
				case table == "queued_turns" && column == "hosted_thread_id":
					native, _ := row.Values["native_thread_id"].(string)
					provider, _ := row.Values["provider"].(string)
					if native != target.NativeThreadID || provider != target.Provider {
						rows.Close()
						return scan, errors.New("threads: outbox does not name the same native conversation")
					}
					ref.Disposition = disposition
					scan.preview.Outbox = append(scan.preview.Outbox, ref)
					if disposition == "delete" {
						deleted = true
					} else {
						updated.Values[column] = ref.To
					}
				default:
					rows.Close()
					return scan, fmt.Errorf("threads: unexpected retired hosted reference in %s.%s row %d", table, column, row.RowID)
				}
			}
			if !deleted {
				if err := hashRetirementRow(expected, updated); err != nil {
					rows.Close()
					return scan, err
				}
			}
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return scan, err
		}
		scan.actual[table] = hex.EncodeToString(actual.Sum(nil))
		scan.expected[table] = hex.EncodeToString(expected.Sum(nil))
	}
	return scan, nil
}
func hashRetirementRow(digest hash.Hash, row RetirementRow) error {
	return json.NewEncoder(digest).Encode(row)
}
func quoteRetirementSQL(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

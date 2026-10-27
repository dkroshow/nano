package threads

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

var creationNameRepairSources = map[string]bool{
	"creation_request": true,
}

type CreationNameRepairRecord struct {
	HostedThreadID string `json:"hostedThreadId"`
	Provider       string `json:"provider"`
	NativeThreadID string `json:"nativeThreadId,omitempty"`
	Name           string `json:"name"`
	Source         string `json:"source"`
	State          string `json:"state,omitempty"`
}

type CreationNameUnrecoverableRecord struct {
	HostedThreadID string `json:"hostedThreadId"`
	Provider       string `json:"provider"`
	NativeThreadID string `json:"nativeThreadId,omitempty"`
	Reason         string `json:"reason"`
	State          string `json:"state,omitempty"`
}

type CreationNameRepairPreview struct {
	Records       []CreationNameRepairRecord        `json:"records"`
	Unrecoverable []CreationNameUnrecoverableRecord `json:"unrecoverable"`
	Missing       int                               `json:"missing"`
	Existing      int                               `json:"existing"`
	Fingerprint   string                            `json:"fingerprint"`
}

type CreationNameRepairRequest struct {
	Apply               bool                              `json:"apply"`
	ExpectedFingerprint string                            `json:"expectedFingerprint,omitempty"`
	Records             []CreationNameRepairRecord        `json:"records"`
	Unrecoverable       []CreationNameUnrecoverableRecord `json:"unrecoverable,omitempty"`
}

type CreationNameRepairReceipt struct {
	Applied      bool                      `json:"applied"`
	Preview      CreationNameRepairPreview `json:"preview"`
	BackupPath   string                    `json:"backupPath,omitempty"`
	BackupSHA256 string                    `json:"backupSha256,omitempty"`
}

// RepairCreationNames previews or records an explicit, reviewed set of
// creation names. Apply reserves the serialized writer, backs it up, repeats
// the fingerprinted preview, and commits every name in one transaction.
func RepairCreationNames(ctx context.Context, db *sql.DB, request CreationNameRepairRequest, backupPath string) (receipt CreationNameRepairReceipt, err error) {
	if db == nil || db.Stats().MaxOpenConnections != 1 {
		return receipt, errors.New("threads: creation-name repair requires the serialized store writer")
	}
	records, unrecoverable, err := normalizeCreationNameRepairInput(request.Records, request.Unrecoverable)
	if err != nil {
		return receipt, err
	}
	if request.Apply && (request.ExpectedFingerprint == "" || backupPath == "") {
		return receipt, errors.New("threads: creation-name apply requires the preview fingerprint and backup destination")
	}
	connection, err := db.Conn(ctx)
	if err != nil {
		return receipt, err
	}
	defer connection.Close()
	preview, err := previewCreationNameRepair(ctx, connection, records, unrecoverable)
	if err != nil {
		return receipt, err
	}
	receipt.Preview = preview
	if request.ExpectedFingerprint != "" && request.ExpectedFingerprint != preview.Fingerprint {
		return receipt, errors.New("threads: creation-name repair preview fingerprint changed")
	}
	if !request.Apply {
		return receipt, nil
	}
	if err := BackupConnection(ctx, connection, backupPath); err != nil {
		return receipt, fmt.Errorf("threads: creation-name repair backup: %w", err)
	}
	receipt.BackupPath = backupPath
	file, err := os.Open(backupPath)
	if err != nil {
		return receipt, err
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return receipt, err
	}
	receipt.BackupSHA256 = hex.EncodeToString(digest.Sum(nil))

	// The connection remained reserved through the backup. Re-preview inside
	// the mutation transaction to catch any foreign writer.
	tx, err := connection.BeginTx(ctx, nil)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	preview, err = previewCreationNameRepairTx(ctx, tx, records, unrecoverable)
	if err != nil {
		return receipt, err
	}
	receipt.Preview = preview
	if preview.Fingerprint != request.ExpectedFingerprint {
		return receipt, errors.New("threads: creation-name repair preview fingerprint changed")
	}
	for _, record := range preview.Records {
		if record.State != "missing" {
			continue
		}
		if _, _, err := RecordCreationNameTx(ctx, tx, record.HostedThreadID, record.Name, "maintenance:"+record.Source, nowRFC3339Nano()); err != nil {
			return receipt, err
		}
	}
	for _, record := range preview.Records {
		name, ok, err := readCreationNameFrom(ctx, tx, record.HostedThreadID)
		if err != nil || !ok || name != record.Name {
			return receipt, fmt.Errorf("threads: creation-name repair verification failed for %s", record.HostedThreadID)
		}
	}
	if err := tx.Commit(); err != nil {
		return receipt, err
	}
	receipt.Applied = true
	return receipt, nil
}

func normalizeCreationNameRepairInput(input []CreationNameRepairRecord, unknown []CreationNameUnrecoverableRecord) ([]CreationNameRepairRecord, []CreationNameUnrecoverableRecord, error) {
	if len(input)+len(unknown) == 0 || len(input)+len(unknown) > 200 {
		return nil, nil, errors.New("threads: creation-name repair preview requires 1-200 classified records")
	}
	records := append([]CreationNameRepairRecord(nil), input...)
	unrecoverable := append([]CreationNameUnrecoverableRecord(nil), unknown...)
	seen := map[string]bool{}
	for i := range records {
		records[i].HostedThreadID = strings.TrimSpace(records[i].HostedThreadID)
		records[i].Provider = strings.TrimSpace(records[i].Provider)
		records[i].NativeThreadID = strings.TrimSpace(records[i].NativeThreadID)
		records[i].Name = strings.TrimSpace(records[i].Name)
		records[i].Source = strings.TrimSpace(records[i].Source)
		records[i].State = ""
		if records[i].HostedThreadID == "" || records[i].Provider == "" || records[i].NativeThreadID == "" || records[i].Name == "" || records[i].Source == "" || utf8.RuneCountInString(records[i].Name) > 200 {
			return nil, nil, errors.New("threads: every creation-name repair record requires a bounded hosted id, confirmed native id, provider, name and source")
		}
		if !creationNameRepairSources[records[i].Source] {
			return nil, nil, fmt.Errorf("threads: unsupported creation-name repair source %q", records[i].Source)
		}
		if seen[records[i].HostedThreadID] {
			return nil, nil, fmt.Errorf("threads: duplicate creation-name repair target %s", records[i].HostedThreadID)
		}
		seen[records[i].HostedThreadID] = true
	}
	for i := range unrecoverable {
		unrecoverable[i].HostedThreadID = strings.TrimSpace(unrecoverable[i].HostedThreadID)
		unrecoverable[i].Provider = strings.TrimSpace(unrecoverable[i].Provider)
		unrecoverable[i].NativeThreadID = strings.TrimSpace(unrecoverable[i].NativeThreadID)
		unrecoverable[i].Reason = strings.TrimSpace(unrecoverable[i].Reason)
		unrecoverable[i].State = ""
		if unrecoverable[i].HostedThreadID == "" || unrecoverable[i].Provider == "" || unrecoverable[i].NativeThreadID == "" || unrecoverable[i].Reason == "" {
			return nil, nil, errors.New("threads: every unrecoverable creation-name record requires hosted id, confirmed native id, provider and reason")
		}
		if seen[unrecoverable[i].HostedThreadID] {
			return nil, nil, fmt.Errorf("threads: duplicate creation-name repair target %s", unrecoverable[i].HostedThreadID)
		}
		seen[unrecoverable[i].HostedThreadID] = true
	}
	sort.Slice(records, func(i, j int) bool { return records[i].HostedThreadID < records[j].HostedThreadID })
	sort.Slice(unrecoverable, func(i, j int) bool { return unrecoverable[i].HostedThreadID < unrecoverable[j].HostedThreadID })
	return records, unrecoverable, nil
}

type creationNameRepairQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func previewCreationNameRepair(ctx context.Context, connection *sql.Conn, records []CreationNameRepairRecord, unrecoverable []CreationNameUnrecoverableRecord) (CreationNameRepairPreview, error) {
	tx, err := connection.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return CreationNameRepairPreview{}, err
	}
	defer tx.Rollback()
	return previewCreationNameRepairTx(ctx, tx, records, unrecoverable)
}

func previewCreationNameRepairTx(ctx context.Context, query creationNameRepairQuery, records []CreationNameRepairRecord, unrecoverable []CreationNameUnrecoverableRecord) (CreationNameRepairPreview, error) {
	preview := CreationNameRepairPreview{
		Records:       append([]CreationNameRepairRecord(nil), records...),
		Unrecoverable: append([]CreationNameUnrecoverableRecord(nil), unrecoverable...),
	}
	for i := range preview.Records {
		record := &preview.Records[i]
		if err := verifyCreationNameRepairIdentity(ctx, query, record.HostedThreadID, record.Provider, record.NativeThreadID); err != nil {
			return preview, err
		}
		name, ok, err := readCreationNameFrom(ctx, query, record.HostedThreadID)
		if err != nil {
			return preview, err
		}
		if ok {
			if name != record.Name {
				return preview, fmt.Errorf("threads: hosted thread %s already has a different creation name", record.HostedThreadID)
			}
			record.State = "existing"
			preview.Existing++
		} else {
			record.State = "missing"
			preview.Missing++
		}
	}
	for i := range preview.Unrecoverable {
		record := &preview.Unrecoverable[i]
		if err := verifyCreationNameRepairIdentity(ctx, query, record.HostedThreadID, record.Provider, record.NativeThreadID); err != nil {
			return preview, err
		}
		if _, ok, err := readCreationNameFrom(ctx, query, record.HostedThreadID); err != nil {
			return preview, err
		} else if ok {
			return preview, fmt.Errorf("threads: hosted thread %s has a recoverable canonical creation name", record.HostedThreadID)
		}
		record.State = "no_recoverable_name"
	}
	encoded, err := json.Marshal(preview)
	if err != nil {
		return preview, err
	}
	sum := sha256.Sum256(encoded)
	preview.Fingerprint = hex.EncodeToString(sum[:])
	return preview, nil
}

func verifyCreationNameRepairIdentity(ctx context.Context, query creationNameRepairQuery, hostedID, wantProvider, wantNative string) error {
	var provider, native string
	var provisional bool
	err := query.QueryRowContext(ctx, `SELECT h.provider,COALESCE(h.current_native_id,''),c.provisional
		FROM hosted_threads h
		JOIN hosted_thread_native_ids c ON c.hosted_thread_id=h.hosted_thread_id AND c.provider=h.provider AND c.native_thread_id=h.current_native_id AND c.retired_at IS NULL
		WHERE h.hosted_thread_id=?`, hostedID).Scan(&provider, &native, &provisional)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("threads: hosted thread %s has no confirmed current identity", hostedID)
	}
	if err != nil {
		return err
	}
	if provisional || provider != wantProvider || native != wantNative {
		return fmt.Errorf("threads: creation-name repair identity changed or is provisional for %s", hostedID)
	}
	return nil
}

func readCreationNameFrom(ctx context.Context, query creationNameRepairQuery, hostedID string) (string, bool, error) {
	var raw string
	err := query.QueryRowContext(ctx, `SELECT effective_value_json FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, hostedID, CreationNameField).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var name string
	if err := json.Unmarshal([]byte(raw), &name); err != nil || strings.TrimSpace(name) == "" {
		return "", false, errors.New("threads: stored creation name is invalid")
	}
	return name, true, nil
}

func nowRFC3339Nano() string { return time.Now().UTC().Format(time.RFC3339Nano) }

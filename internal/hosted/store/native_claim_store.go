package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// NativeOwners names the hosted threads a native conversation belongs to: its
// current owner, read by its index, or, for a native no hosted thread holds
// now (a session that moved on to another), every owner it had.
func (s *MetadataStore) NativeOwners(provider, nativeID string) ([]string, error) {
	owner, err := s.ActiveNativeOwner(provider, nativeID)
	if err != nil || owner != "" || s == nil {
		if owner == "" {
			return nil, err
		}
		return []string{owner}, err
	}
	rows, err := s.db.Query(`SELECT DISTINCT hosted_thread_id FROM hosted_thread_native_ids WHERE provider=? AND native_thread_id=?`, provider, nativeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var hosted string
		if err := rows.Scan(&hosted); err != nil {
			return nil, err
		}
		owners = append(owners, hosted)
	}
	return owners, rows.Err()
}

// ActiveNativeOwner returns the unique current hosted owner of a native conversation.
func (s *MetadataStore) ActiveNativeOwner(provider, nativeID string) (string, error) {
	if s == nil || provider == "" || nativeID == "" {
		return "", nil
	}
	rows, err := s.db.Query(`SELECT hosted_thread_id FROM hosted_thread_native_ids WHERE provider=? AND native_thread_id=? AND retired_at IS NULL LIMIT 2`, provider, nativeID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var hostedID string
	found := false
	for rows.Next() {
		if found {
			return "", fmt.Errorf("native conversation %s has conflicting current hosted owners", nativeID)
		}
		if err := rows.Scan(&hostedID); err != nil {
			return "", err
		}
		found = true
	}
	return hostedID, rows.Err()
}

// HostedKnowsNative reports whether a native identity belongs to the thread’s current or recorded history.
func (s *MetadataStore) HostedKnowsNative(hostedID, provider, nativeID string) (bool, error) {
	if s == nil || hostedID == "" || provider == "" || nativeID == "" {
		return false, nil
	}
	var current string
	err := s.db.QueryRow(`SELECT COALESCE(current_native_id,'') FROM hosted_threads WHERE hosted_thread_id=? AND provider=?`, hostedID, provider).Scan(&current)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if current == nativeID {
		return true, nil
	}
	var found string
	err = s.db.QueryRow(`SELECT hosted_thread_id FROM hosted_thread_native_ids WHERE hosted_thread_id=? AND provider=? AND native_thread_id=?`, hostedID, provider, nativeID).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return found != "", nil
}

// ResolvedHandleOwner returns the hosted thread a retired provisional handle
// still names, or "": the one thread (named, when given) whose recorded
// resolution of the handle is its current native, of which it is the only
// active owner.
func (s *MetadataStore) ResolvedHandleOwner(ctx context.Context, named, provider, handle string) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.hosted_thread_id, n.resolved_native_id, COALESCE(h.current_native_id,'') FROM hosted_thread_native_ids n JOIN hosted_threads h ON h.hosted_thread_id=n.hosted_thread_id AND h.provider=n.provider WHERE n.provider=? AND n.native_thread_id=? AND n.resolved_native_id IS NOT NULL AND (?='' OR n.hosted_thread_id=?) LIMIT 2`, provider, handle, named, named)
	if err != nil {
		return "", err
	}
	var hostedID, resolved, current string
	found := 0
	for rows.Next() {
		if err := rows.Scan(&hostedID, &resolved, &current); err != nil {
			rows.Close()
			return "", err
		}
		found++
	}
	// Closed before the owner read below, which needs the connection.
	if err := rows.Close(); err != nil {
		return "", err
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if found > 1 {
		return "", fmt.Errorf("provisional handle %s resolves for more than one hosted thread", handle)
	}
	if found == 0 || current != resolved {
		return "", nil
	}
	owner, err := s.ActiveNativeOwner(provider, resolved)
	if err != nil || owner != hostedID {
		return "", err
	}
	return hostedID, nil
}

// TransferNativeClaim is the only writer that may move an active native claim
// from one hosted record to another. Ordinary claimNativeID never steals.
func (s *MetadataStore) TransferNativeClaim(targetHostedID, provider, nativeID string, provisional bool) error {
	if s == nil || targetHostedID == "" || provider == "" || nativeID == "" {
		return fmt.Errorf("hosted id, provider, and native id are required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var hostedProvider string
	if err := tx.QueryRow(`SELECT provider FROM hosted_threads WHERE hosted_thread_id=?`, targetHostedID).Scan(&hostedProvider); err != nil {
		return err
	}
	if hostedProvider != provider {
		return fmt.Errorf("native claim provider %s conflicts with hosted provider %s", provider, hostedProvider)
	}
	var activeOwner string
	err = tx.QueryRow(`SELECT hosted_thread_id FROM hosted_thread_native_ids WHERE provider=? AND native_thread_id=? AND retired_at IS NULL`, provider, nativeID).Scan(&activeOwner)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && activeOwner == targetHostedID {
		return tx.Commit()
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE hosted_thread_native_ids SET retired_at=? WHERE provider=? AND native_thread_id=? AND retired_at IS NULL`, now, provider, nativeID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO hosted_thread_native_ids(hosted_thread_id,provider,native_thread_id,provisional,first_seen_at,last_seen_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(hosted_thread_id,provider,native_thread_id) DO UPDATE SET provisional=excluded.provisional,retired_at=NULL,last_seen_at=excluded.last_seen_at`, targetHostedID, provider, nativeID, provisional, now, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE hosted_threads SET current_native_id=?,revision=revision+1,updated_at=? WHERE hosted_thread_id=?`, nativeID, now, targetHostedID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var revision int64
	if err := s.db.QueryRow(`SELECT revision FROM hosted_threads WHERE hosted_thread_id=?`, targetHostedID).Scan(&revision); err != nil {
		return err
	}
	s.publishChange(targetHostedID, revision, "nativeThreadId")
	return nil
}

// InheritMissingDesiredPosture copies only absent desired fields from the prior native owner.
func (s *MetadataStore) InheritMissingDesiredPosture(targetID, sourceID string) error {
	if s == nil || targetID == "" || sourceID == "" || targetID == sourceID {
		return nil
	}
	source, err := s.View(sourceID)
	if err != nil {
		return fmt.Errorf("read predecessor metadata %s: %w", sourceID, err)
	}
	target, err := s.View(targetID)
	if err != nil {
		return fmt.Errorf("read target metadata %s: %w", targetID, err)
	}
	for _, field := range []string{"cwd", PostureFieldModel, PostureFieldEffort, PostureFieldPermissionMode} {
		if _, ok := ResumeString(target.Fields[field]); ok {
			continue
		}
		value, ok := ResumeString(source.Fields[field])
		if !ok {
			continue
		}
		key := fmt.Sprintf("inherit:%s:%s:%s", sourceID, targetID, field)
		if _, err := s.AppendEvent(MetadataEvent{
			HostedThreadID: targetID, Field: field, Value: value,
			Disposition: metadataDispositionRequested, Source: PostureSourceCreation,
			Freshness: metadataFreshnessLive, SourceEventKey: key,
			Evidence: "inherit:" + sourceID,
		}); err != nil {
			return fmt.Errorf("inherit %s from %s: %w", field, sourceID, err)
		}
	}
	return nil
}

// PersistMissingResumeCwd records a resume directory only when canonical cwd is absent.
func (s *MetadataStore) PersistMissingResumeCwd(hostedID, cwd string) error {
	if s == nil || hostedID == "" || strings.TrimSpace(cwd) == "" {
		return nil
	}
	view, err := s.View(hostedID)
	if err != nil {
		return err
	}
	if _, ok := ResumeString(view.Fields["cwd"]); ok {
		return nil
	}
	key := fmt.Sprintf("resume-cwd:%s:%s", hostedID, cwd)
	_, err = s.AppendEvent(MetadataEvent{
		HostedThreadID: hostedID, Field: "cwd", Value: cwd,
		Disposition: metadataDispositionRequested, Source: PostureSourceCreation,
		Freshness: metadataFreshnessLive, SourceEventKey: key,
		Evidence: "resume_cwd",
	})
	return err
}

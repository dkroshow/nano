package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	hostedvalues "github.com/dkroshow/nano/internal/hosted"
)

var (
	errNativeClaimOwned = hostedvalues.ErrNativeClaimOwned
	errNativeClaimLive  = errors.New("native identity is live-attached on another hosted record")
)

// nativeOwners names the hosted threads a native conversation belongs to: its
// current owner, read by its index, or, for a native no hosted thread holds
// now (a session that moved on to another), every owner it had.
func (s *controlMetadataStore) nativeOwners(provider, nativeID string) ([]string, error) {
	owner, err := s.activeNativeOwner(provider, nativeID)
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

func (s *controlMetadataStore) activeNativeOwner(provider, nativeID string) (string, error) {
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

func (s *controlMetadataStore) hostedKnowsNative(hostedID, provider, nativeID string) (bool, error) {
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

// resolvedHandleOwner returns the hosted thread a retired provisional handle
// still names, or "": the one thread (named, when given) whose recorded
// resolution of the handle is its current native, of which it is the only
// active owner.
func (s *controlMetadataStore) resolvedHandleOwner(ctx context.Context, named, provider, handle string) (string, error) {
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
	owner, err := s.activeNativeOwner(provider, resolved)
	if err != nil || owner != hostedID {
		return "", err
	}
	return hostedID, nil
}

// transferNativeClaim is the only writer that may move an active native claim
// from one hosted record to another. Ordinary claimNativeID never steals.
func (s *controlMetadataStore) transferNativeClaim(targetHostedID, provider, nativeID string, provisional bool) error {
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

func (s *server) transferNativeClaim(targetHostedID, provider, nativeID string) error {
	control := s.controlStore()
	if s == nil || control == nil {
		return errHostedControlUnavailable
	}
	owner, err := control.activeNativeOwner(provider, nativeID)
	if err != nil {
		return err
	}
	if owner != "" && owner != targetHostedID {
		if _, live := s.liveHostedSession(owner); live {
			return errNativeClaimLive
		}
	}
	return control.transferNativeClaim(targetHostedID, provider, nativeID, nativeThreadIDProvisional(provider, nativeID))
}

// resolveResumeHostedIdentity is a read-only routing decision. An exact active
// claim still wins. A conversation with no Nano history may proceed only after
// prepareExternalResume verifies current provider-owned source evidence; the
// actual registration remains an explicit spawn-time transaction, never a
// preview or identity-resolution side effect.
func (s *server) resolveResumeHostedIdentity(req *spawnRequest) error {
	if req == nil || req.Mode != "resume" {
		return nil
	}
	nativeID := strings.TrimSpace(req.ThreadID)
	if nativeID == "" || req.Provider == "" {
		return nil
	}
	control := s.controlStore()
	if control == nil {
		return errHostedControlUnavailable
	}
	named := strings.TrimSpace(req.HostedThreadID)
	if err := control.nativeRecoveryResumeGuard(context.Background(), named, req.Provider, nativeID); err != nil {
		return err
	}
	owner, err := control.activeNativeOwner(req.Provider, nativeID)
	if err != nil {
		return err
	}
	if owner == "" {
		// A provisional handle retires when its resolution is recorded, which
		// commits before its attachment record is re-keyed. A record left under
		// the handle still resumes, and resumeClaudeSession finishes the re-key,
		// but only while the recorded native is that thread's current one and
		// nobody else owns it.
		if nativeThreadIDProvisional(req.Provider, nativeID) {
			resolvedOwner, err := control.resolvedHandleOwner(context.Background(), named, req.Provider, nativeID)
			if err != nil {
				return err
			}
			if resolvedOwner != "" {
				req.HostedThreadID = resolvedOwner
				return nil
			}
		}
		if named != "" {
			return fmt.Errorf("hosted thread %s has no current claim for native conversation %s", named, nativeID)
		}
		return s.prepareExternalResume(context.Background(), req)
	}
	if named != "" && named != owner {
		knows, err := control.hostedKnowsNative(named, req.Provider, nativeID)
		if err != nil {
			return err
		}
		if !knows {
			return fmt.Errorf("hosted thread %s is not native conversation %s", named, nativeID)
		}
	}
	req.HostedThreadID = owner
	return nil
}

func (s *controlMetadataStore) inheritMissingDesiredPosture(targetID, sourceID string) error {
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
	for _, field := range []string{"cwd", postureFieldModel, postureFieldEffort, postureFieldPermissionMode} {
		if _, ok := hostedvalues.ResumeString(target.Fields[field]); ok {
			continue
		}
		value, ok := hostedvalues.ResumeString(source.Fields[field])
		if !ok {
			continue
		}
		key := fmt.Sprintf("inherit:%s:%s:%s", sourceID, targetID, field)
		if _, err := s.AppendEvent(metadataEvent{
			HostedThreadID: targetID, Field: field, Value: value,
			Disposition: metadataDispositionRequested, Source: postureSourceCreation,
			Freshness: metadataFreshnessLive, SourceEventKey: key,
			Evidence: "inherit:" + sourceID,
		}); err != nil {
			return fmt.Errorf("inherit %s from %s: %w", field, sourceID, err)
		}
	}
	return nil
}

func (s *controlMetadataStore) persistMissingResumeCwd(hostedID, cwd string) error {
	if s == nil || hostedID == "" || strings.TrimSpace(cwd) == "" {
		return nil
	}
	view, err := s.View(hostedID)
	if err != nil {
		return err
	}
	if _, ok := hostedvalues.ResumeString(view.Fields["cwd"]); ok {
		return nil
	}
	key := fmt.Sprintf("resume-cwd:%s:%s", hostedID, cwd)
	_, err = s.AppendEvent(metadataEvent{
		HostedThreadID: hostedID, Field: "cwd", Value: cwd,
		Disposition: metadataDispositionRequested, Source: postureSourceCreation,
		Freshness: metadataFreshnessLive, SourceEventKey: key,
		Evidence: "resume_cwd",
	})
	return err
}

func (s *server) inheritResumePosture(hostedID, provider, nativeID string) error {
	if s == nil || s.controlStore() == nil || s.agentExecutions == nil || hostedID == "" || nativeID == "" {
		return nil
	}
	matches, err := s.agentExecutions.sessionsForNativeHistory(provider, nativeID)
	if err != nil {
		return err
	}
	for _, rec := range matches {
		if rec.ID == "" || rec.ID == hostedID {
			continue
		}
		if err := s.controlStore().inheritMissingDesiredPosture(hostedID, rec.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) repairCanonicalNativeIdentity(provider, nativeID, targetHostedID string) error {
	if s == nil || s.controlStore() == nil {
		return errHostedControlUnavailable
	}
	if err := s.transferNativeClaim(targetHostedID, provider, nativeID); err != nil {
		return err
	}
	return s.inheritResumePosture(targetHostedID, provider, nativeID)
}

func (s *server) controlStore() *controlMetadataStore {
	if s == nil {
		return nil
	}
	if s.controlMetadata != nil && s.controlMetadataErr == "" {
		return s.controlMetadata
	}
	if s.agentExecutions != nil {
		return s.agentExecutions.control
	}
	return nil
}

func (s *server) nativeCurrentRecords(nativeID string) ([]agentSessionRecord, error) {
	if s == nil || s.agentExecutions == nil || nativeID == "" {
		return nil, nil
	}
	providers, err := s.agentExecutions.nativeHistoryProviders(nativeID)
	if err != nil {
		return nil, err
	}
	if len(providers) > 1 {
		var all []agentSessionRecord
		for _, provider := range providers {
			matches, err := s.agentExecutions.sessionsForNativeHistory(provider, nativeID)
			if err != nil {
				return nil, err
			}
			all = append(all, matches...)
		}
		return all, nil
	}
	if len(providers) == 0 {
		return nil, nil
	}
	provider := providers[0]
	if control := s.controlStore(); control != nil {
		owner, err := control.activeNativeOwner(provider, nativeID)
		if err != nil {
			return nil, err
		}
		if owner != "" {
			record, ok, err := s.agentExecutions.sessionFromCanonical(owner)
			if err != nil {
				return nil, err
			}
			if ok {
				return []agentSessionRecord{record}, nil
			}
			return []agentSessionRecord{{ID: owner, Provider: provider, NativeThreadID: nativeID}}, nil
		}
	}
	return s.agentExecutions.sessionsForNativeHistory(provider, nativeID)
}

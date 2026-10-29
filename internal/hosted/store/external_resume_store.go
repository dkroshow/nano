package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ExternalResumeFacts are the verified identity and posture supplied at admission.
type ExternalResumeFacts struct {
	Verified                                              bool
	Provider, NativeThreadID, PresentationMode, CatalogID string
	Metadata                                              map[string]string
	NativeRecovery                                        bool
}

// WasExternalResumeRegistration is the durable retry marker for provider
// posture. Every first registration records at least cwd with this source key,
// so later attempts can distinguish provider-history posture from an ordinary
// hosted resume even when all optional settings were unknown. GLOB, not LIKE:
// the case-sensitive prefix searches the source_event_key index, where LIKE
// scanned every metadata event once per boot registration.
func (s *MetadataStore) WasExternalResumeRegistration(hostedID string) (bool, error) {
	if s == nil || hostedID == "" {
		return false, nil
	}
	var found int
	err := s.db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM thread_metadata_events
		WHERE hosted_thread_id=? AND source_event_key GLOB 'external-resume:*'
	)`, hostedID).Scan(&found)
	return found != 0, err
}

// EnsureExternalResumeSession atomically creates the first hosted identity,
// its active native claim, and the historical provider posture used to resume.
// The store lock serializes in-process retries; the partial unique index and
// transaction are the durable cross-request guard.
func (s *ExecutionStore) EnsureExternalResumeSession(req ExternalResumeFacts) (agentSessionRecord, error) {
	if s == nil || s.control == nil || !req.Verified || req.Provider == "" || req.NativeThreadID == "" {
		return agentSessionRecord{}, errors.New("verified external resume evidence is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if owner, err := s.control.ActiveNativeOwner(req.Provider, req.NativeThreadID); err != nil {
		return agentSessionRecord{}, err
	} else if owner != "" {
		record, ok, err := s.sessionFromCanonicalLocked(owner)
		if err != nil {
			return agentSessionRecord{}, err
		}
		if !ok {
			return agentSessionRecord{}, fmt.Errorf("current hosted owner %s disappeared", owner)
		}
		s.sessions[owner] = record
		s.sessionBaseline[owner] = record.Revision
		return record, nil
	}

	hostedID, err := mintHostedThreadID()
	if err != nil {
		return agentSessionRecord{}, err
	}
	mode, err := NormalizePresentationMode(req.PresentationMode)
	if err != nil {
		return agentSessionRecord{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.control.db.Begin()
	if err != nil {
		return agentSessionRecord{}, err
	}
	defer tx.Rollback()

	var activeOwner string
	err = tx.QueryRow(`SELECT hosted_thread_id FROM hosted_thread_native_ids WHERE provider=? AND native_thread_id=? AND retired_at IS NULL`, req.Provider, req.NativeThreadID).Scan(&activeOwner)
	if err == nil {
		return agentSessionRecord{}, fmt.Errorf("native conversation %s acquired hosted owner %s during registration; retry Resume", req.NativeThreadID, activeOwner)
	}
	if err != sql.ErrNoRows {
		return agentSessionRecord{}, err
	}
	var historical int
	if err := tx.QueryRow(`SELECT count(*) FROM hosted_thread_native_ids WHERE provider=? AND native_thread_id=?`, req.Provider, req.NativeThreadID).Scan(&historical); err != nil {
		return agentSessionRecord{}, err
	}
	if historical != 0 {
		return agentSessionRecord{}, fmt.Errorf("native conversation %s was previously hosted by Nano; refusing a duplicate registration", req.NativeThreadID)
	}
	if req.NativeRecovery {
		var held int
		if err := tx.QueryRow(`SELECT count(*) FROM hosted_native_recoveries WHERE
			(old_native_id=? AND new_native_id<>'' AND state IN ('bound_pending_activation','activated')) OR
			(state IN ('reserved','creating','created','bound_pending_activation','creation_unknown') AND
			(old_native_id=? OR new_native_id=?))`, req.NativeThreadID, req.NativeThreadID, req.NativeThreadID).Scan(&held); err != nil {
			return agentSessionRecord{}, err
		}
		if held != 0 {
			return agentSessionRecord{}, fmt.Errorf("%w: recovery-held or retired native identity", errNativeRecoveryConflict)
		}
	}

	fields := make([]string, 0, len(req.Metadata))
	for _, field := range []string{"cwd", PostureFieldModel, PostureFieldEffort, PostureFieldServiceTier, PostureFieldPermissionMode} {
		if strings.TrimSpace(req.Metadata[field]) != "" {
			fields = append(fields, field)
		}
	}
	revision := int64(1 + len(fields))
	if _, err := tx.Exec(`INSERT INTO hosted_threads(hosted_thread_id,provider,current_native_id,presentation_mode,sidebar_placement,notify_invoker_on_terminal,terminal_notification_policy,revision,created_at,updated_at) VALUES(?,?,?,?,?,0,'none',?,?,?)`, hostedID, req.Provider, req.NativeThreadID, mode, sidebarPlacementInvoker, revision, now, now); err != nil {
		return agentSessionRecord{}, err
	}
	if _, err := tx.Exec(`INSERT INTO hosted_execution_sessions(hosted_thread_id,client_session_id,request_hash,nano_session_id,status,error,run_id,created_at,updated_at,revision) VALUES(?,NULL,'',NULL,?,NULL,?,?,?,1)`, hostedID, agentSessionCreating, s.runID, now, now); err != nil {
		return agentSessionRecord{}, err
	}
	if _, err := tx.Exec(`INSERT INTO hosted_thread_native_ids(hosted_thread_id,provider,native_thread_id,provisional,first_seen_at,last_seen_at) VALUES(?,?,?,0,?,?)`, hostedID, req.Provider, req.NativeThreadID, now, now); err != nil {
		return agentSessionRecord{}, err
	}
	for _, field := range fields {
		value, _ := json.Marshal(req.Metadata[field])
		key := fmt.Sprintf("external-resume:%s:%s:%s", req.Provider, req.NativeThreadID, field)
		result, err := tx.Exec(`INSERT INTO thread_metadata_events(hosted_thread_id,field,value_json,disposition,source,evidence,native_thread_id,nano_session_id,attachment_generation,evidence_freshness,causation_event_id,source_event_key,observed_at,committed_at)
			VALUES(?,?,?,?,?,?,?,NULL,0,?,NULL,?,?,?)`, hostedID, field, string(value), metadataDispositionObserved, PostureSourceProvider, "external_resume_catalog:"+req.CatalogID, req.NativeThreadID, metadataFreshnessHistorical, key, now, now)
		if err != nil {
			return agentSessionRecord{}, err
		}
		eventID, _ := result.LastInsertId()
		if _, err := tx.Exec(`INSERT INTO thread_metadata_current(hosted_thread_id,field,desired_value_json,desired_disposition,desired_source,desired_event_id,desired_settlement_event_id,effective_value_json,effective_disposition,effective_source,effective_event_id,thread_revision,updated_at)
			VALUES(?,?,NULL,?,NULL,NULL,NULL,?,?,?,?,?,?)`, hostedID, field, metadataDispositionUnknown, string(value), metadataDispositionObserved, PostureSourceProvider, eventID, revision, now); err != nil {
			return agentSessionRecord{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return agentSessionRecord{}, err
	}
	record := agentSessionRecord{
		ID: hostedID, Provider: req.Provider, NativeThreadID: req.NativeThreadID,
		PresentationMode: mode, SidebarPlacement: sidebarPlacementInvoker,
		Status: agentSessionCreating, RunID: s.runID, CreatedAt: now, UpdatedAt: now, Revision: 1,
	}
	s.sessions[hostedID] = record
	s.sessionBaseline[hostedID] = record.Revision
	changed := append([]string{"identity", "nativeThreadId"}, fields...)
	s.control.publishChange(hostedID, revision, changed...)
	return record, nil
}

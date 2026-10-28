package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/threadindex"

	"github.com/dkroshow/nano/internal/history"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/sessions"
)

type externalResumeSourceVerifier interface {
	VerifyExternalResumeSource(context.Context, string, string, string) (history.ThreadProjection, error)
}

// prepareExternalResume proves that a provider-native conversation exists and
// has never been registered in Nano. It is deliberately read-only so preview,
// link resolution, and opening a historical pane cannot create hosted state.
func (s *server) prepareExternalResume(ctx context.Context, req *spawnRequest) error {
	if s == nil || req == nil || req.Provider == "" || req.ThreadID == "" {
		return errors.New("provider and native conversation id are required")
	}
	if s.agentExecutions == nil || s.controlStore() == nil {
		return errHostedControlUnavailable
	}
	historyRecords, err := s.agentExecutions.sessionsForNativeHistory(req.Provider, req.ThreadID)
	if err != nil {
		return err
	}
	if len(historyRecords) != 0 {
		return fmt.Errorf("native conversation %s was previously hosted by Nano but has no current claim; refusing first registration", req.ThreadID)
	}
	if rec, ok := s.attachmentFor(req.Provider, req.ThreadID); ok && rec.HostedThreadID != "" {
		return fmt.Errorf("native conversation %s has a historical Nano attachment %s but no current claim", req.ThreadID, rec.HostedThreadID)
	}
	if s.catalog == nil {
		return fmt.Errorf("cannot verify native conversation %s: thread catalog unavailable", req.ThreadID)
	}
	page, err := s.catalog.ListThreads(ctx, catalogListQuery{
		IncludeSubagents: true, IncludeGuardians: true, IncludeProgrammatic: true,
		Archived: archivedIncludeFilter, ThreadID: req.ThreadID, Limit: 1,
	})
	if err != nil {
		return fmt.Errorf("verify native conversation %s: %w", req.ThreadID, err)
	}
	if len(page.Items) == 0 {
		return fmt.Errorf("native conversation %s was not found in current provider history", req.ThreadID)
	}
	row := page.Items[0]
	if row.NativeID != req.ThreadID || row.Provider != req.Provider {
		return fmt.Errorf("native conversation %s belongs to provider %s, not %s", req.ThreadID, row.Provider, req.Provider)
	}
	detail, err := s.catalog.LoadThread(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("load native conversation evidence for %s: %w", req.ThreadID, err)
	}
	if detail.NativeID != req.ThreadID || detail.Provider != req.Provider {
		return fmt.Errorf("native conversation %s provider identity changed during verification", req.ThreadID)
	}
	if detail.Source.Conflict {
		return fmt.Errorf("native conversation %s has conflicting provider source records", req.ThreadID)
	}
	if detail.Source.Status != string(history.SourcePresent) || detail.Sources == nil {
		return fmt.Errorf("native conversation %s has no current provider source", req.ThreadID)
	}
	verifiedSource := false
	for _, source := range *detail.Sources {
		if source.Status != string(history.SourcePresent) || source.IdentityEvidence != string(history.EvidenceNativeDurable) {
			continue
		}
		info, statErr := os.Lstat(source.Path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		verifiedSource = true
		break
	}
	if !verifiedSource {
		return fmt.Errorf("native conversation %s lacks a present provider source with durable identity evidence", req.ThreadID)
	}
	if verifier, ok := s.catalog.(externalResumeSourceVerifier); ok {
		projection, err := verifier.VerifyExternalResumeSource(ctx, detail.ID, req.Provider, req.ThreadID)
		if err != nil {
			return fmt.Errorf("native conversation %s current provider source failed verification: %w", req.ThreadID, err)
		}
		detail.Metadata = threadindex.MapMetadata(projection.Metadata)
	}

	metadata := map[string]string{}
	for field, value := range map[string]catalogValue{
		"cwd":                                  detail.Metadata.CWD,
		hostedmodel.PostureFieldModel:          detail.Metadata.Model,
		hostedmodel.PostureFieldEffort:         detail.Metadata.Effort,
		hostedmodel.PostureFieldServiceTier:    detail.Metadata.Priority,
		hostedmodel.PostureFieldPermissionMode: detail.Metadata.Permission,
	} {
		if known := knownCatalogValue(value); known != "" {
			metadata[field] = known
		}
	}
	if metadata["cwd"] == "" {
		return fmt.Errorf("native conversation %s has no recorded working directory; Resume cannot safely choose one", req.ThreadID)
	}

	// Codex exposes a non-attaching provider read. The catalog proves durable
	// identity for every provider; thread/read additionally proves this daemon
	// can resolve the exact rollout before Nano commits registration.
	if req.Provider == "codex" {
		if s.codex == nil {
			return errors.New("cannot verify Codex resume availability: app-server unavailable")
		}
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var result struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if err := s.codex.Call(readCtx, "thread/read", map[string]any{"threadId": req.ThreadID, "includeTurns": false}, &result); err != nil {
			return fmt.Errorf("Codex conversation %s is not available to resume: %w", req.ThreadID, err)
		}
		if result.Thread.ID != req.ThreadID {
			return fmt.Errorf("Codex conversation verification returned %q for requested %q", result.Thread.ID, req.ThreadID)
		}
	}

	req.externalResume = true
	req.ExternalResumePosture = true
	req.externalResumeCatalog = detail.ID
	req.externalResumeMetadata = metadata
	req.Cwd = metadata["cwd"]
	req.Model = metadata[hostedmodel.PostureFieldModel]
	req.Effort = metadata[hostedmodel.PostureFieldEffort]
	req.ServiceTier = metadata[hostedmodel.PostureFieldServiceTier]
	req.PermissionMode = metadata[hostedmodel.PostureFieldPermissionMode]
	return nil
}

// wasExternalResumeRegistration is the durable retry marker for provider
// posture. Every first registration records at least cwd with this source key,
// so later attempts can distinguish provider-history posture from an ordinary
// hosted resume even when all optional settings were unknown. GLOB, not LIKE:
// the case-sensitive prefix searches the source_event_key index, where LIKE
// scanned every metadata event once per boot registration.
func (s *controlMetadataStore) wasExternalResumeRegistration(hostedID string) (bool, error) {
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

// ensureExternalResumeSession atomically creates the first hosted identity,
// its active native claim, and the historical provider posture used to resume.
// The store lock serializes in-process retries; the partial unique index and
// transaction are the durable cross-request guard.
func (s *agentExecutionStore) ensureExternalResumeSession(req spawnRequest) (agentSessionRecord, error) {
	if s == nil || s.control == nil || !req.externalResume || req.Provider == "" || req.ThreadID == "" {
		return agentSessionRecord{}, errors.New("verified external resume evidence is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if owner, err := s.control.activeNativeOwner(req.Provider, req.ThreadID); err != nil {
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
	mode, err := normalizePresentationMode(req.PresentationMode)
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
	err = tx.QueryRow(`SELECT hosted_thread_id FROM hosted_thread_native_ids WHERE provider=? AND native_thread_id=? AND retired_at IS NULL`, req.Provider, req.ThreadID).Scan(&activeOwner)
	if err == nil {
		return agentSessionRecord{}, fmt.Errorf("native conversation %s acquired hosted owner %s during registration; retry Resume", req.ThreadID, activeOwner)
	}
	if err != sql.ErrNoRows {
		return agentSessionRecord{}, err
	}
	var historical int
	if err := tx.QueryRow(`SELECT count(*) FROM hosted_thread_native_ids WHERE provider=? AND native_thread_id=?`, req.Provider, req.ThreadID).Scan(&historical); err != nil {
		return agentSessionRecord{}, err
	}
	if historical != 0 {
		return agentSessionRecord{}, fmt.Errorf("native conversation %s was previously hosted by Nano; refusing a duplicate registration", req.ThreadID)
	}
	if sessions.ProviderLifecycleCapabilities(req.Provider).NativeRecovery {
		var held int
		if err := tx.QueryRow(`SELECT count(*) FROM hosted_native_recoveries WHERE
			(old_native_id=? AND new_native_id<>'' AND state IN ('bound_pending_activation','activated')) OR
			(state IN ('reserved','creating','created','bound_pending_activation','creation_unknown') AND
			(old_native_id=? OR new_native_id=?))`, req.ThreadID, req.ThreadID, req.ThreadID).Scan(&held); err != nil {
			return agentSessionRecord{}, err
		}
		if held != 0 {
			return agentSessionRecord{}, fmt.Errorf("%w: recovery-held or retired native identity", errNativeRecoveryConflict)
		}
	}

	fields := make([]string, 0, len(req.externalResumeMetadata))
	for _, field := range []string{"cwd", hostedmodel.PostureFieldModel, hostedmodel.PostureFieldEffort, hostedmodel.PostureFieldServiceTier, hostedmodel.PostureFieldPermissionMode} {
		if strings.TrimSpace(req.externalResumeMetadata[field]) != "" {
			fields = append(fields, field)
		}
	}
	revision := int64(1 + len(fields))
	if _, err := tx.Exec(`INSERT INTO hosted_threads(hosted_thread_id,provider,current_native_id,presentation_mode,sidebar_placement,notify_invoker_on_terminal,terminal_notification_policy,revision,created_at,updated_at) VALUES(?,?,?,?,?,0,'none',?,?,?)`, hostedID, req.Provider, req.ThreadID, mode, sidebarPlacementInvoker, revision, now, now); err != nil {
		return agentSessionRecord{}, err
	}
	if _, err := tx.Exec(`INSERT INTO hosted_execution_sessions(hosted_thread_id,client_session_id,request_hash,nano_session_id,status,error,run_id,created_at,updated_at,revision) VALUES(?,NULL,'',NULL,?,NULL,?,?,?,1)`, hostedID, agentSessionCreating, s.runID, now, now); err != nil {
		return agentSessionRecord{}, err
	}
	if _, err := tx.Exec(`INSERT INTO hosted_thread_native_ids(hosted_thread_id,provider,native_thread_id,provisional,first_seen_at,last_seen_at) VALUES(?,?,?,0,?,?)`, hostedID, req.Provider, req.ThreadID, now, now); err != nil {
		return agentSessionRecord{}, err
	}
	for _, field := range fields {
		value, _ := json.Marshal(req.externalResumeMetadata[field])
		key := fmt.Sprintf("external-resume:%s:%s:%s", req.Provider, req.ThreadID, field)
		result, err := tx.Exec(`INSERT INTO thread_metadata_events(hosted_thread_id,field,value_json,disposition,source,evidence,native_thread_id,nano_session_id,attachment_generation,evidence_freshness,causation_event_id,source_event_key,observed_at,committed_at)
			VALUES(?,?,?,?,?,?,?,NULL,0,?,NULL,?,?,?)`, hostedID, field, string(value), metadataDispositionObserved, hostedmodel.PostureSourceProvider, "external_resume_catalog:"+req.externalResumeCatalog, req.ThreadID, metadataFreshnessHistorical, key, now, now)
		if err != nil {
			return agentSessionRecord{}, err
		}
		eventID, _ := result.LastInsertId()
		if _, err := tx.Exec(`INSERT INTO thread_metadata_current(hosted_thread_id,field,desired_value_json,desired_disposition,desired_source,desired_event_id,desired_settlement_event_id,effective_value_json,effective_disposition,effective_source,effective_event_id,thread_revision,updated_at)
			VALUES(?,?,NULL,?,NULL,NULL,NULL,?,?,?,?,?,?)`, hostedID, field, metadataDispositionUnknown, string(value), metadataDispositionObserved, hostedmodel.PostureSourceProvider, eventID, revision, now); err != nil {
			return agentSessionRecord{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return agentSessionRecord{}, err
	}
	record := agentSessionRecord{
		ID: hostedID, Provider: req.Provider, NativeThreadID: req.ThreadID,
		PresentationMode: mode, SidebarPlacement: sidebarPlacementInvoker,
		Status: agentSessionCreating, RunID: s.runID, CreatedAt: now, UpdatedAt: now, Revision: 1,
	}
	s.sessions[hostedID] = record
	s.sessionBaseline[hostedID] = record.Revision
	changed := append([]string{"identity", "nativeThreadId"}, fields...)
	s.control.publishChange(hostedID, revision, changed...)
	return record, nil
}

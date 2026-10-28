package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	state "github.com/dkroshow/nano/internal/app/statestore"
	"github.com/dkroshow/nano/internal/httpapi"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type retiredHostedReceipt struct {
	state.RetirementReceipt
	OK          bool                      `json:"ok"`
	Error       string                    `json:"error,omitempty"`
	Resolutions []identityResolveResponse `json:"resolutions"`
}

func (s *server) handleRetiredHostedRecords(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var wire struct {
		Apply               *bool                   `json:"apply"`
		OutboxDisposition   string                  `json:"outboxDisposition"`
		ExpectedFingerprint string                  `json:"expectedFingerprint,omitempty"`
		ExpectedCounts      *state.RetirementCounts `json:"expectedCounts,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil || wire.Apply == nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "explicit apply and outboxDisposition are required")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "expected one JSON object")
		return
	}
	if wire.OutboxDisposition != "repoint" && wire.OutboxDisposition != "delete" || (*wire.Apply && wire.ExpectedFingerprint == "") {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "choose repoint or delete; apply also requires expectedFingerprint from preview")
		return
	}
	request := state.RetirementRequest{Apply: *wire.Apply, OutboxDisposition: wire.OutboxDisposition, ExpectedFingerprint: wire.ExpectedFingerprint, ExpectedCounts: wire.ExpectedCounts}
	receipt := s.retireHostedRecords(r.Context(), request)
	status := http.StatusOK
	if !receipt.OK {
		status = http.StatusConflict
	}
	writeCatalogJSON(w, status, receipt)
}

func (s *server) retireHostedRecords(ctx context.Context, request state.RetirementRequest) retiredHostedReceipt {
	result := retiredHostedReceipt{Resolutions: []identityResolveResponse{}}
	control := s.controlStore()
	if control == nil || s.agentExecutions == nil || s.queuedTurns == nil || s.agentExecutions.control != control || s.queuedTurns.db != control.db {
		result.Error = "canonical retirement stores are unavailable"
		return result
	}
	// Match the relationship -> execution -> database order of ordinary thread
	// mutations.
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	s.agentExecutions.mu.Lock()
	defer s.agentExecutions.mu.Unlock()
	check := func(preview state.RetirementPreview) error {
		retired := map[string]bool{}
		for _, target := range preview.Records {
			retired[target.HostedThreadID] = true
		}
		// Runtime/attachment edges require a wider operation. Historical duplicate
		// cleanup must not detach a provider or rewrite an attachment file.
		return s.checkRetiredReferences(retired, func() error {
			if s.attachments != nil {
				s.attachments.mu.Lock()
				defer s.attachments.mu.Unlock()
				for _, attachment := range s.attachments.byKey {
					if retired[attachment.HostedThreadID] || retired[attachment.InvokerHostedThreadID] {
						return fmt.Errorf("retired identity still has a durable attachment reference")
					}
				}
			}
			return nil
		})
	}
	verify := func(tx *sql.Tx, targets []state.RetiredHostedRecord) error {
		resolutions := []identityResolveResponse{}
		for _, target := range targets {
			resolved, err := resolveCanonicalNativeIdentity(ctx, tx, target.NativeThreadID)
			if err != nil {
				return err
			}
			if resolved.MatchCount != 1 || resolved.HostedThreadID != target.OwnerHostedThreadID || resolved.NativeThreadID != target.NativeThreadID || resolved.Provider != target.Provider || resolved.NativeThreadIDProvisional {
				return fmt.Errorf("native identity %s did not resolve to its surviving owner", target.NativeThreadID)
			}
			// Names are a separate read-only overlay and do not enter the transaction.
			if name, ok := s.durableThreadName(resolved.Provider, resolved.NativeThreadID); ok {
				resolved.DisplayName = name
			}
			resolutions = append(resolutions, resolved)
		}
		result.Resolutions = resolutions
		return nil
	}
	backupPath := ""
	if request.Apply {
		parent := filepath.Join(filepath.Dir(s.queuedTurns.path), "threads-maintenance-backups")
		if err := os.MkdirAll(parent, 0700); err != nil {
			result.Error = err.Error()
			return result
		}
		dir, err := os.MkdirTemp(parent, "retired-hosted-")
		if err != nil {
			result.Error = err.Error()
			return result
		}
		backupPath = filepath.Join(dir, "nano_threads.sqlite")
	}
	receipt, err := state.RetireHostedRecords(ctx, control.db, request, backupPath, check, verify)
	result.RetirementReceipt = receipt
	if err != nil {
		result.Resolutions = []identityResolveResponse{}
		result.Error = err.Error()
		return result
	}
	if receipt.Applied {
		s.applyRetiredHostedCaches(receipt.Preview)
	}
	result.OK = true
	return result
}

// The caller holds execution and queue locks. These assignments cannot fail;
// all fallible work, including resolver proof, happened before SQLite commit.
func (s *server) applyRetiredHostedCaches(preview state.RetirementPreview) {
	executions := s.agentExecutions
	replacements := map[string]string{}
	for _, target := range preview.Records {
		replacements[target.HostedThreadID] = target.OwnerHostedThreadID
		if rec, ok := executions.sessions[target.HostedThreadID]; ok {
			delete(executions.byClient, rec.ClientSessionID)
		}
		delete(executions.sessions, target.HostedThreadID)
		delete(executions.sessionBaseline, target.HostedThreadID)
		delete(executions.deletedSessions, target.HostedThreadID)
	}
	for id, rec := range executions.sessions {
		if owner := replacements[rec.InvokerHostedThreadID]; owner != "" {
			rec.InvokerHostedThreadID = owner
			executions.sessions[id] = rec
		}
	}
	for id, rec := range executions.turns {
		if replacements[rec.HostedThreadID] != "" {
			executions.setTurnLocked(id, nil)
			delete(executions.turnBaseline, id)
			delete(executions.deletedTurns, id)
			continue
		}
		if owner := replacements[rec.SenderHostedThreadID]; owner != "" {
			rec.SenderHostedThreadID = owner
			executions.setTurnLocked(id, &rec)
		}
	}
	control := executions.control
	control.volumeMu.Lock()
	for key := range control.eventVolume {
		hostedID, _, _ := strings.Cut(key, "\x00")
		if replacements[hostedID] != "" {
			delete(control.eventVolume, key)
			delete(control.warnedVolume, key)
		}
	}
	control.volumeMu.Unlock()
	executions.publishPendingTransitionsLocked()
}

package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"

	state "github.com/dkroshow/nano/internal/app/statestore"
	"github.com/dkroshow/nano/internal/threads"
)

// RepairCreationNames keeps maintenance and its cache notifications under the execution lock.
func (s *ExecutionStore) RepairCreationNames(ctx context.Context, request threads.CreationNameRepairRequest, databasePath string, publish func(threads.CreationNameRepairReceipt) error) (threads.CreationNameRepairReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	backupPath := ""
	if request.Apply {
		parent := filepath.Join(filepath.Dir(databasePath), "threads-maintenance-backups")
		if err := os.MkdirAll(parent, 0700); err != nil {
			return threads.CreationNameRepairReceipt{}, err
		}
		dir, err := os.MkdirTemp(parent, "creation-names-")
		if err != nil {
			return threads.CreationNameRepairReceipt{}, err
		}
		backupPath = filepath.Join(dir, "nano_threads.sqlite")
	}
	receipt, err := threads.RepairCreationNames(ctx, s.control.db, request, backupPath)
	if err != nil {
		return receipt, err
	}
	if receipt.Applied {
		for _, record := range receipt.Preview.Records {
			var revision int64
			if err := s.control.db.QueryRow(`SELECT revision FROM hosted_threads WHERE hosted_thread_id=?`, record.HostedThreadID).Scan(&revision); err == nil {
				s.control.publishChange(record.HostedThreadID, revision, threads.CreationNameField)
			}
		}
		if err := publish(receipt); err != nil {
			return receipt, err
		}
	}
	return receipt, nil
}

// RetireHostedRecords applies retirement and cache changes under one execution lock.
func (s *ExecutionStore) RetireHostedRecords(ctx context.Context, request state.RetirementRequest, databasePath string, check func(state.RetirementPreview) error, verify func(*sql.Tx, []state.RetiredHostedRecord) error) (state.RetirementReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	backupPath := ""
	if request.Apply {
		parent := filepath.Join(filepath.Dir(databasePath), "threads-maintenance-backups")
		if err := os.MkdirAll(parent, 0700); err != nil {
			return state.RetirementReceipt{}, err
		}
		dir, err := os.MkdirTemp(parent, "retired-hosted-")
		if err != nil {
			return state.RetirementReceipt{}, err
		}
		backupPath = filepath.Join(dir, "nano_threads.sqlite")
	}
	receipt, err := state.RetireHostedRecords(ctx, s.control.db, request, backupPath, check, verify)
	if err != nil {
		return receipt, err
	}
	if receipt.Applied {
		s.applyRetiredHostedCaches(receipt.Preview)
	}
	return receipt, nil
}

// The caller holds the execution lock. These assignments cannot fail;
// all fallible work, including resolver proof, happened before SQLite commit.
func (executions *ExecutionStore) applyRetiredHostedCaches(preview state.RetirementPreview) {
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

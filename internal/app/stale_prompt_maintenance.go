package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threads"
)

// stalePromptAge is how old an unsettled delivered prompt must be before an
// operator may expire it.
const stalePromptAge = 24 * time.Hour

type stalePromptCounts struct {
	Accepted  int `json:"accepted"`
	Uncertain int `json:"uncertain"`
}

type stalePromptReceipt struct {
	OK             bool                         `json:"ok"`
	Error          string                       `json:"error,omitempty"`
	Applied        bool                         `json:"applied"`
	Cutoff         string                       `json:"cutoff"`
	Fingerprint    string                       `json:"fingerprint"`
	Candidates     int                          `json:"candidates"`
	Threads        int                          `json:"threads"`
	ByProvider     map[string]stalePromptCounts `json:"byProvider"`
	RunningThreads int                          `json:"runningThreads"`
	ClientActionID string                       `json:"clientActionId,omitempty"`
	BackupPath     string                       `json:"backupPath,omitempty"`
	Settled        int                          `json:"settled"`
	Expired        int                          `json:"expired"`
	Unchanged      int                          `json:"unchanged"`
}

// handleStalePrompts previews or applies the expiry of old delivered prompts
// that exact identity never settled. Nothing invokes it automatically.
func (s *server) handleStalePrompts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var wire struct {
		Apply               *bool  `json:"apply"`
		Cutoff              string `json:"cutoff,omitempty"`
		ExpectedFingerprint string `json:"expectedFingerprint,omitempty"`
		ClientActionID      string `json:"clientActionId,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil || wire.Apply == nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "explicit apply is required")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "expected one JSON object")
		return
	}
	cutoff := time.Now().UTC().Add(-stalePromptAge)
	if *wire.Apply {
		parsed, err := time.Parse(time.RFC3339Nano, wire.Cutoff)
		if err != nil || wire.ExpectedFingerprint == "" || !validAgentClientID(wire.ClientActionID) {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "apply requires the preview's cutoff and fingerprint, and a clientActionId")
			return
		}
		if parsed.After(cutoff) {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_request", "cutoff must be at least 24 hours old")
			return
		}
		cutoff = parsed
	}
	receipt := s.expireStalePrompts(r.Context(), *wire.Apply, cutoff, wire.ExpectedFingerprint, wire.ClientActionID)
	status := http.StatusOK
	if !receipt.OK {
		status = http.StatusConflict
	}
	writeCatalogJSON(w, status, receipt)
}

func (s *server) expireStalePrompts(ctx context.Context, apply bool, cutoff time.Time, expectedFingerprint, clientActionID string) stalePromptReceipt {
	receipt := stalePromptReceipt{Cutoff: cutoff.Format(time.RFC3339Nano), ByProvider: map[string]stalePromptCounts{}}
	if s.turns == nil || s.queuedTurns == nil {
		receipt.Error = "prompt store is unavailable"
		return receipt
	}
	stale, err := s.turns.Stale(ctx, cutoff)
	if err != nil {
		receipt.Error = err.Error()
		return receipt
	}
	running := map[string]bool{}
	for _, record := range stale {
		hosted := record.Outbox.HostedThreadID
		if _, seen := running[hosted]; !seen {
			running[hosted] = s.threadBusy(hosted)
		}
	}
	byThread := map[string][]prompts.Record{}
	digest := sha256.New()
	fmt.Fprintf(digest, "%s\n", receipt.Cutoff)
	for _, record := range stale {
		hosted := record.Outbox.HostedThreadID
		if running[hosted] {
			continue
		}
		byThread[hosted] = append(byThread[hosted], record)
		fmt.Fprintf(digest, "%s %d\n", record.Outbox.ActionID, record.Revision)
		counts := receipt.ByProvider[string(record.Provider)]
		if record.Outbox.State == prompts.Uncertain {
			counts.Uncertain++
		} else {
			counts.Accepted++
		}
		receipt.ByProvider[string(record.Provider)] = counts
		receipt.Candidates++
	}
	for _, busy := range running {
		if busy {
			receipt.RunningThreads++
		}
	}
	receipt.Threads = len(byThread)
	receipt.Fingerprint = hex.EncodeToString(digest.Sum(nil))
	if !apply {
		receipt.OK = true
		return receipt
	}
	if expectedFingerprint != receipt.Fingerprint {
		receipt.Error = "stale prompts changed since the preview; preview again"
		return receipt
	}
	receipt.ClientActionID = clientActionID
	parent := filepath.Join(filepath.Dir(s.queuedTurns.Path()), "threads-maintenance-backups")
	if err := os.MkdirAll(parent, 0700); err != nil {
		receipt.Error = err.Error()
		return receipt
	}
	dir, err := os.MkdirTemp(parent, "stale-prompts-")
	if err != nil {
		receipt.Error = err.Error()
		return receipt
	}
	receipt.BackupPath = filepath.Join(dir, "nano_threads.sqlite")
	if err := threads.Backup(ctx, s.queuedTurns.Database(), receipt.BackupPath); err != nil {
		receipt.Error = "backup failed; nothing changed: " + err.Error()
		return receipt
	}
	// From here a failure can leave the apply partial; the counts say how far.
	receipt.Applied = true
	hostedIDs := make([]string, 0, len(byThread))
	for hosted := range byThread {
		hostedIDs = append(hostedIDs, hosted)
	}
	sort.Strings(hostedIDs)
	for _, hosted := range hostedIDs {
		records := byThread[hosted]
		// Exact identity settles what the saved transcript proves first;
		// only the rest expire.
		if err := s.settleStaleThread(ctx, hosted, records); err != nil {
			receipt.Unchanged += len(records)
			log.Printf("stale prompts %s: thread %s left unchanged: %v", clientActionID, hosted, err)
			continue
		}
		// Checked again after the saved reads, just before the writes.
		if s.threadBusy(hosted) {
			receipt.Unchanged += len(records)
			continue
		}
		for _, record := range records {
			current, expired, err := s.turns.Expire(ctx, record.Outbox.ActionID, cutoff)
			switch {
			case err != nil:
				receipt.Error = err.Error()
				return receipt
			case expired:
				receipt.Expired++
				// An open pane drops an expired prompt only when told.
				if sess := s.SessionForHostedThread(hosted); sess != nil {
					sess.PublishLiveOutbox(current.Outbox)
				}
			case current.Outbox.State == prompts.Confirmed:
				receipt.Settled++
			default:
				receipt.Unchanged++
			}
		}
	}
	receipt.OK = true
	log.Printf("stale prompts %s: cutoff=%s candidates=%d settled=%d expired=%d unchanged=%d backup=%s",
		clientActionID, receipt.Cutoff, receipt.Candidates, receipt.Settled, receipt.Expired, receipt.Unchanged, receipt.BackupPath)
	return receipt
}

// settleStaleThread runs exact settlement for one thread: through its live
// session when attached, otherwise against the thread's saved transcript. A
// thread the catalog does not hold has nothing to prove its prompts by; any
// other error leaves the thread for a later run.
func (s *server) settleStaleThread(ctx context.Context, hosted string, records []prompts.Record) error {
	var err error
	if sess := s.SessionForHostedThread(hosted); sess != nil {
		_, err = sess.SettlePrompts(ctx)
	} else {
		var all []prompts.Record
		if all, err = s.turns.List(ctx, hosted); err != nil {
			return err
		}
		// A provisional Claude binding stands for the thread's newest real
		// native thread, which a newer prompt than these may record.
		current := ""
		for _, record := range all {
			if record.NativeThreadID != "" && !sessions.ClaudeNativeIDProvisional(record.NativeThreadID) {
				current = record.NativeThreadID
			}
		}
		saved := func(ctx context.Context, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
			return s.savedPromptRows(ctx, current, keys)
		}
		_, err = sessions.SettleThreadPrompts(ctx, s.turns, saved, hosted, string(records[0].Provider), current)
	}
	if errors.Is(err, history.ErrSavedTranscriptUnavailable) {
		return nil
	}
	return err
}

// threadBusy reports whether the hosted thread's live session has a turn
// running or being restored, or a delivery in flight. It holds no lock across
// store work. A turn or attachment can still begin between this check and a
// prompt's expiry; that prompt is over a day old, so it is never the one being
// delivered, and expiry neither touches rows already shown nor hides a saved
// row, so no prompt the transcript holds is lost from view.
func (s *server) threadBusy(hosted string) bool {
	sess := s.SessionForHostedThread(hosted)
	if sess == nil {
		return false
	}
	return sess.BusyForMaintenance()
}

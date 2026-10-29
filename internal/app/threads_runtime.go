package app

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	state "github.com/dkroshow/nano/internal/app/statestore"
	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/edge/statewiring"
	hosted "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/threads"
)

type threadRuntimeStores struct {
	control    *controlMetadataStore
	executions *agentExecutionStore
	queue      *queuedTurnStore
	attempts   *sendAttemptStore
	completed  *completedTurnStore
	turns      *prompts.Store
}

// Open all hosted writers from the checked canonical file before any provider
// reattaches. A partial migration or failed constructor exposes no writers.
func openThreadRuntimeStores(ctx context.Context, directory, legacyCompleted, runID string) (*threadRuntimeStores, error) {
	return openThreadRuntimeStoresAt(ctx, state.StorePaths{
		Destination: filepath.Join(directory, "nano_threads.sqlite"), Control: filepath.Join(directory, "control.sqlite"),
		BackupRoot: filepath.Join(directory, "threads-backups"),
		Turns:      prompts.LegacyTurnPaths{Queue: filepath.Join(directory, "queued-turns.json"), Attempts: filepath.Join(directory, "send-attempts.json"), Completed: legacyCompleted},
	}, runID)
}

func openThreadRuntimeStoresAt(ctx context.Context, paths state.StorePaths, runID string) (*threadRuntimeStores, error) {
	if _, err := state.PrepareStore(ctx, paths, statewiring.Migrations()); err != nil {
		return nil, fmt.Errorf("prepare threads store: %w", err)
	}
	control, err := openControlMetadataStore(paths.Destination)
	if err != nil {
		return nil, err
	}
	result := &threadRuntimeStores{control: control, completed: hosted.NewCompletedTurnStore(control.CanonicalDatabase())}
	failed := true
	defer func() {
		if failed {
			_ = control.Close()
		}
	}()
	if result.turns, err = prompts.OpenStore(ctx, control.CanonicalDatabase()); err != nil {
		return nil, err
	}
	adoption, err := result.turns.AdoptLegacy(ctx)
	if err != nil {
		return nil, fmt.Errorf("adopt legacy turn records: %w", err)
	}
	if adoption.Adopted > 0 || adoption.Unmanaged > 0 {
		log.Printf("transactional turns adopted=%d uncertain=%d unmanaged=%d", adoption.Adopted, adoption.Uncertain, adoption.Unmanaged)
	}
	bootParks, err := result.turns.ParkIncompleteAtBoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("park incomplete turn records: %w", err)
	}
	for _, parked := range bootParks {
		log.Printf("transactional turn boot parked action=%q provider=%q hostedThreadId=%q nativeThreadId=%q missing=%s",
			parked.ActionID, parked.Provider, parked.HostedThreadID, parked.NativeThreadID, strings.Join(parked.Missing, ","))
	}
	if result.queue, err = prompts.NewLegacyQueue(control.CanonicalDatabase(), paths.Destination, result.turns); err != nil {
		return nil, err
	}
	result.queue.SetBootParks(bootParks)
	if result.attempts, err = prompts.NewLegacyJournal(control.CanonicalDatabase(), paths.Destination, runID, result.turns); err != nil {
		return nil, err
	}
	if result.executions, err = newControlAgentExecutionStore(control, runID); err != nil {
		return nil, err
	}
	// Retention keeps every operation until the prompt owner can say whose
	// evidence may go; from here on it asks.
	result.executions.SetPruneEligible(result.turns.PruneEligible)
	failed = false
	return result, nil
}

// Back up the canonical database the two compatibility views read from.
func backupSubmissionStore(ctx context.Context, queue *queuedTurnStore, attempts *sendAttemptStore, destination string) error {
	if queue == nil || attempts == nil || queue.Database() == nil || attempts.Database() == nil || queue.Path() == "" || queue.Path() != attempts.Path() {
		return fmt.Errorf("submission stores do not share the canonical database")
	}
	return threads.Backup(ctx, queue.Database(), destination)
}

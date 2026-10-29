package stateopen

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/edge/statewiring"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	state "github.com/dkroshow/nano/internal/state/store"
)

// RuntimeStores holds the owners opened from one checked canonical database.
type RuntimeStores struct {
	Control    *hostedstore.MetadataStore
	Executions *hostedstore.ExecutionStore
	Queue      *prompts.LegacyQueue
	Attempts   *prompts.LegacyJournal
	Completed  *hostedmodel.CompletedTurnStore
	Turns      *prompts.Store
}

// OpenRuntimeStoresAt completes migration and opens all owners before attachment starts.
func OpenRuntimeStoresAt(ctx context.Context, paths state.StorePaths, runID string) (*RuntimeStores, error) {
	if _, err := state.PrepareStore(ctx, paths, statewiring.Migrations()); err != nil {
		return nil, fmt.Errorf("prepare threads store: %w", err)
	}
	control, err := openMetadata(paths.Destination)
	if err != nil {
		return nil, err
	}
	result := &RuntimeStores{Control: control, Completed: hostedmodel.NewCompletedTurnStore(control.CanonicalDatabase())}
	failed := true
	defer func() {
		if failed {
			_ = control.Close()
		}
	}()
	if result.Turns, err = prompts.OpenStore(ctx, control.CanonicalDatabase()); err != nil {
		return nil, err
	}
	adoption, err := result.Turns.AdoptLegacy(ctx)
	if err != nil {
		return nil, fmt.Errorf("adopt legacy turn records: %w", err)
	}
	if adoption.Adopted > 0 || adoption.Unmanaged > 0 {
		log.Printf("transactional turns adopted=%d uncertain=%d unmanaged=%d", adoption.Adopted, adoption.Uncertain, adoption.Unmanaged)
	}
	bootParks, err := result.Turns.ParkIncompleteAtBoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("park incomplete turn records: %w", err)
	}
	for _, parked := range bootParks {
		log.Printf("transactional turn boot parked action=%q provider=%q hostedThreadId=%q nativeThreadId=%q missing=%s",
			parked.ActionID, parked.Provider, parked.HostedThreadID, parked.NativeThreadID, strings.Join(parked.Missing, ","))
	}
	if result.Queue, err = prompts.NewLegacyQueue(control.CanonicalDatabase(), paths.Destination, result.Turns); err != nil {
		return nil, err
	}
	result.Queue.SetBootParks(bootParks)
	if result.Attempts, err = prompts.NewLegacyJournal(control.CanonicalDatabase(), paths.Destination, runID, result.Turns); err != nil {
		return nil, err
	}
	if result.Executions, err = hostedstore.NewExecutionStore(control, runID, subscriptions.IsFeatureAction); err != nil {
		return nil, err
	}
	// Retention keeps every operation until the prompt owner can say whose
	// evidence may go; from here on it asks.
	result.Executions.SetPruneEligible(result.Turns.PruneEligible)
	failed = false
	return result, nil
}

package app

import (
	turns "github.com/dkroshow/nano/internal/core/prompts"
)

const (
	submissionQueued     = string(turns.Queued)
	submissionDelivering = string(turns.Delivering)
	submissionAccepted   = string(turns.Accepted)
	submissionUncertain  = string(turns.Uncertain)
	submissionParked     = string(turns.Parked)
)

// queuedTurnRecord is the schema-20 compatibility shape exposed by the read
// proxy below. Production delivery writes only through internal/core/prompts.Store.
// HostedThreadID is the stable pre-native identity; migrated records may carry
// only NativeThreadID.
type queuedTurnRecord = turns.QueuedTurn

const (
	turnOriginHuman  = string(turns.HumanOrigin)
	turnOriginAgent  = string(turns.AgentOrigin)
	turnOriginSystem = string(turns.SystemOrigin)
)

// queuedTurnStore is the prompt owner's compatibility read view.
type queuedTurnStore = turns.LegacyQueue

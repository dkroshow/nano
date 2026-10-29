package sessions

import (
	promptsmodel "github.com/dkroshow/nano/internal/core/prompts"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	liveviewmodel "github.com/dkroshow/nano/internal/liveview"
	observabilitymodel "github.com/dkroshow/nano/internal/observability"
	threadsmodel "github.com/dkroshow/nano/internal/threads"
)

const agentTurnCompleted = threadsmodel.AgentTurnCompleted
const agentTurnDeliveryUnknown = threadsmodel.AgentTurnDeliveryUnknown
const agentTurnFailed = threadsmodel.AgentTurnFailed
const agentTurnInterrupted = threadsmodel.AgentTurnInterrupted
const agentTurnRunning = threadsmodel.AgentTurnRunning
const conversationPageMaxRows = liveviewmodel.PageMaxRows
const conversationViewFull liveviewmodel.View = liveviewmodel.Full
const conversationViewMessages liveviewmodel.View = liveviewmodel.Messages
const liveProjectionStateTurnCap = liveviewmodel.StateTurnCap
const liveProjectionTurnTail = liveviewmodel.TurnTail
const liveProjectionVersion = liveviewmodel.Version
const metadataDispositionCleared = hostedstore.DispositionCleared
const metadataDispositionConfirmed = hostedstore.DispositionConfirmed
const metadataDispositionRejected = hostedstore.DispositionRejected
const metadataDispositionRequested = hostedstore.DispositionRequested
const metadataFreshnessLive = hostedstore.FreshnessLive
const metadataFreshnessReplayed = hostedstore.FreshnessReplayed
const presentationShared = threadsmodel.PresentationShared
const projectionSnapshot = liveviewmodel.SnapshotOp
const restartAttached = observabilitymodel.RestartAttached
const restartLive = observabilitymodel.RestartLive
const restartRegistered = observabilitymodel.RestartRegistered
const submissionParked = string(promptsmodel.Parked)
const submissionQueued = string(promptsmodel.Queued)
const transcriptFirstPaintRows = liveviewmodel.FirstPaintRows
const turnOriginHuman = string(promptsmodel.HumanOrigin)

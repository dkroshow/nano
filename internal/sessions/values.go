package sessions

import (
	promptsmodel "github.com/dkroshow/nano/internal/core/prompts"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	liveviewmodel "github.com/dkroshow/nano/internal/liveview"
	observabilitymodel "github.com/dkroshow/nano/internal/observability"
	threadsmodel "github.com/dkroshow/nano/internal/threads"
)

type agentExecutionError = threadsmodel.ExecutionError
type agentResultMeta = hostedstore.ResultMeta
type agentResumedTurnMatch = hostedstore.ResumedTurnMatch
type agentSessionRecord = threadsmodel.SessionRecord
type agentTerminalTurn = hostedstore.TerminalTurn
type agentTurnRecord = threadsmodel.TurnRecord
type attachmentRecord = hostedstore.Attachment
type conversationView = liveviewmodel.View
type durableTurnEvidence = liveviewmodel.DurableTurnEvidence
type hostedAttachmentView = hostedstore.AttachmentView
type hostedMetadataName = hostedstore.MetadataName
type hostedMetadataView = hostedstore.MetadataView
type liveProjectionItem = liveviewmodel.Item
type liveProjectionPage = liveviewmodel.Page
type liveProjectionRuntime = liveviewmodel.Runtime
type liveProjectionSnapshot = liveviewmodel.Snapshot
type liveProjectionTurn = liveviewmodel.Turn
type liveProjectionUpdate = liveviewmodel.Update
type liveTextStatus = liveviewmodel.Status
type metadataEvent = hostedstore.MetadataEvent
type metadataEventFilter = hostedstore.MetadataEventFilter
type metadataEventPage = hostedstore.MetadataEventPage
type nativeResolution = hostedstore.NativeResolution
type queuedTurnRecord = promptsmodel.QueuedTurn
type threadDiagnostic = observabilitymodel.ThreadDiagnostic

const agentTurnCompleted = threadsmodel.AgentTurnCompleted
const agentTurnDeliveryUnknown = threadsmodel.AgentTurnDeliveryUnknown
const agentTurnFailed = threadsmodel.AgentTurnFailed
const agentTurnInterrupted = threadsmodel.AgentTurnInterrupted
const agentTurnRunning = threadsmodel.AgentTurnRunning
const conversationPageMaxRows = liveviewmodel.PageMaxRows
const conversationViewFull conversationView = liveviewmodel.Full
const conversationViewMessages conversationView = liveviewmodel.Messages
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

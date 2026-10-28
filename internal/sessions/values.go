package sessions

import (
	promptsmodel "github.com/dkroshow/nano/internal/core/prompts"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	liveviewmodel "github.com/dkroshow/nano/internal/liveview"
	observabilitymodel "github.com/dkroshow/nano/internal/observability"
	threadsmodel "github.com/dkroshow/nano/internal/threads"
)

type agentExecutionError = threadsmodel.ExecutionError
type agentResultMeta = hostedmodel.ResultMeta
type agentResumedTurnMatch = hostedmodel.ResumedTurnMatch
type agentSessionRecord = threadsmodel.SessionRecord
type agentTerminalTurn = hostedmodel.TerminalTurn
type agentTurnRecord = threadsmodel.TurnRecord
type attachmentRecord = hostedmodel.Attachment
type conversationView = liveviewmodel.View
type durableTurnEvidence = liveviewmodel.DurableTurnEvidence
type hostedAttachmentView = hostedmodel.AttachmentView
type hostedMetadataName = hostedmodel.MetadataName
type hostedMetadataView = hostedmodel.MetadataView
type liveProjectionItem = liveviewmodel.Item
type liveProjectionPage = liveviewmodel.Page
type liveProjectionRuntime = liveviewmodel.Runtime
type liveProjectionSnapshot = liveviewmodel.Snapshot
type liveProjectionTurn = liveviewmodel.Turn
type liveProjectionUpdate = liveviewmodel.Update
type liveTextStatus = liveviewmodel.Status
type metadataEvent = hostedmodel.MetadataEvent
type metadataEventFilter = hostedmodel.MetadataEventFilter
type metadataEventPage = hostedmodel.MetadataEventPage
type nativeResolution = hostedmodel.NativeResolution
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
const metadataDispositionCleared = hostedmodel.DispositionCleared
const metadataDispositionConfirmed = hostedmodel.DispositionConfirmed
const metadataDispositionRejected = hostedmodel.DispositionRejected
const metadataDispositionRequested = hostedmodel.DispositionRequested
const metadataFreshnessLive = hostedmodel.FreshnessLive
const metadataFreshnessReplayed = hostedmodel.FreshnessReplayed
const presentationShared = threadsmodel.PresentationShared
const projectionSnapshot = liveviewmodel.SnapshotOp
const restartAttached = observabilitymodel.RestartAttached
const restartLive = observabilitymodel.RestartLive
const restartRegistered = observabilitymodel.RestartRegistered
const submissionParked = string(promptsmodel.Parked)
const submissionQueued = string(promptsmodel.Queued)
const transcriptFirstPaintRows = liveviewmodel.FirstPaintRows
const turnOriginHuman = string(promptsmodel.HumanOrigin)

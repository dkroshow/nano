package app

import (
	turns "github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/threads"
)

type completedTurn = coreturns.Record
type agentExecutionError = threads.ExecutionError
type agentTerminalTurn = hosted.TerminalTurn
type agentResultMeta = hosted.ResultMeta
type orchestrationAuditTarget = hosted.AuditTarget
type metadataEvent = hosted.MetadataEvent
type metadataEventPage = hosted.MetadataEventPage
type metadataEventFilter = hosted.MetadataEventFilter
type nativeResolution = hosted.NativeResolution

const presentationShared = threads.PresentationShared
const presentationBackground = threads.PresentationBackground
const sidebarPlacementInvoker = threads.SidebarPlacementInvoker
const agentSessionDetached = threads.AgentSessionDetached
const agentTurnQueued = threads.AgentTurnQueued
const agentTurnDeliveryPending = threads.AgentTurnDeliveryPending
const agentTurnDeliveryUnknown = threads.AgentTurnDeliveryUnknown
const agentTurnRunning = threads.AgentTurnRunning
const agentTurnWaiting = threads.AgentTurnWaiting
const agentTurnAwaitingSaved = threads.AgentTurnAwaitingSaved
const agentTurnFailed = threads.AgentTurnFailed
const agentTurnInterrupted = threads.AgentTurnInterrupted
const metadataDispositionRequested = hosted.DispositionRequested
const metadataDispositionConfirmed = hosted.DispositionConfirmed
const metadataDispositionRejected = hosted.DispositionRejected
const metadataDispositionCleared = hosted.DispositionCleared
const metadataDispositionObserved = hosted.DispositionObserved
const metadataFreshnessLive = hosted.FreshnessLive
const metadataFreshnessHistorical = hosted.FreshnessHistorical
const metadataFreshnessReplayed = hosted.FreshnessReplayed
const metadataSourceMigration = hosted.SourceMigration
const submissionQueued = string(turns.Queued)
const submissionDelivering = string(turns.Delivering)
const submissionAccepted = string(turns.Accepted)
const submissionUncertain = string(turns.Uncertain)
const submissionParked = string(turns.Parked)

type queuedTurnRecord = turns.QueuedTurn

const turnOriginHuman = string(turns.HumanOrigin)
const turnOriginSystem = string(turns.SystemOrigin)

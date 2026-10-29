package app

import (
	"github.com/dkroshow/nano/internal/core/prompts"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/threads"
)

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
const metadataDispositionRequested = hostedstore.DispositionRequested
const metadataDispositionConfirmed = hostedstore.DispositionConfirmed
const metadataDispositionRejected = hostedstore.DispositionRejected
const metadataDispositionCleared = hostedstore.DispositionCleared
const metadataDispositionObserved = hostedstore.DispositionObserved
const metadataFreshnessLive = hostedstore.FreshnessLive
const metadataFreshnessHistorical = hostedstore.FreshnessHistorical
const metadataFreshnessReplayed = hostedstore.FreshnessReplayed
const metadataSourceMigration = hostedstore.SourceMigration
const submissionQueued = string(prompts.Queued)
const submissionDelivering = string(prompts.Delivering)
const submissionAccepted = string(prompts.Accepted)
const submissionUncertain = string(prompts.Uncertain)
const submissionParked = string(prompts.Parked)

const turnOriginHuman = string(prompts.HumanOrigin)
const turnOriginSystem = string(prompts.SystemOrigin)

const (
	sidebarPlacementStandalone = threads.SidebarPlacementStandalone
	agentTurnCompleted         = threads.AgentTurnCompleted
)

const (
	turnOriginAgent = string(prompts.AgentOrigin)
)

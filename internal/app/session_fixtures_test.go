package app

import (
	timemodel "time"

	"github.com/dkroshow/nano/internal/core/prompts"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	interactmodel "github.com/dkroshow/nano/internal/interact"
	protocolmodel "github.com/dkroshow/nano/internal/protocol"
	"github.com/dkroshow/nano/internal/sessions"
)

// These test-local names refer to the real session boundary. They carry no
// independent implementation or runtime state.

var addSavedUserKey = sessions.AddSavedUserKey

type attachedSourceCatalog = sessions.AttachedCatalog
type bootProviderControl = sessions.BootControl

var cancelQueuedTurnStores = sessions.CancelQueuedTurnStores

type claudeAnswer = sessions.ClaudeAnswer
type claudeAnswers = sessions.ClaudeAnswers

var claudeArgs = sessions.ClaudeArgs
var claudeCanonicalResumeArgs = sessions.ClaudeCanonicalResumeArgs
var claudeCommand = sessions.ClaudeCommand
var claudeExecutionEvidence = sessions.ClaudeExecutionEvidence

const claudeMemberBatch = sessions.ClaudeMemberBatch

var claudeNativeIDProvisional = sessions.ClaudeNativeIDProvisional
var claudeProjectionTerminal = sessions.ClaudeProjectionTerminal
var claudeSettlementEntry = sessions.ClaudeSettlementEntry
var closeDetachedClaude = sessions.CloseDetachedClaude

type codexDaemon = sessions.CodexDaemon

var codexResumeTurnTerminal = sessions.CodexResumeTurnTerminal
var codexSessionRequest = sessions.CodexSessionRequest
var codexTurnOverrides = sessions.CodexTurnOverrides

type deliveryExpiries = sessions.DeliveryExpiries

const discardedByCaller = sessions.DiscardedByCaller

var errPromptEnded = sessions.ErrPromptEnded
var errQueuedTurnDelivering = sessions.ErrQueuedTurnDelivering
var errQueuedTurnNotFound = sessions.ErrQueuedTurnNotFound
var exactClaudeNative = sessions.ExactClaudeNative
var fetchGrokModels = sessions.FetchGrokModels
var grokCommand = sessions.GrokCommand

const grokJSONRPCVersion = sessions.GrokJSONRPCVersion

var grokProbeCwd = sessions.GrokProbeCwd
var grokProcessEnv = sessions.GrokProcessEnv

const grokProtocolVersion = sessions.GrokProtocolVersion

var grokSessionRequest = sessions.GrokSessionRequest

var nativeThreadIDProvisional = sessions.NativeThreadIDProvisional
var newBootProviderControl = sessions.NewBootControl
var newClaudeAnswers = sessions.NewClaudeAnswers

// The scripted provider owns this test process, so a lost connection must not start the installed CLI.
func newCodexDaemon(url, label string) *codexDaemon {
	return sessions.NewCodexDaemon(url, label, func() bool { return true })
}

var newDeliveryExpiries = sessions.NewDeliveryExpiries
var newSession = sessions.New
var newSessionID = sessions.NewID
var newSessionRegistry = sessions.NewRegistry
var prepareWireSpoolDir = sessions.PrepareWireSpoolDir

type promptActionRefused = sessions.PromptActionRefused

var promptPresentation = sessions.PromptPresentation
var providerLifecycleCapabilities = sessions.ProviderLifecycleCapabilities

type providerRequest = sessions.ProviderRequest

var resolveClaudeBin = sessions.ResolveClaudeBin
var resolveGrokBin = sessions.ResolveGrokBin

type savedPromptReader = sessions.SavedPromptReader

var savedRowsGeneration = sessions.SavedRowsGeneration
var savedRowsOf = sessions.SavedRowsOf
var sendTextHash = sessions.SendTextHash

type session = sessions.Session
type sessionAvailability = sessions.Availability
type sessionConfig = sessions.Config
type sessionExecutions = sessions.Executions
type sessionJournal = sessions.Journal
type sessionLiveText = sessions.LiveTextLoop
type sessionMetadata = sessions.Metadata
type sessionQueue = sessions.Queue
type sessionRegistry = sessions.Registry

const sessionStatusExited = sessions.StatusExited

type sessionSummary = sessions.Summary

var settleThreadPrompts = sessions.SettleThreadPrompts
var slowDeliveryAt = sessions.SlowDeliveryAt
var submissionEventID = sessions.SubmissionEventID

type threadAttachedError = sessions.ThreadAttachedError

var turnRequestHash = sessions.TurnRequestHash

type pendingInteraction = interactmodel.PendingInteraction
type permissionEvent = hostedstore.PermissionEvent
type permissionEventPage = hostedstore.PermissionEventPage

const postureFieldEffort = hostedstore.PostureFieldEffort
const postureFieldModel = hostedstore.PostureFieldModel
const postureFieldPermissionMode = hostedstore.PostureFieldPermissionMode
const postureFieldServiceTier = hostedstore.PostureFieldServiceTier

type postureFieldValue = hostedstore.FieldValue

const postureSourceCreation = hostedstore.PostureSourceCreation
const postureSourceOperator = hostedstore.PostureSourceOperator
const postureSourceProvider = hostedstore.PostureSourceProvider
const savedPromptSettleTimeout = 10 * timemodel.Second

type sendAttemptRecord = prompts.SendAttempt
type sessionPosture = hostedstore.Posture

type fixturePromptView = prompts.View

type wireEvent = protocolmodel.Event

// Expected Claude CLI spelling at the application spawn boundary.
const claudeIdentityFlag = "--append-system-prompt"

type transcriptRepairResponse = sessions.TranscriptRepairResponse

type postureEntry = hostedstore.PostureEntry

const postureEvidenceSpawnRequest = hostedstore.PostureEvidenceSpawnRequest
const postureEvidenceSetModel = hostedstore.PostureEvidenceSetModel

const permissionEventRequested = hostedstore.PermissionEventRequested
const permissionEventResolved = hostedstore.PermissionEventResolved

// These are the expected action names and deadline in the public prompt contract.
const promptResend = "resend"
const promptDiscard = "discard"
const promptSlowDelivery = 5 * timemodel.Second
const kindToolPermission = "toolPermission"
const decisionAccept = "accept"

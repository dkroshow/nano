package app

// These test-local names refer to the real session boundary. They carry no
// independent implementation or runtime state.
import (
	promptsmodel "github.com/dkroshow/nano/internal/core/prompts"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	interactmodel "github.com/dkroshow/nano/internal/interact"
	protocolmodel "github.com/dkroshow/nano/internal/protocol"
	sessionruntime "github.com/dkroshow/nano/internal/sessions"
	timemodel "time"
)

var addSavedUserKey = sessionruntime.AddSavedUserKey

type attachedSourceCatalog = sessionruntime.AttachedCatalog
type bootProviderControl = sessionruntime.BootControl

var cancelQueuedTurnStores = sessionruntime.CancelQueuedTurnStores

type claudeAnswer = sessionruntime.ClaudeAnswer
type claudeAnswers = sessionruntime.ClaudeAnswers

var claudeArgs = sessionruntime.ClaudeArgs
var claudeCanonicalResumeArgs = sessionruntime.ClaudeCanonicalResumeArgs
var claudeCommand = sessionruntime.ClaudeCommand
var claudeExecutionEvidence = sessionruntime.ClaudeExecutionEvidence

const claudeMemberBatch = sessionruntime.ClaudeMemberBatch

var claudeNativeIDProvisional = sessionruntime.ClaudeNativeIDProvisional
var claudeProjectionTerminal = sessionruntime.ClaudeProjectionTerminal
var claudeSettlementEntry = sessionruntime.ClaudeSettlementEntry
var closeDetachedClaude = sessionruntime.CloseDetachedClaude

type codexDaemon = sessionruntime.CodexDaemon

var codexResumeTurnTerminal = sessionruntime.CodexResumeTurnTerminal
var codexSessionRequest = sessionruntime.CodexSessionRequest
var codexTurnOverrides = sessionruntime.CodexTurnOverrides

type deliveryExpiries = sessionruntime.DeliveryExpiries

const discardedByCaller = sessionruntime.DiscardedByCaller

var errPromptEnded = sessionruntime.ErrPromptEnded
var errQueuedTurnDelivering = sessionruntime.ErrQueuedTurnDelivering
var errQueuedTurnNotFound = sessionruntime.ErrQueuedTurnNotFound
var exactClaudeNative = sessionruntime.ExactClaudeNative
var fetchGrokModels = sessionruntime.FetchGrokModels
var grokCommand = sessionruntime.GrokCommand

const grokJSONRPCVersion = sessionruntime.GrokJSONRPCVersion

var grokProbeCwd = sessionruntime.GrokProbeCwd
var grokProcessEnv = sessionruntime.GrokProcessEnv

const grokProtocolVersion = sessionruntime.GrokProtocolVersion

var grokSessionRequest = sessionruntime.GrokSessionRequest

type liveTextObserver = sessionruntime.LiveTextObserver

var nativeThreadIDProvisional = sessionruntime.NativeThreadIDProvisional
var newBootProviderControl = sessionruntime.NewBootControl
var newClaudeAnswers = sessionruntime.NewClaudeAnswers

// The scripted provider owns this test process, so a lost connection must not start the installed CLI.
func newCodexDaemon(url, label string) *codexDaemon {
	return sessionruntime.NewCodexDaemon(url, label, func() bool { return true })
}

var newDeliveryExpiries = sessionruntime.NewDeliveryExpiries
var newSession = sessionruntime.New
var newSessionID = sessionruntime.NewID
var newSessionRegistry = sessionruntime.NewRegistry
var prepareWireSpoolDir = sessionruntime.PrepareWireSpoolDir

type promptActionRefused = sessionruntime.PromptActionRefused

var promptPresentation = sessionruntime.PromptPresentation
var providerLifecycleCapabilities = sessionruntime.ProviderLifecycleCapabilities

type providerRequest = sessionruntime.ProviderRequest

var resolveClaudeBin = sessionruntime.ResolveClaudeBin
var resolveGrokBin = sessionruntime.ResolveGrokBin

type savedPromptReader = sessionruntime.SavedPromptReader

var savedRowsGeneration = sessionruntime.SavedRowsGeneration
var savedRowsOf = sessionruntime.SavedRowsOf
var sendTextHash = sessionruntime.SendTextHash

type session = sessionruntime.Session
type sessionAvailability = sessionruntime.Availability
type sessionConfig = sessionruntime.Config
type sessionExecutions = sessionruntime.Executions
type sessionJournal = sessionruntime.Journal
type sessionLiveText = sessionruntime.LiveText
type sessionMetadata = sessionruntime.Metadata
type sessionPane = sessionruntime.Pane
type sessionProjection = sessionruntime.Projection
type sessionQueue = sessionruntime.Queue
type sessionRegistry = sessionruntime.Registry

const sessionStatusExited = sessionruntime.StatusExited

type sessionSummary = sessionruntime.Summary

var settleThreadPrompts = sessionruntime.SettleThreadPrompts
var slowDeliveryAt = sessionruntime.SlowDeliveryAt
var submissionEventID = sessionruntime.SubmissionEventID

type threadAttachedError = sessionruntime.ThreadAttachedError

var turnRequestHash = sessionruntime.TurnRequestHash

type pendingInteraction = interactmodel.PendingInteraction
type permissionEvent = hostedmodel.PermissionEvent
type permissionEventPage = hostedmodel.PermissionEventPage

const postureFieldEffort = hostedmodel.PostureFieldEffort
const postureFieldModel = hostedmodel.PostureFieldModel
const postureFieldPermissionMode = hostedmodel.PostureFieldPermissionMode
const postureFieldServiceTier = hostedmodel.PostureFieldServiceTier

type postureFieldValue = hostedmodel.FieldValue

const postureSourceCreation = hostedmodel.PostureSourceCreation
const postureSourceOperator = hostedmodel.PostureSourceOperator
const postureSourceProvider = hostedmodel.PostureSourceProvider
const savedPromptSettleTimeout = 10 * timemodel.Second

type sendAttemptRecord = promptsmodel.SendAttempt
type sessionPosture = hostedmodel.Posture

type fixturePromptView = promptsmodel.View

type wireEvent = protocolmodel.Event

// Expected Claude CLI spelling at the application spawn boundary.
const claudeIdentityFlag = "--append-system-prompt"

type transcriptRepairResponse = sessionruntime.TranscriptRepairResponse

type postureEntry = hostedmodel.PostureEntry

const postureEvidenceSpawnRequest = hostedmodel.PostureEvidenceSpawnRequest
const postureEvidenceSetModel = hostedmodel.PostureEvidenceSetModel

const permissionEventRequested = hostedmodel.PermissionEventRequested
const permissionEventResolved = hostedmodel.PermissionEventResolved

// These are the expected action names and deadline in the public prompt contract.
const promptResend = "resend"
const promptDiscard = "discard"
const promptSlowDelivery = 5 * timemodel.Second
const kindToolPermission = "toolPermission"
const decisionAccept = "accept"

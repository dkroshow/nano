package app

import (
	timemodel "time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
)

// These test-local names refer to the real session boundary. They carry no
// independent implementation or runtime state.

var addSavedUserKey = sessions.AddSavedUserKey

var cancelQueuedTurnStores = sessions.CancelQueuedTurnStores

var claudeArgs = sessions.ClaudeArgs
var claudeCanonicalResumeArgs = sessions.ClaudeCanonicalResumeArgs
var claudeCommand = sessions.ClaudeCommand
var claudeExecutionEvidence = sessions.ClaudeExecutionEvidence

const claudeMemberBatch = sessions.ClaudeMemberBatch

var claudeNativeIDProvisional = sessions.ClaudeNativeIDProvisional
var claudeProjectionTerminal = sessions.ClaudeProjectionTerminal
var claudeSettlementEntry = sessions.ClaudeSettlementEntry
var closeDetachedClaude = sessions.CloseDetachedClaude

var codexResumeTurnTerminal = sessions.CodexResumeTurnTerminal
var codexSessionRequest = sessions.CodexSessionRequest
var codexTurnOverrides = sessions.CodexTurnOverrides

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
func newCodexDaemon(url, label string) *sessions.CodexDaemon {
	return sessions.NewCodexDaemon(url, label, func() bool { return true })
}

var newDeliveryExpiries = sessions.NewDeliveryExpiries
var newSession = sessions.New
var newSessionID = sessions.NewID
var newSessionRegistry = sessions.NewRegistry
var prepareWireSpoolDir = sessions.PrepareWireSpoolDir

var promptPresentation = sessions.PromptPresentation
var providerLifecycleCapabilities = sessions.ProviderLifecycleCapabilities

var resolveClaudeBin = sessions.ResolveClaudeBin
var resolveGrokBin = sessions.ResolveGrokBin

var savedRowsGeneration = sessions.SavedRowsGeneration
var savedRowsOf = sessions.SavedRowsOf
var sendTextHash = sessions.SendTextHash

const sessionStatusExited = sessions.StatusExited

var settleThreadPrompts = sessions.SettleThreadPrompts
var slowDeliveryAt = sessions.SlowDeliveryAt
var submissionEventID = sessions.SubmissionEventID

var turnRequestHash = sessions.TurnRequestHash

const postureFieldEffort = hostedstore.PostureFieldEffort
const postureFieldModel = hostedstore.PostureFieldModel
const postureFieldPermissionMode = hostedstore.PostureFieldPermissionMode
const postureFieldServiceTier = hostedstore.PostureFieldServiceTier

const postureSourceCreation = hostedstore.PostureSourceCreation
const postureSourceOperator = hostedstore.PostureSourceOperator
const postureSourceProvider = hostedstore.PostureSourceProvider
const savedPromptSettleTimeout = 10 * timemodel.Second

// Expected Claude CLI spelling at the application spawn boundary.
const claudeIdentityFlag = "--append-system-prompt"

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

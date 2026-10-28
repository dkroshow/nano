package sessions

import prompts "github.com/dkroshow/nano/internal/core/prompts"

type queuedTurnStore = prompts.LegacyQueue
type sendAttemptStore = prompts.LegacyJournal

const submissionAccepted = string(prompts.Accepted)
const submissionDelivering = string(prompts.Delivering)
const submissionUncertain = string(prompts.Uncertain)

var sendTextHash = SendTextHash
var promptPresentation = PromptPresentation

type session = Session

type providerRequest = ProviderRequest

var claudeArgs = ClaudeArgs
var submissionEventID = SubmissionEventID

type codexDaemon = CodexDaemon

func newCodexDaemon(url, label string) *CodexDaemon {
	return NewCodexDaemon(url, label, func() bool { return true })
}

type bootProviderControl = BootControl

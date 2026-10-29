package sessions

import (
	prompts "github.com/dkroshow/nano/internal/core/prompts"
)

const submissionAccepted = string(prompts.Accepted)
const submissionDelivering = string(prompts.Delivering)
const submissionUncertain = string(prompts.Uncertain)

var sendTextHash = SendTextHash
var promptPresentation = PromptPresentation

var claudeArgs = ClaudeArgs
var submissionEventID = SubmissionEventID

func newCodexDaemon(url, label string) *CodexDaemon {
	return NewCodexDaemon(url, label, func() bool { return true })
}

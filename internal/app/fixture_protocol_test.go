package app

import (
	"github.com/dkroshow/nano/internal/config"
	prompts "github.com/dkroshow/nano/internal/core/prompts"
	shim "github.com/dkroshow/nano/internal/wire/claude"
)

const supervisedDaemonLabel = config.DefaultCodexSupervisorLabel
const (
	shimMethodStart         = shim.MethodStart
	shimMethodAttach        = shim.MethodAttach
	shimMethodInput         = shim.MethodInput
	shimMethodClose         = shim.MethodClose
	shimMethodLine          = shim.MethodLine
	shimMethodExited        = shim.MethodExited
	shimMethodReplayDone    = shim.MethodReplayDone
	shimCodeSessionAttached = shim.CodeSessionAttached
	shimCodeSessionNotFound = shim.CodeSessionNotFound
	shimCodeSessionExited   = shim.CodeSessionExited
	sendAttemptQueued       = string(prompts.JournalQueued)
	sendAttemptSending      = string(prompts.JournalSending)
	sendAttemptAccepted     = string(prompts.JournalAccepted)
	sendAttemptFailed       = string(prompts.JournalFailed)
	sendAttemptRefused      = string(prompts.JournalRefused)
)

// Expected provider CLI options in application integration fixtures.
const claudeModelFlag = "--model"
const claudeResumeFlag = "--resume"

const sendAttemptTimeout = "timeout" // Imported schema-20 journal spelling.

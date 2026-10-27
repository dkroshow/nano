package main

import (
	"encoding/json"

	"github.com/dkroshow/nano/internal/history"

	"github.com/google/uuid"
)

const submissionIDNamespace = history.LegacyNanoIdentity

// submissionEventID is the submission identity Nano
// assigns before delivery. It is deterministic so retrying one durable action
// reuses its UUID, while separate actions (including equal-text prompts) do
// not. The length-safe JSON tuple prevents caller-controlled separators from
// creating the same name.
func submissionEventID(hostedThreadID, actionID string) string {
	if hostedThreadID == "" || actionID == "" {
		return ""
	}
	name, _ := json.Marshal([]string{submissionIDNamespace, "claude-user", hostedThreadID, actionID})
	return uuid.NewSHA1(uuid.NameSpaceOID, name).String()
}

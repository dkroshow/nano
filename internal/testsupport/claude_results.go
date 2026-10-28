package testsupport

import "embed"

//go:embed testdata/claude-result-binding/*
var claudeResults embed.FS

func ClaudeResultFixture(name string) ([]byte, error) {
	return claudeResults.ReadFile("testdata/claude-result-binding/" + name)
}

type ClaudeResultPromptIdentity struct{ Name, Turn string }

func ClaudeResultPromptIdentities() []ClaudeResultPromptIdentity {
	return []ClaudeResultPromptIdentity{
		{"A", "claude:hosted:36187"}, {"B", "claude:hosted:36195"}, {"C", "claude:hosted:36198"}, {"D", "claude:hosted:36201"}, {"E", "claude:hosted:36228"},
	}
}

const ClaudeResultAnswerA = "msg_945108b79d7659eba44f02ca7d8bf0be:0"
const ClaudeResultAnswerMerged = "msg_62a3a49eff7b5ff194d5d58d220ef8a4:0"

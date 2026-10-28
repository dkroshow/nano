package sessions

import fixture "github.com/dkroshow/nano/internal/testsupport/claudeshim"

var dataLines = fixture.DataLines
var waitForClaude = fixture.WaitFor
var waitForClaudeWithin = fixture.WaitForWithin

func claudeDataWireEvents(sess *session) []wireEvent {
	return fixture.DataEvents(claudeWireEvents(sess))
}

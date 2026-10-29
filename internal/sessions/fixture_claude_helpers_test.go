package sessions

import (
	"github.com/dkroshow/nano/internal/protocol"
	fixture "github.com/dkroshow/nano/internal/testsupport/claudeshim"
)

var dataLines = fixture.DataLines
var waitForClaude = fixture.WaitFor
var waitForClaudeWithin = fixture.WaitForWithin

func claudeDataWireEvents(sess *Session) []protocol.Event {
	return fixture.DataEvents(claudeWireEvents(sess))
}

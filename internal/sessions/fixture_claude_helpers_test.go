package sessions

import (
	fixture "github.com/dkroshow/nano/internal/testsupport/claudeshim"
	"github.com/dkroshow/nano/internal/wire"
)

var dataLines = fixture.DataLines
var waitForClaude = fixture.WaitFor
var waitForClaudeWithin = fixture.WaitForWithin

func claudeDataWireEvents(sess *Session) []wire.Event {
	return fixture.DataEvents(claudeWireEvents(sess))
}

package sessions

import (
	"fmt"
	"sync"
	"testing"
)

// A delivery's outcome and its prompt's echo arrive on different goroutines,
// the send and the wire reader. Each must read the shared wait only under the
// session's lock; under -race, an unlocked read fails here. Boundary: the two
// entry points the send and the wire reader call.
func TestCodexEchoAndDeliveryOutcomeMayArriveTogether(t *testing.T) {
	sess, _ := readyTransactionalSendTestSession(t, nil)
	sess.hostedThreadID = "hosted"
	for i := range 100 {
		action := fmt.Sprintf("race-%d", i)
		sess.expectCodexEcho(codexEchoWait{actionID: action, claimToken: "claim", claimEpoch: 1})
		echo := `{"method":"item/started","params":{"turnId":"turn-race","item":{"type":"userMessage","clientId":"` + submissionEventID("hosted", action) + `"}}}`
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); sess.deliveredCodexEcho(action, true) }()
		go func() { defer wg.Done(); sess.observeCodexTurnInput([]byte(echo)) }()
		wg.Wait()
	}
}

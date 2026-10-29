package sessions

import (
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func TestCodexLiveReaderKeepsFailedOutcomeAndReceiptTime(t *testing.T) {
	p := newLiveProjection("codex")
	a := p.adapter.(*codexLiveAdapter)
	at := time.Unix(200, 0)
	p.applyWire(protocol.Recv, []byte(`{"method":"item/started","params":{"turnId":"turn","item":{"id":"cmd","type":"commandExecution","command":"printf TWFu"}}}`), at)
	p.applyWire(protocol.Recv, []byte(`{"method":"item/commandExecution/outputDelta","params":{"turnId":"turn","itemId":"cmd","delta":"TWFu"}}`), at)
	p.applyWire(protocol.Recv, []byte(`{"method":"item/completed","params":{"turnId":"turn","item":{"id":"cmd","type":"commandExecution","command":"printf TWFu","aggregatedOutput":"TWFu","status":"failed","exitCode":1}}}`), at)
	for _, parsed := range a.reader.Items {
		if parsed.NativeItemID == "cmd" {
			if !parsed.Complete || parsed.Outcome != model.OutcomeFailed || parsed.CreatedAt != nil {
				t.Fatal("live reader conflated completion, outcome or receipt time")
			}
			return
		}
	}
	t.Fatal("live reader lost command identity")
}

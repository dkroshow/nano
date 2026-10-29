package sessions

import (
	"testing"

	fixture "github.com/dkroshow/nano/internal/testsupport"
	"github.com/dkroshow/nano/internal/wire"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// While a provider compacts a turn's context, the pane's status and the
// rail's runtime both carry when it began, from the provider's own signals:
// Codex's contextCompaction item start (its startedAtMs) until the item
// completes; Claude's first "compacting" status line, kept through the
// repeats, until a status line without it. Claude's compacting is the turn's
// work, so it shows even when it comes before anything else in the turn. The
// turn's end clears it too. Boundary: Nano's wire ingress, on captured
// compaction lines.
func TestCompactingFollowsTheProvidersOwnSignals(t *testing.T) {
	codex, claude := fixture.CompactionFixture(t, "codex"), fixture.CompactionFixture(t, "claude")
	for _, test := range []struct {
		provider string
		start    []fixture.TimedLine // the turn's start, then compaction starting
		end      []fixture.TimedLine // compaction ending
		turnEnd  []byte
		since    int64
	}{
		{
			provider: "codex",
			start:    []fixture.TimedLine{{Line: []byte(`{"method":"turn/started","params":{"threadId":"16952569-d518-579a-914e-2c422f5d5946","turn":{"id":"893574cb-2552-53b8-bbd1-667ea32ac097","status":"inProgress"}}}`)}, codex[0]},
			end:      codex[1:],
			turnEnd:  []byte(`{"method":"turn/completed","params":{"threadId":"16952569-d518-579a-914e-2c422f5d5946","turn":{"id":"893574cb-2552-53b8-bbd1-667ea32ac097","status":"interrupted"}}}`),
			since:    1791514710947,
		},
		{
			provider: "claude",
			start:    claude[:4], // requesting, then three compacting lines 30 s apart
			end:      claude[4:],
			turnEnd:  []byte(`{"type":"result","subtype":"success","is_error":false,"duration_ms":1000}`),
			since:    claude[1].At.UnixMilli(),
		},
	} {
		t.Run(test.provider, func(t *testing.T) {
			sess := sessionWithOwners(&Session{provider: test.provider,
				lifecycleState: testLifecycleSnapshot("ready", false),
				projection:     newDeliveryTestProjection(test.provider),
				wire:           Wire{subs: map[chan wire.Event]struct{}{}}})
			sess.threadID = "16952569-d518-579a-914e-2c422f5d5946"
			apply := func(lines []fixture.TimedLine) {
				for _, l := range lines {
					sess.wire.recordWireAt(protocol.Recv, l.Line, true, false, l.At)
				}
			}
			shown := func() (pane, rail int64) {
				summary, _ := sess.volatileSummary(false)
				return sess.projection.status().Runtime.CompactingSinceMs, summary.CompactingSinceMs
			}
			apply(test.start)
			if pane, rail := shown(); pane != test.since || rail != test.since {
				t.Fatalf("compacting since: pane %d, rail %d; want %d", pane, rail, test.since)
			}
			apply(test.end)
			if pane, rail := shown(); pane != 0 || rail != 0 {
				t.Fatalf("after compaction ended: pane %d, rail %d; want none", pane, rail)
			}
			apply(test.start[len(test.start)-1:])
			if pane, _ := shown(); pane == 0 {
				t.Fatal("a second compaction did not show")
			}
			sess.wire.recordWire(protocol.Recv, test.turnEnd)
			if pane, rail := shown(); pane != 0 || rail != 0 {
				t.Fatalf("after the turn ended: pane %d, rail %d; want none", pane, rail)
			}
		})
	}
}

package sessions

import (
	"sync"

	"encoding/json"
	"log"
	"strings"
	"time"

	coresession "github.com/dkroshow/nano/internal/core/session"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/inspection"
	"github.com/dkroshow/nano/internal/wire"
)

// wireBufferCap bounds the raw events a session keeps in memory: the tail the
// Event Feed shows on connect. Older events page from the spool, which holds
// every event. At 20,000 the rings held about 500 MB across 450 sessions, 60%
// of the live heap, for a panel that is rarely open.
const wireBufferCap = 500

type queuedCodexDelta struct {
	method   string
	threadID string
	turnID   string
	itemID   string
	delta    string
	at       time.Time
	event    wire.Event
	wireSeq  []int64
}

type queuedCodexDeltaBatch struct {
	first  queuedCodexDelta
	delta  strings.Builder
	frames []queuedCodexDelta
}

func parseQueuedCodexDelta(line []byte) (queuedCodexDelta, bool) {
	var envelope struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &envelope) != nil ||
		(envelope.Method != "item/agentMessage/delta" && envelope.Method != "item/commandExecution/outputDelta") ||
		envelope.Params.ThreadID == "" || envelope.Params.TurnID == "" || envelope.Params.ItemID == "" {
		return queuedCodexDelta{}, false
	}
	return queuedCodexDelta{
		method:   envelope.Method,
		threadID: envelope.Params.ThreadID, turnID: envelope.Params.TurnID,
		itemID: envelope.Params.ItemID, delta: envelope.Params.Delta,
	}, true
}

func (batch *queuedCodexDeltaBatch) append(frame queuedCodexDelta) {
	batch.delta.WriteString(frame.delta)
	batch.frames = append(batch.frames, frame)
}

func (batch *queuedCodexDeltaBatch) mergedLine() ([]byte, error) {
	return json.Marshal(struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		} `json:"params"`
	}{
		Method: batch.first.method,
		Params: struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		}{ThreadID: batch.first.threadID, TurnID: batch.first.turnID, ItemID: batch.first.itemID, Delta: batch.delta.String()},
	})
}

func (attachmentState *State) catalogPathForNudge(line []byte) string {
	if attachmentState.identity.provider == "codex" {
		attachmentState.learnCatalogSourcePath(codexCatalogPath(line))
	}
	attachmentState.stateMu.Lock()
	path, threadID := attachmentState.catalogSourcePath, attachmentState.threadID
	attachmentState.stateMu.Unlock()
	if attachmentState.identity.provider == "claude" {
		return claudeTranscriptPath(attachmentState.cwd, threadID)
	}
	return path
}

// learnCatalogSourcePath records the native file a Codex thread reported and
// moves the attachment's hold to it when it differs.
func (attachmentState *State) learnCatalogSourcePath(path string) {
	if path == "" {
		return
	}
	attachmentState.stateMu.Lock()
	changed := attachmentState.catalogSourcePath != path
	attachmentState.catalogSourcePath = path
	threadID := attachmentState.threadID
	attachmentState.stateMu.Unlock()
	if changed {
		attachmentState.bindCatalogSource(threadID)
	}
}

func (attachmentState *State) setProjectionThreadID(id string) {
	attachmentState.stateMu.Lock()
	if attachmentState.durablyTerminalTurnThreadID != "" && attachmentState.durablyTerminalTurnThreadID != id {
		attachmentState.durablyTerminalTurnID = ""
		attachmentState.durablyTerminalTurnThreadID = ""
	}
	attachmentState.stateMu.Unlock()
	if attachmentState.view.liveProjection() != nil {
		attachmentState.view.liveProjection().setNativeThreadID(id)
	}
	attachmentState.bindCatalogSource(id)
}

// bindCatalogSource holds the native files of identity id. Until id is a
// real provider identity the attachment is held unresolved, with no file.
// Holding runs outside session locks; a close that races it releases again,
// so a closed attachment never keeps a watch.
func (attachmentState *State) bindCatalogSource(id string) {
	if attachmentState.catalogSources == nil {
		return
	}
	attachmentState.stateMu.Lock()
	closed, cwd, known, background := attachmentState.closed, attachmentState.cwd, attachmentState.catalogSourcePath, attachmentState.catalogBackground
	attachmentState.stateMu.Unlock()
	if closed {
		return
	}
	attachmentState.catalogSources.BindAttachedSource(attachmentState.identity.id, attachmentState.identity.provider, id, cwd, known, background)
	attachmentState.stateMu.Lock()
	closed = attachmentState.closed
	attachmentState.stateMu.Unlock()
	if closed {
		attachmentState.releaseCatalogSource()
	}
}

func (attachmentState *State) releaseCatalogSource() {
	if attachmentState.catalogSources != nil {
		attachmentState.catalogSources.ReleaseAttachedSource(attachmentState.identity.id)
	}
}

func (wireState *Wire) recordWire(dir wire.Direction, line []byte) {
	wireState.recordWireLive(dir, line, true)
}

// recordWireLive controls whether arrival time may stand as a turn's start
// anchor. Claude re-attach calls recordWireAt directly so it can also mark
// a frame as replayed.
func (wireState *Wire) recordWireLive(dir wire.Direction, line []byte, live bool, wireSeq ...int64) {
	wireState.recordWireAt(dir, line, live, false, time.Time{}, wireSeq...)
}

// recordWireRaw preserves provider evidence that belongs to another native
// Session multiplexed onto this attachment. It intentionally does not invoke
// any parent semantic consumer (projection, runtime, send confirmation, or
// hosted-operation observation).
func (wireState *Wire) recordWireRaw(dir wire.Direction, line []byte) {
	at := time.Now()
	// A multiplexed child without its own attachment is not watched: its raw
	// traffic hints nothing, and only an explicit child attachment opts in.
	wireState.publishWireEvent(wire.Event{
		TS: at.Format(time.RFC3339Nano), Dir: string(dir),
		Msg: json.RawMessage(append([]byte(nil), line...)),
	}, true)
}

// recordWireAt is recordWireLive with an explicit arrival time, which the
// Projection stamps onto every item the line produces.
//
// The live flag protects a turn's start anchor; replay independently marks a
// Claude re-attach source turn for owner reconciliation. Neither protects the
// per-message timestamps, and for a long time nothing did. A
// claude re-attach replayed a whole conversation through the arrival clock, so
// every message in the transcript re-dated itself to the instant nano came
// back: an eleven-minute session read as eleven minutes of activity compressed
// into the 0.3s the replay took. The shim spools the time it first saw each
// line and now sends it back with the replay, so a replayed line is dated when
// it happened. A zero `at` means "now" — every genuinely live path, and any
// line from a shim too old to send one.
func (wireState *Wire) recordWireAt(dir wire.Direction, line []byte, live, replay bool, at time.Time, wireSeq ...int64) {
	wireState.recordWireWith(nil, nil, dir, line, live, replay, at, wireSeq...)
}

// recordWireWith checks gate, when set, immediately before the frame is
// applied and again before it is published: an error skips the frame, or at
// least its publication when the gate closed while the frame was applying.
// prepare runs once the frame is admitted. Replayed send lines take the
// Session loop too, so they land in spool order among the received frames
// queued ahead of them.
func (wireState *Wire) recordWireWith(gate func() error, prepare func(), dir wire.Direction, line []byte, live, replay bool, at time.Time, wireSeq ...int64) {
	if at.IsZero() {
		at = time.Now()
	}
	evt := wire.Event{
		TS:  at.Format(time.RFC3339Nano),
		Dir: string(dir),
		Msg: json.RawMessage(append([]byte(nil), line...)),
	}
	if core := wireState.runtime.coreSession(); core != nil && (dir == wire.Recv || replay) {
		frame := append([]byte(nil), evt.Msg...)
		sequence := append([]int64(nil), wireSeq...)
		adapterEvent, terminal := wireState.adapterTurnEvent(dir, frame, live, at)
		adapterEvent.LegacyApply = func() error {
			if gate != nil {
				if err := gate(); err != nil {
					return err
				}
			}
			if prepare != nil {
				prepare()
			}
			wireState.applyWireSemantic(gate, dir, frame, live, replay, at, sequence...)
			if terminal != nil && wireState.view.liveProjection() != nil {
				terminal.FinalMessageID = wireState.view.liveProjection().finalAgentItemID(terminal.TurnID)
				if usage := wireState.state.completedTokenUsage(terminal.TurnID); usage != nil {
					terminal.Usage = coreturns.Usage{InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens, OutputTokens: usage.OutputTokens}
				}
			}
			// A frame admitted before its attach was abandoned finishes its own
			// apply, but neither publishes nor carries a completion.
			if gate != nil {
				if err := gate(); err != nil {
					return err
				}
			}
			wireState.publishWireEvent(evt, true)
			return nil
		}
		if gate == nil && prepare == nil && live && !replay && wireState.identity.provider == "codex" &&
			dir == wire.Recv && wireState.view.liveProjection() != nil {
			if delta, ok := parseQueuedCodexDelta(frame); ok &&
				(delta.method != "item/agentMessage/delta" || !wireState.view.liveProjection().dropsLiveTextDeltas()) {
				delta.at = at
				delta.event = evt
				delta.wireSeq = sequence
				batch := &queuedCodexDeltaBatch{first: delta}
				batch.append(delta)
				adapterEvent.AsyncBatchKey = strings.Join([]string{
					"codex-wire-delta", delta.method, delta.threadID, delta.turnID, delta.itemID,
				}, "\x00")
				adapterEvent.AsyncBatchEpoch = core.AttachmentEpoch()
				adapterEvent.AsyncBatchBytes = len(frame)
				adapterEvent.AsyncBatchPayload = delta
				adapterEvent.AsyncBatchMerge = func(payload any) {
					if next, ok := payload.(queuedCodexDelta); ok {
						batch.append(next)
					}
				}
				adapterEvent.LegacyApply = func() error {
					merged, err := batch.mergedLine()
					if err != nil {
						return err
					}
					last := batch.frames[len(batch.frames)-1]
					sequences := make([]int64, 0, len(batch.frames))
					for _, queued := range batch.frames {
						sequences = append(sequences, queued.wireSeq...)
					}
					wireState.applyWireSemantic(nil, dir, merged, live, false, last.at, sequences...)
					for _, queued := range batch.frames {
						wireState.publishWireEvent(queued.event, true)
					}
					return nil
				}
			}
		}
		core.ObserveAsync(adapterEvent)
		return
	}
	if gate != nil && gate() != nil {
		return
	}
	if prepare != nil {
		prepare()
	}
	wireState.applyWireSemantic(gate, dir, line, live, replay, at, wireSeq...)
	wireState.publishWireEvent(evt, true)
}

func (wireState *Wire) adapterTurnEvent(dir wire.Direction, line []byte, live bool, at time.Time) (coresession.AdapterEvent, *coreturns.Finish) {
	if !live || dir != wire.Recv {
		return coresession.AdapterEvent{}, nil
	}
	active, turnID, _, ok := turnActiveTransition(wireState.identity.provider, dir, line)
	if !ok || turnID == "" {
		return coresession.AdapterEvent{}, nil
	}
	actionID := ""
	if wireState.delivery.queuedPromptStore() != nil {
		if prompt, found := wireState.delivery.queuedPromptStore().ByProviderTurn(wireState.identity.provider, wireState.state.ThreadID(), wireState.identity.hostedThreadID, turnID); found {
			actionID = prompt.ClientActionID
		}
	}
	event := coresession.AdapterEvent{ProviderTurnID: turnID}
	if active {
		started := coreturns.Start{HostedThreadID: wireState.identity.hostedThreadID, Provider: wireState.identity.provider, NativeThreadID: wireState.state.ThreadID(), TurnID: turnID, ActionID: actionID, At: at, Source: "adapter"}
		event.TurnEvents = []coresession.TurnEvent{{Started: &started}}
		return event, nil
	}
	status := agentTurnCompleted
	if wireState.identity.provider == "codex" {
		var envelope struct {
			Params struct {
				Turn struct {
					Status string `json:"status"`
				} `json:"turn"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &envelope) == nil && envelope.Params.Turn.Status != "" {
			status = envelope.Params.Turn.Status
		}
	} else if wireState.identity.provider == "grok" {
		_, parsedStatus, _, terminal := grokTerminalFromWire(line)
		if !terminal {
			return coresession.AdapterEvent{}, nil
		}
		status = parsedStatus
	}
	finished := &coreturns.Finish{HostedThreadID: wireState.identity.hostedThreadID, Provider: wireState.identity.provider, NativeThreadID: wireState.state.ThreadID(), TurnID: turnID, ActionID: actionID, Outcome: status, At: at, Source: "adapter"}
	event.TurnEvents = []coresession.TurnEvent{{Finished: finished}}
	return event, finished
}

// applyWireSemantic is the c2: raw-wire translation shim. core/session calls
// it on its sole event goroutine, so one frame finishes before the next event.
//
// gate, when set, is rechecked before the durable agent-operation update, so a
// frame admitted before its attach was abandoned cannot complete an operation.
func (wireState *Wire) applyWireSemantic(gate func() error, dir wire.Direction, line []byte, live, replay bool, at time.Time, wireSeq ...int64) {
	// Normalize before publishing the raw line. Every browser therefore sees
	// the same projection order, and a slow/raw-feed tab cannot affect the
	// transcript reducer.
	if wireState.view.liveProjection() != nil {
		wireState.view.liveProjection().applyWireLive(dir, line, at, live, replay, wireSeq...)
	}
	// Wire-native catalog freshness: turn edges hint the session's known
	// native file. Runs outside every session lock, and the callback is the
	// catalog's nonblocking queue, so recording never stalls.
	wireState.state.nudgeCatalogWire(dir, line)
	wireState.state.observeProviderWork(dir, line, live)
	wireState.view.applyLiveRuntimeWire(dir, line, at)
	// An MCP server changing startup state means the cached tool registry is
	// stale; the next panel fetch re-asks app-server (mcp_inventory.go).
	if inspection.MCPInventoryStale(wireState.identity.provider, dir, line) {
		wireState.view.invalidateMCP()
	}
	if wireState.view.liveProjection() != nil && wireNudgesProjectionMeta(wireState.identity.provider, dir, line) {
		wireState.view.liveProjection().nudgeMetaRefresh()
	}
	if gate != nil && gate() != nil {
		return
	}
	wireState.delivery.observeAgentExecutionWire(dir, line)
}

func (wireState *Wire) publishWireEvent(evt wire.Event, writeSpool bool) {
	wireState.mu.Lock()
	wireState.seq++
	evt.Seq = wireState.seq
	wireState.events = append(wireState.events, evt)
	if len(wireState.events) > wireBufferCap {
		wireState.events = wireState.events[len(wireState.events)-wireBufferCap:]
	}
	subs := make([]chan wire.Event, 0, len(wireState.subs))
	for ch := range wireState.subs {
		subs = append(subs, ch)
	}
	spool := wireState.spool
	wireState.mu.Unlock()

	var spoolErr error
	if writeSpool && spool != nil {
		spoolErr = spool.append(evt)
		if spoolErr != nil {
			log.Printf("wire spool append %s seq %d: %v", wireState.identity.id, evt.Seq, spoolErr)
		}
	}
	for _, ch := range subs {
		select {
		case ch <- evt:
		default: // slow subscriber: drop rather than block the read loop
		}
	}
	if spoolErr != nil {
		wireState.recordSpoolDropped(evt.Seq, spoolErr)
	}
}

// nanoNotification builds a synthetic JSON-RPC notification for events nano
// itself originates (spool failures, instruction-file edits) so they render
// in the Event Feed like any other wire notification.
func nanoNotification(method string, params map[string]any) (wire.Event, bool) {
	line, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return wire.Event{}, false
	}
	return wire.Event{
		TS:  time.Now().Format(time.RFC3339Nano),
		Dir: string(wire.Recv),
		Msg: json.RawMessage(line),
	}, true
}

// recordNanoEvent publishes a synthetic nano-origin notification into the
// feed (and spool), keeping actions nano performs outside the agent wire —
// like instruction-file edits — visible in the Event Feed.
func (wireState *Wire) recordNanoEvent(method string, params map[string]any) {
	if evt, ok := nanoNotification(method, params); ok {
		wireState.publishWireEvent(evt, true)
	}
}

func (wireState *Wire) recordSpoolDropped(seq int64, err error) {
	evt, ok := nanoNotification("wire/spool_dropped", map[string]any{
		"afterSeq": seq,
		"error":    err.Error(),
	})
	if ok {
		wireState.publishWireEvent(evt, false) // the spool just failed; don't re-enter it
	}
}

// Wire owns the ordered raw event history, retained spool and event subscribers.
type Wire struct {
	identity *attachmentIdentity
	delivery *Delivery
	state    *State
	runtime  *Runtime
	view     *View

	mu     sync.Mutex
	events []wire.Event
	seq    int64
	subs   map[chan wire.Event]struct{}
	spool  *wireSpool
}

func (attachmentState *State) nudgeCatalogWire(dir wire.Direction, line []byte) {
	nudgesCatalog := wireNudgesCatalog(attachmentState.identity.provider, dir, line)
	if attachmentState.catalogRefresh != nil && nudgesCatalog {
		if path := attachmentState.catalogPathForNudge(line); path != "" {
			attachmentState.catalogRefresh(path)
		}
	}
}

func (wireState *Wire) closeSpool() {
	if wireState.spool != nil {
		if err := wireState.spool.close(true); err != nil {
			log.Printf("close wire spool %s: %v", wireState.identity.id, err)
		}
	}
}

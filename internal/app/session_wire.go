package app

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	coresession "github.com/dkroshow/nano/internal/core/session"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/inspection"
	"github.com/dkroshow/nano/internal/protocol"
)

// wireBufferCap bounds the raw events a session keeps in memory: the tail the
// Event Feed shows on connect. Older events page from the spool, which holds
// every event. At 20,000 the rings held about 500 MB across 450 sessions, 60%
// of the live heap, for a panel that is rarely open.
const wireBufferCap = 500

type wireEvent struct {
	Seq int64           `json:"seq"`
	TS  string          `json:"ts"`
	Dir string          `json:"dir"`
	Msg json.RawMessage `json:"msg"`
}

type queuedCodexDelta struct {
	method   string
	threadID string
	turnID   string
	itemID   string
	delta    string
	at       time.Time
	event    wireEvent
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

func (sess *session) catalogPathForNudge(line []byte) string {
	if sess.provider == "codex" {
		sess.learnCatalogSourcePath(codexCatalogPath(line))
	}
	sess.stateMu.Lock()
	path, threadID := sess.catalogSourcePath, sess.threadID
	sess.stateMu.Unlock()
	if sess.provider == "claude" {
		return claudeTranscriptPath(sess.cwd, threadID)
	}
	return path
}

// learnCatalogSourcePath records the native file a Codex thread reported and
// moves the attachment's hold to it when it differs.
func (sess *session) learnCatalogSourcePath(path string) {
	if path == "" {
		return
	}
	sess.stateMu.Lock()
	changed := sess.catalogSourcePath != path
	sess.catalogSourcePath = path
	threadID := sess.threadID
	sess.stateMu.Unlock()
	if changed {
		sess.bindCatalogSource(threadID)
	}
}

func (sess *session) setProjectionThreadID(id string) {
	sess.stateMu.Lock()
	if sess.durablyTerminalTurnThreadID != "" && sess.durablyTerminalTurnThreadID != id {
		sess.durablyTerminalTurnID = ""
		sess.durablyTerminalTurnThreadID = ""
	}
	sess.stateMu.Unlock()
	if sess.projection != nil {
		sess.projection.SetNativeThreadID(id)
	}
	sess.bindCatalogSource(id)
}

// bindCatalogSource holds the native files of identity id. Until id is a
// real provider identity the attachment is held unresolved, with no file.
// Holding runs outside session locks; a close that races it releases again,
// so a closed attachment never keeps a watch.
func (sess *session) bindCatalogSource(id string) {
	if sess.catalogSources == nil {
		return
	}
	sess.stateMu.Lock()
	closed, cwd, known, background := sess.closed, sess.cwd, sess.catalogSourcePath, sess.catalogBackground
	sess.stateMu.Unlock()
	if closed {
		return
	}
	sess.catalogSources.BindAttachedSource(sess.id, sess.provider, id, cwd, known, background)
	sess.stateMu.Lock()
	closed = sess.closed
	sess.stateMu.Unlock()
	if closed {
		sess.releaseCatalogSource()
	}
}

func (sess *session) releaseCatalogSource() {
	if sess.catalogSources != nil {
		sess.catalogSources.ReleaseAttachedSource(sess.id)
	}
}

func (sess *session) recordWire(dir protocol.Direction, line []byte) {
	sess.recordWireLive(dir, line, true)
}

// recordWireLive controls whether arrival time may stand as a turn's start
// anchor. Claude re-attach calls recordWireAt directly so it can also mark
// a frame as replayed.
func (sess *session) recordWireLive(dir protocol.Direction, line []byte, live bool, wireSeq ...int64) {
	sess.recordWireAt(dir, line, live, false, time.Time{}, wireSeq...)
}

// recordWireRaw preserves provider evidence that belongs to another native
// session multiplexed onto this attachment. It intentionally does not invoke
// any parent semantic consumer (projection, runtime, send confirmation, or
// hosted-operation observation).
func (sess *session) recordWireRaw(dir protocol.Direction, line []byte) {
	at := time.Now()
	// A multiplexed child without its own attachment is not watched: its raw
	// traffic hints nothing, and only an explicit child attachment opts in.
	sess.publishWireEvent(wireEvent{
		TS: at.Format(time.RFC3339Nano), Dir: string(dir),
		Msg: json.RawMessage(append([]byte(nil), line...)),
	}, true)
}

// recordWireAt is recordWireLive with an explicit arrival time, which the
// projection stamps onto every item the line produces.
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
func (sess *session) recordWireAt(dir protocol.Direction, line []byte, live, replay bool, at time.Time, wireSeq ...int64) {
	sess.recordWireWith(nil, nil, dir, line, live, replay, at, wireSeq...)
}

// recordWireWith checks gate, when set, immediately before the frame is
// applied and again before it is published: an error skips the frame, or at
// least its publication when the gate closed while the frame was applying.
// prepare runs once the frame is admitted. Replayed send lines take the
// session loop too, so they land in spool order among the received frames
// queued ahead of them.
func (sess *session) recordWireWith(gate func() error, prepare func(), dir protocol.Direction, line []byte, live, replay bool, at time.Time, wireSeq ...int64) {
	if at.IsZero() {
		at = time.Now()
	}
	evt := wireEvent{
		TS:  at.Format(time.RFC3339Nano),
		Dir: string(dir),
		Msg: json.RawMessage(append([]byte(nil), line...)),
	}
	if core := sess.coreSession(); core != nil && (dir == protocol.Recv || replay) {
		frame := append([]byte(nil), evt.Msg...)
		sequence := append([]int64(nil), wireSeq...)
		adapterEvent, terminal := sess.adapterTurnEvent(dir, frame, live, at)
		adapterEvent.LegacyApply = func() error {
			if gate != nil {
				if err := gate(); err != nil {
					return err
				}
			}
			if prepare != nil {
				prepare()
			}
			sess.applyWireSemantic(gate, dir, frame, live, replay, at, sequence...)
			if terminal != nil && sess.projection != nil {
				terminal.FinalMessageID = sess.projection.FinalAgentItemID(terminal.TurnID)
				if usage := sess.completedTokenUsage(terminal.TurnID); usage != nil {
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
			sess.publishWireEvent(evt, true)
			return nil
		}
		if gate == nil && prepare == nil && live && !replay && sess.provider == "codex" &&
			dir == protocol.Recv && sess.projection != nil {
			if delta, ok := parseQueuedCodexDelta(frame); ok &&
				(delta.method != "item/agentMessage/delta" || !sess.projection.DropsLiveTextDeltas()) {
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
					sess.applyWireSemantic(nil, dir, merged, live, false, last.at, sequences...)
					for _, queued := range batch.frames {
						sess.publishWireEvent(queued.event, true)
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
	sess.applyWireSemantic(gate, dir, line, live, replay, at, wireSeq...)
	sess.publishWireEvent(evt, true)
}

func (sess *session) adapterTurnEvent(dir protocol.Direction, line []byte, live bool, at time.Time) (coresession.AdapterEvent, *coreturns.Finish) {
	if !live || dir != protocol.Recv {
		return coresession.AdapterEvent{}, nil
	}
	active, turnID, _, ok := turnActiveTransition(sess.provider, dir, line)
	if !ok || turnID == "" {
		return coresession.AdapterEvent{}, nil
	}
	actionID := ""
	if sess.queuedTurns != nil {
		if prompt, found := sess.queuedTurns.ByProviderTurn(sess.provider, sess.ThreadID(), sess.hostedThreadID, turnID); found {
			actionID = prompt.ClientActionID
		}
	}
	event := coresession.AdapterEvent{ProviderTurnID: turnID}
	if active {
		started := coreturns.Start{HostedThreadID: sess.hostedThreadID, Provider: sess.provider, NativeThreadID: sess.ThreadID(), TurnID: turnID, ActionID: actionID, At: at, Source: "adapter"}
		event.TurnEvents = []coresession.TurnEvent{{Started: &started}}
		return event, nil
	}
	status := agentTurnCompleted
	if sess.provider == "codex" {
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
	} else if sess.provider == "grok" {
		_, parsedStatus, _, terminal := grokTerminalFromWire(line)
		if !terminal {
			return coresession.AdapterEvent{}, nil
		}
		status = parsedStatus
	}
	finished := &coreturns.Finish{HostedThreadID: sess.hostedThreadID, Provider: sess.provider, NativeThreadID: sess.ThreadID(), TurnID: turnID, ActionID: actionID, Outcome: status, At: at, Source: "adapter"}
	event.TurnEvents = []coresession.TurnEvent{{Finished: finished}}
	return event, finished
}

// applyWireSemantic is the c2: raw-wire translation shim. core/session calls
// it on its sole event goroutine, so one frame finishes before the next event.
//
// gate, when set, is rechecked before the durable agent-operation update, so a
// frame admitted before its attach was abandoned cannot complete an operation.
func (sess *session) applyWireSemantic(gate func() error, dir protocol.Direction, line []byte, live, replay bool, at time.Time, wireSeq ...int64) {
	// Normalize before publishing the raw line. Every browser therefore sees
	// the same projection order, and a slow/raw-feed tab cannot affect the
	// transcript reducer.
	if sess.projection != nil {
		sess.projection.ApplyWireLive(dir, line, at, live, replay, wireSeq...)
	}
	// Wire-native catalog freshness: turn edges hint the session's known
	// native file. Runs outside every session lock, and the callback is the
	// catalog's nonblocking queue, so recording never stalls.
	nudgesCatalog := wireNudgesCatalog(sess.provider, dir, line)
	if sess.catalogRefresh != nil && nudgesCatalog {
		if path := sess.catalogPathForNudge(line); path != "" {
			sess.catalogRefresh(path)
		}
	}
	sess.observeProviderWork(dir, line, live)
	sess.applyLiveRuntimeWire(dir, line, at)
	// An MCP server changing startup state means the cached tool registry is
	// stale; the next panel fetch re-asks app-server (mcp_inventory.go).
	if inspection.MCPInventoryStale(sess.provider, dir, line) {
		sess.mcp.Invalidate()
	}
	if sess.projection != nil && wireNudgesProjectionMeta(sess.provider, dir, line) {
		sess.projection.NudgeMetaRefresh()
	}
	if gate != nil && gate() != nil {
		return
	}
	sess.observeAgentExecutionWire(dir, line)
}

func (sess *session) publishWireEvent(evt wireEvent, writeSpool bool) {
	sess.mu.Lock()
	sess.seq++
	evt.Seq = sess.seq
	sess.events = append(sess.events, evt)
	if len(sess.events) > wireBufferCap {
		sess.events = sess.events[len(sess.events)-wireBufferCap:]
	}
	subs := make([]chan wireEvent, 0, len(sess.subs))
	for ch := range sess.subs {
		subs = append(subs, ch)
	}
	spool := sess.spool
	sess.mu.Unlock()

	var spoolErr error
	if writeSpool && spool != nil {
		spoolErr = spool.append(evt)
		if spoolErr != nil {
			log.Printf("wire spool append %s seq %d: %v", sess.id, evt.Seq, spoolErr)
		}
	}
	for _, ch := range subs {
		select {
		case ch <- evt:
		default: // slow subscriber: drop rather than block the read loop
		}
	}
	if spoolErr != nil {
		sess.recordSpoolDropped(evt.Seq, spoolErr)
	}
}

// nanoNotification builds a synthetic JSON-RPC notification for events nano
// itself originates (spool failures, instruction-file edits) so they render
// in the Event Feed like any other wire notification.
func nanoNotification(method string, params map[string]any) (wireEvent, bool) {
	line, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return wireEvent{}, false
	}
	return wireEvent{
		TS:  time.Now().Format(time.RFC3339Nano),
		Dir: string(protocol.Recv),
		Msg: json.RawMessage(line),
	}, true
}

// recordNanoEvent publishes a synthetic nano-origin notification into the
// feed (and spool), keeping actions nano performs outside the agent wire —
// like instruction-file edits — visible in the Event Feed.
func (sess *session) recordNanoEvent(method string, params map[string]any) {
	if evt, ok := nanoNotification(method, params); ok {
		sess.publishWireEvent(evt, true)
	}
}

func (sess *session) recordSpoolDropped(seq int64, err error) {
	evt, ok := nanoNotification("wire/spool_dropped", map[string]any{
		"afterSeq": seq,
		"error":    err.Error(),
	})
	if ok {
		sess.publishWireEvent(evt, false) // the spool just failed; don't re-enter it
	}
}

// claude_shim_transport.go — the durable half of the Claude transport seam
// (claude.go). Hosted sessions run inside the claude-shim daemon (:7692),
// which owns the `claude -p` child and spools its stdio, so a session survives
// a nano restart the way hosted codex threads survive one
// (docs/decisions/claude-shim-daemon.md).
//
// One WebSocket connection per nano session — deliberately not multiplexed.
// The shim allows a single attached client per session, so a connection that
// maps 1:1 onto a session makes detach exactly "this connection dropped": no
// demux bookkeeping, and a dead connection can never strand a sibling session.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	coresession "github.com/dkroshow/nano/internal/core/session"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/lifecycle"
	wireclaude "github.com/dkroshow/nano/internal/wire/claude"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

const (
	claudeShimDialTimeout = 3 * time.Second
	claudeShimCallTimeout = 10 * time.Second
	// Before it answers session/attach the shim decodes the session's whole
	// spool (cmd/claude-shim spool.readAfter): about 60 MB/s on this machine
	// under load, slower while a boot attaches many sessions at once.
	claudeShimAttachBytesPerSecond = 10 << 20
	claudeShimAttachMaxTimeout     = 2 * time.Minute
	// Must exceed the shim's 3s graceful-exit period, plus kill/reap time.
	claudeShimCloseTimeout = 10 * time.Second
	// Existing shims interpret fromSeq as "send events whose sequence is
	// greater". MaxInt64 therefore requests an empty replay while still using
	// the deployed, frozen shim contract for an idle retained-history seam.
	claudeShimTailSeq int64 = wireclaude.TailSeq
)

// claudeShimReplayTimeout bounds the wait for replayDone on attach (a local
// file scan: overrunning it means the shim is wedged, not that the spool is
// big) and is the no-progress window while the session loop applies the
// replay (awaitApplied). A variable only so tests can shorten it.
var claudeShimReplayTimeout = 30 * time.Second

// The shim's JSON-RPC surface (frozen contract, docs/decisions/claude-shim-daemon.md).
const (
	shimMethodStart      = "session/start"
	shimMethodAttach     = "session/attach"
	shimMethodInput      = "session/input"
	shimMethodClose      = "session/close"
	shimMethodLine       = "session/line"
	shimMethodExited     = "session/exited"
	shimMethodReplayDone = "session/replayDone"
)

// Shim error codes, and the sentinels callers dispatch on with errors.Is.
const (
	shimCodeSessionAttached = -32001
	shimCodeSessionNotFound = -32002
	shimCodeSessionExited   = -32003
)

var (
	errShimSessionAttached = errors.New("claude shim: session already attached")
	errShimSessionNotFound = errors.New("claude shim: session not found")
	errShimSessionExited   = errors.New("claude shim: session exited")
	// errShimUnreachable: the attach never reached the shim (dial failed).
	errShimUnreachable = errors.New("claude shim: unreachable")
	// The shim already confirmed this child exists. Failure to settle its
	// replay is a local attachment failure, never permission to respawn it.
	errClaudeReplayUnsettled = errors.New("claude replay did not settle")
	// errClaudeAttachAbandoned skips work a failed attach left queued.
	errClaudeAttachAbandoned = errors.New("claude attach was abandoned")
)

// shimRPCError maps a shim error response onto its sentinel so callers can
// branch on the condition rather than the code; unmapped codes pass through.
func shimRPCError(rpcErr *wireclaude.RPCError) error {
	switch rpcErr.Code {
	case shimCodeSessionAttached:
		return fmt.Errorf("%w (%s)", errShimSessionAttached, rpcErr.Message)
	case shimCodeSessionNotFound:
		return fmt.Errorf("%w (%s)", errShimSessionNotFound, rpcErr.Message)
	case shimCodeSessionExited:
		return fmt.Errorf("%w (%s)", errShimSessionExited, rpcErr.Message)
	}
	return rpcErr
}

// claudeShimTransport hosts one session inside the shim.
type claudeShimTransport struct {
	owner   *claudeSession
	client  *wireclaude.Client
	posture claudeSpawnPosture

	// shimSessionID addresses the session on input/close. Written before the
	// transport is handed to any caller; the read loop never reads it (one
	// connection carries exactly one session).
	shimSessionID string

	// closing marks a teardown we initiated, so the read loop's exit is not
	// mistaken for the shim dying under us.
	closing atomic.Bool

	// abandoned marks a connection given up after frames were queued. Every
	// frame and turn event it queued becomes a no-op on the session loop, so
	// it can neither mutate state nor write a closed spool after the failure.
	abandoned atomic.Bool
	// applied counts replayed frames the session loop has applied: the
	// attach's progress clock while it waits out a large backlog.
	applied atomic.Int64
	// replayFacts follows the replay from its floor to decide which replayed
	// permission requests are still open when it ends; replayErr is its first
	// continuity failure. Both are touched only by the goroutine draining the
	// replay (markSessionReady).
	replayFacts *claudeSpoolFacts
	replayErr   error

	// seq tracks the shim's own line numbering, which nano decoded nowhere
	// until the turn anchor needed it: the number sampled before a turn is sent
	// is what the replayed user line is later checked against
	// (claude.go, anchorSeq).
	seq atomic.Int64

	replayDone chan struct{}
	replayOnce sync.Once

	// Notifications can race the response to session/start or session/attach:
	// the shim writes the response and its first spool line on the same socket,
	// while the caller only learns shimSessionID after call returns. The socket
	// reader only appends while the session is not ready; the attach goroutine
	// drains those notifications after replayDone has been ingested. Keeping
	// transcript projection off the socket reader during replay prevents a
	// large history from backpressuring and disconnecting the shim.
	notifyMu             sync.Mutex
	sessionReady         bool
	pendingNotifications []shimNotification
	replayIngested       chan struct{}
	replayIngestedOnce   sync.Once

	eventMu       sync.RWMutex
	turnEventSink func(claudeShimEvent)
}

type shimNotification struct {
	method string
	params json.RawMessage
}

type claudeShimEventKind = wireclaude.EventKind

const (
	claudeShimAccepted = wireclaude.EventAccepted
	claudeShimItem     = wireclaude.EventItem
	claudeShimResult   = wireclaude.EventResult
)

// claudeShimEvent is the transport-to-projection seam for provider-owned turn
// identity. SH1 populates it; C2 may consume it without reconstructing an
// ordinal from Nano state.
type claudeShimEvent = wireclaude.Event

// startClaudeShimSession dials the shim and has it spawn the child. The
// starting connection becomes the session's attached client by construction.
func startClaudeShimSession(owner *claudeSession, shimURL string, posture claudeSpawnPosture) (*claudeShimTransport, error) {
	tr, err := dialClaudeShim(owner, shimURL, posture)
	if err != nil {
		return nil, err
	}
	var res struct {
		ShimSessionID string `json:"shimSessionId"`
	}
	turnIDSession := owner.sess.hostedThreadID
	if turnIDSession == "" {
		turnIDSession = owner.sess.id
	}
	params := map[string]any{
		"bin": posture.Bin, "args": posture.Args, "cwd": owner.sess.cwd,
		"turnIdSession": turnIDSession,
	}
	if err := tr.call(shimMethodStart, params, &res, claudeShimCallTimeout); err != nil {
		tr.abandon()
		return nil, fmt.Errorf("%s: %w", shimMethodStart, err)
	}
	tr.shimSessionID = res.ShimSessionID
	tr.SetTurnEventSink(tr.observeTurnEvent)
	tr.markSessionReady()
	// A freshly started child has no spool behind it, so nothing it says is a
	// replay. The shim only sends session/replayDone to an ATTACHING client, so
	// without this the transport would read as "still replaying" for its whole
	// life — and the control channel, which ignores inbound requests during a
	// replay, would silently drop every dialog the child ever raised.
	tr.finishReplay()
	return tr, nil
}

// attachClaudeSession binds sess to a session already living in the shim,
// replaying its spooled lines after plan.FromSeq through the same path live
// lines take, then following live, and checks the shim's answer against what
// the plan observed. posture comes from the stored attachment record so
// re-attaching cannot erase the flags a later `--resume` fallback needs.
//
// Returns errShimSessionAttached (another nano holds it) or
// errShimSessionNotFound (the shim never knew it, or lost it with its own
// restart) for callers to dispatch on.
func attachClaudeSession(sess *session, shimURL, nativeSessionID string, plan claudeReplayPlan, posture claudeSpawnPosture, stored attachmentRecord) (*claudeSession, error) {
	fromSeq := plan.FromSeq
	sess.beginTurnRestore()
	restoreSettled := false
	defer func() {
		if !restoreSettled {
			sess.cancelTurnRestore()
		}
	}()
	// supersedes: the id we attached by. The replayed init normally re-derives
	// the same one and nothing is retired; a shim that answered with a different
	// session would leave the requested record stale, so drop it then.
	c := newClaudeSession(sess, nativeSessionID)
	// Captured before the attach: the replay re-derives identity and rewrites
	// this record (captureInit), and nothing downstream of that point can still
	// see what the previous nano wrote. persistedAnchor starts at the stored
	// value because that IS what is on disk — so a turn that finished
	// unattended registers as a change and clears the record, instead of
	// leaving a stale anchor for the next restart to restore onto a new turn.
	c.storedAnchor = stored
	c.persistedAnchor.Store(stored.TurnStartedAtMs)
	// Recreate action-bearing submitted rows before shim replay so replayed user
	// evidence promotes those rows in place rather than painting a duplicate.
	sess.restoreSubmissionOutbox()
	// A failed attach whose last admitted frame never acknowledged is still
	// running on its session loop; a second generation must not overlap it.
	if err := claudeDrainSettled(sess); err != nil {
		return nil, err
	}
	tr, err := dialClaudeShim(c, shimURL, posture)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errShimUnreachable, err)
	}
	tr.SetTurnEventSink(tr.observeTurnEvent)
	var res struct {
		ShimSessionID   string `json:"shimSessionId"`
		NativeSessionID string `json:"nativeSessionId"`
		State           string `json:"state"`
		TurnActive      *bool  `json:"turnActive"`
		LastSeq         int64  `json:"lastSeq"`
	}
	params := map[string]any{"nativeSessionId": nativeSessionID, "fromSeq": fromSeq}
	// The stored shim handle is the only address a never-prompted child has:
	// its spool holds no init line, so the shim's byNativeID index cannot know
	// it. The shim prefers the shim id and falls back to the native id, so a
	// stale handle (the shim restarted and re-minted nothing) still resolves
	// through the native id when one exists.
	if stored.ShimSessionID != "" {
		params["shimSessionId"] = stored.ShimSessionID
	}
	if err := tr.call(shimMethodAttach, params, &res, claudeShimAttachTimeout(plan.SpoolBytes)); err != nil {
		tr.abandon()
		return nil, fmt.Errorf("%s: %w", shimMethodAttach, err)
	}
	// The spool only grows, so a shim reporting less than the scan read is
	// not replaying the spool that chose the floor. A missing spool is proof
	// of nothing unless the shim has recorded nothing at all. No frame has
	// been processed yet, so abandoning here leaves the child untouched.
	switch {
	case plan.Err != nil:
		tr.abandon()
		return nil, plan.Err
	case plan.Missing && res.LastSeq != 0:
		tr.abandon()
		return nil, fmt.Errorf("%w: the shim holds %d spooled lines but Nano found no spool", errClaudeRecoveryUnproven, res.LastSeq)
	case res.LastSeq < plan.Observed:
		tr.abandon()
		return nil, fmt.Errorf("%w: the shim reports lastSeq %d below the scanned spool's %d", errClaudeRecoveryUnproven, res.LastSeq, plan.Observed)
	}
	seedSeq := fromSeq
	if seedSeq == claudeShimTailSeq {
		seedSeq = 0
	}
	tr.seq.Store(seedSeq)
	// Replay must continue exactly from the floor; the pending notifications
	// are not processed until markSessionReady below.
	tr.replayFacts = newClaudeSpoolFacts("", seedSeq, nil)
	tr.shimSessionID = res.ShimSessionID
	// An empty replay can have no system.init line. The attach response still
	// carries the native identity required by transactional prompt bindings.
	if res.NativeSessionID != "" {
		sess.stateMu.Lock()
		if sess.threadID == "" {
			sess.threadID = res.NativeSessionID
		}
		sess.stateMu.Unlock()
		sess.setProjectionThreadID(res.NativeSessionID)
	}
	c.tr = tr
	// Do not let the WebSocket reader project any replay line. It keeps
	// ingesting the ordered notification stream until replayDone is queued;
	// only then can this goroutine drain the finite replay without applying
	// backpressure to the shim's bounded outbound queue.
	select {
	case <-tr.replayIngested:
	case <-tr.client.Done():
	case <-time.After(claudeShimReplayTimeout):
		// The shim already answered for this child: never a replacement.
		tr.abandon()
		return nil, fmt.Errorf("%w: shim replay ingestion did not settle within %s", errClaudeReplayUnsettled, claudeShimReplayTimeout)
	}
	tr.markSessionReady()
	// From here frames are queued on the session loop; every failure must
	// also stop them.
	fail := func(err error) (*claudeSession, error) {
		if !tr.abandonQueued(sess) {
			err = fmt.Errorf("%w (its abandoned replay is still draining; retries wait for it)", err)
		}
		return nil, err
	}
	if tr.replayErr != nil {
		return fail(tr.replayErr)
	}
	if fromSeq != claudeShimTailSeq && res.LastSeq > seedSeq && tr.lastSeq() < res.LastSeq {
		select {
		case <-tr.client.Done():
		default:
			return fail(fmt.Errorf("%w: the shim replayed through %d of %d", errClaudeRecoveryUnproven, tr.lastSeq(), res.LastSeq))
		}
	}
	tr.observeSeq(res.LastSeq)
	// The shim answered, so this session's transport is up and will take input —
	// the same readiness the start path gets (claude.go markTransportReady). A
	// replayed init refines the identity; it is not what makes the session
	// usable, and a spool that never held one must not leave it "starting".
	if !c.markTransportReady() {
		// Closed while the attach was in flight. Let go of the shim session
		// rather than replaying a spool into a session nobody is watching.
		tr.abandoned.Store(true)
		_ = tr.close()
		return nil, errSpawnSessionClosed
	}
	log.Printf("claude session %s attached to shim session %s (native %s, state %s, lastSeq %d, fromSeq %d)",
		sess.id, res.ShimSessionID, res.NativeSessionID, res.State, res.LastSeq, fromSeq)
	// Hand the session back only once the replay has landed: the caller then
	// sees identity (thread id, model, ready) already re-derived from the
	// replayed init line, exactly as a fresh start would leave it.
	select {
	case <-tr.replayDone:
	case <-c.exited:
	case <-time.After(claudeShimReplayTimeout):
		return fail(fmt.Errorf("%w: shim replay exceeded %s", errClaudeReplayUnsettled, claudeShimReplayTimeout))
	}
	// replayDone only proves that the transport handed off the replay. Raw
	// frames are applied asynchronously on the session loop; this ordered
	// acknowledgement ensures their transcript and wire publication is complete
	// before attachment settlement, even if the provider has already exited.
	if core := sess.coreSession(); core != nil {
		if err := tr.awaitApplied(core); err != nil {
			return fail(fmt.Errorf("%w: processing acknowledgement failed: %w", errClaudeReplayUnsettled, err))
		}
	}
	// The replay has re-derived the turn state but deliberately left any turn
	// it found undated. This is where the persisted anchor is revalidated
	// against what the spool actually showed, and where a record describing a
	// turn that finished unattended is discarded (claude.go).
	c.restoreReplayedTurnAnchor()
	// The attach response snapshots the child under the same lock as LastSeq.
	// Apply it only if no newer live line arrived meanwhile; otherwise that line
	// is the newer activity/terminal authority already reflected by Nano.
	if res.TurnActive != nil && tr.lastSeq() == res.LastSeq {
		startedAtMs := int64(0)
		if *res.TurnActive {
			startedAtMs = stored.TurnStartedAtMs
			turnID := ""
			if sess.projection != nil {
				turnID = sess.projection.EnsureClaudeActiveTurn(startedAtMs)
			}
			sess.restoreTurnActive(true, turnID, startedAtMs)
		} else {
			sess.restoreTurnActive(false, "", 0)
		}
	}
	// The browser must see the replay as one reconciliation, not as a live
	// performance of every historical turn. Publish only the state that remains
	// after both the ordered spool and the shim's attach snapshot have settled.
	select {
	case <-tr.client.Done():
		return fail(fmt.Errorf("%w: shim disconnected before settlement", errClaudeReplayUnsettled))
	default:
	}
	// Recover the spool's unanswered requests before readiness can drain a queue.
	c.rehydratePending(nil)
	if !sess.awaitSavedCatchUp(tr.client.Done(), nil) {
		if sess.ended() {
			return fail(errSpawnSessionClosed)
		}
		return fail(fmt.Errorf("%w: shim disconnected while the saved file caught up", errClaudeReplayUnsettled))
	}
	sess.stateMu.Lock()
	epoch, sequence := sess.lifecycleState.Epoch, sess.nextEvidenceSequenceLocked()
	active, turnID, anchor := sess.turnRestoreActive, sess.turnRestoreID, sess.turnRestoreStartedAtMs
	sess.stateMu.Unlock()
	if res.State == "exited" {
		c.markExited()
	}
	restoreSettled = sess.settleProviderEvidence(claudeLifecycleAdapter{epoch: epoch, sequence: sequence, state: res.State, active: active, settled: true}, turnID, anchor)
	if !restoreSettled && res.State != "exited" {
		return fail(fmt.Errorf("%w: claude shim did not supply settled current evidence", errClaudeReplayUnsettled))
	}
	c.syncTurnAnchor()
	// Only now: the control handshake is what re-arms stop/model/dialogs on a
	// re-attached session, and its pending_* arrays are the authoritative
	// rehydration source — but it must not race the replay, whose inbound
	// control frames are deliberately ignored while it runs
	// (claude_control.go).
	go c.startControlChannel()
	return c, nil
}

// awaitApplied waits until the session loop has applied every frame queued
// before this call. A large backlog may legitimately take minutes; the attach
// fails only when the loop applies nothing for claudeShimReplayTimeout.
func (t *claudeShimTransport) awaitApplied(core *coresession.Session) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- core.Observe(ctx, coresession.AdapterEvent{}) }()
	poll := claudeShimReplayTimeout / 10
	if poll > time.Second {
		poll = time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	progress, progressAt := t.applied.Load(), time.Now()
	for {
		select {
		case err := <-done:
			return err
		case now := <-ticker.C:
			if applied := t.applied.Load(); applied != progress {
				progress, progressAt = applied, now
			} else if now.Sub(progressAt) >= claudeShimReplayTimeout {
				return fmt.Errorf("no replay progress for %s after %d frames", claudeShimReplayTimeout, progress)
			}
		}
	}
}

// abandonQueued gives up a connection whose frames are already queued. Queued
// frames and turn events become no-ops, and a frame already applying cannot
// publish or carry a completion (recordWireWith). It reports whether the
// session loop acknowledged passing them within claudeShimReplayTimeout.
// If not, a frame is still applying: the thread is marked draining until the
// loop acknowledges, and every attach refuses meanwhile (claudeDrainSettled).
// Nothing blocks on it and the provider child is untouched.
func (t *claudeShimTransport) abandonQueued(sess *session) bool {
	t.abandon()
	core := sess.coreSession()
	if core == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudeShimReplayTimeout)
	err := core.Observe(ctx, coresession.AdapterEvent{})
	cancel()
	if err == nil {
		return true
	}
	done := make(chan struct{})
	key := claudeDrainKey(sess)
	claudeDrains.Store(key, done)
	go func() {
		_ = core.Observe(context.Background(), coresession.AdapterEvent{})
		claudeDrains.CompareAndDelete(key, done)
		close(done)
	}()
	return false
}

// claudeDrains holds the threads whose abandoned replay has not yet been
// acknowledged by their session loop, keyed by hosted thread.
var claudeDrains sync.Map

func claudeDrainKey(sess *session) string {
	if sess.hostedThreadID != "" {
		return sess.hostedThreadID
	}
	return "session:" + sess.id
}

// claudeDrainSettled refuses, without waiting, an attach that would overlap an
// abandoned drain. The refusal is no-fallback: the original child exists.
func claudeDrainSettled(sess *session) error {
	if _, draining := claudeDrains.Load(claudeDrainKey(sess)); draining {
		return fmt.Errorf("%w: a previous attach's abandoned replay is still draining", errClaudeReplayUnsettled)
	}
	return nil
}

// queuedWorkAllowed is the session-loop gate for this connection's frames and
// turn events.
func (t *claudeShimTransport) queuedWorkAllowed() error {
	if t.abandoned.Load() {
		return errClaudeAttachAbandoned
	}
	return nil
}

// admitted records one frame the loop began applying: replay progress.
func (t *claudeShimTransport) admitted() { t.applied.Add(1) }

func dialClaudeShim(owner *claudeSession, shimURL string, posture claudeSpawnPosture) (*claudeShimTransport, error) {
	tr := &claudeShimTransport{
		owner: owner, posture: posture, replayDone: make(chan struct{}), replayIngested: make(chan struct{}),
	}
	client, err := wireclaude.Dial(shimURL, claudeShimDialTimeout, tr.onNotify, tr.onDisconnect)
	if err != nil {
		return nil, err
	}
	tr.client = client
	return tr, nil
}

// claudeShimAttachTimeout bounds session/attach by the spool the shim reads
// first: the ordinary call timeout plus the spool at
// claudeShimAttachBytesPerSecond, at most claudeShimAttachMaxTimeout.
func claudeShimAttachTimeout(spoolBytes int64) time.Duration {
	return min(claudeShimCallTimeout+time.Duration(spoolBytes/claudeShimAttachBytesPerSecond)*time.Second, claudeShimAttachMaxTimeout)
}

func (t *claudeShimTransport) call(method string, params, result any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return t.callContext(ctx, method, params, result)
}

func (t *claudeShimTransport) callContext(ctx context.Context, method string, params, result any) error {
	err := t.client.Call(ctx, method, params, result, claudeShimCallTimeout)
	var rpcErr *wireclaude.RPCError
	if errors.As(err, &rpcErr) {
		return shimRPCError(rpcErr)
	}
	return err
}

// isReplaying reports whether the shim is still pushing spooled history. The
// boundary is exact: the shim snapshots the spool and takes the attachment under
// one lock, so replayDone closing is the first live line's predecessor, never an
// overlap (cmd/claude-shim/session.go).
func (t *claudeShimTransport) isReplaying() bool {
	select {
	case <-t.replayDone:
		return false
	default:
		return true
	}
}

// lastSeq is the newest line number the shim has delivered on this connection.
func (t *claudeShimTransport) lastSeq() int64 { return t.seq.Load() }

func (t *claudeShimTransport) observeSeq(seq int64) bool {
	for {
		last := t.seq.Load()
		if seq <= last {
			return false
		}
		if t.seq.CompareAndSwap(last, seq) {
			return true
		}
	}
}

func (t *claudeShimTransport) sendLine(line []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), claudeShimCallTimeout)
	defer cancel()
	return t.sendLineContext(ctx, line)
}

func (t *claudeShimTransport) sendLineContext(ctx context.Context, line []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	params := map[string]any{"shimSessionId": t.shimSessionID, "line": string(line)}
	var accepted struct {
		TurnID string `json:"turnId"`
	}
	if err := t.callContext(ctx, shimMethodInput, params, &accepted); err != nil {
		return fmt.Errorf("%s: %w", shimMethodInput, err)
	}
	if kind, ok := claudeShimEventKindFor(protocol.Send, line); ok {
		t.emitTurnEvent(claudeShimEvent{
			Kind: kind, ProviderTurnID: accepted.TurnID,
			Direction: protocol.Send, Line: append([]byte(nil), line...),
		})
	}
	return nil
}

func (t *claudeShimTransport) SetTurnEventSink(sink func(claudeShimEvent)) {
	t.eventMu.Lock()
	t.turnEventSink = sink
	t.eventMu.Unlock()
}

func (t *claudeShimTransport) emitTurnEvent(event claudeShimEvent) {
	// A result names its own prompt (claudeAnsweredTurn), tagged or not.
	if event.ProviderTurnID == "" && event.Kind != claudeShimResult {
		return
	}
	t.eventMu.RLock()
	sink := t.turnEventSink
	t.eventMu.RUnlock()
	if sink != nil {
		sink(event)
	}
}

func (sess *session) observeClaudeShimEvent(event claudeShimEvent) {
	sess.observeClaudeShimEventGated(event, nil)
}

// observeTurnEvent is this connection's turn-event sink: its events share the
// connection's abandonment gate.
func (t *claudeShimTransport) observeTurnEvent(event claudeShimEvent) {
	t.owner.sess.observeClaudeShimEventGated(event, t.queuedWorkAllowed)
}

func (sess *session) observeClaudeShimEventGated(event claudeShimEvent, gate func() error) {
	if sess == nil || event.ProviderTurnID == "" && event.Kind != claudeShimResult {
		return
	}
	// A spooled frame carries its own identity to the projection when it is
	// applied (claudeFrameTurn). Only the acknowledgement of a live send, which
	// has no spool sequence yet, sets it here, before its own send line.
	if sess.projection != nil && event.Seq == 0 {
		sess.projection.SetClaudeProviderTurnID(event.ProviderTurnID)
	}
	core := sess.coreSession()
	if core == nil {
		return
	}
	at := event.At
	if at.IsZero() {
		at = time.Now()
	}
	adapterEvent := coresession.AdapterEvent{ProviderTurnID: event.ProviderTurnID}
	var finish *coreturns.Finish
	switch event.Kind {
	case claudeShimAccepted:
		started := coreturns.Start{HostedThreadID: sess.hostedThreadID, Provider: sess.provider, NativeThreadID: sess.ThreadID(), TurnID: event.ProviderTurnID, At: at, Source: "adapter"}
		adapterEvent.TurnEvents = []coresession.TurnEvent{{Started: &started}}
	case claudeShimResult:
		// The shim tags a result with the prompt it last wrote, which a result
		// arriving after queued writes did not answer. The loop names the turn
		// when it applies the event, after any submit ahead of it has bound its
		// prompt.
		adapterEvent.ProviderTurnID = ""
		status, _ := claudeProjectionTerminal(event.Line)
		finish = &coreturns.Finish{HostedThreadID: sess.hostedThreadID, Provider: sess.provider, NativeThreadID: sess.ThreadID(),
			Outcome: status, At: at, Source: "adapter"}
		adapterEvent.TurnEvents = []coresession.TurnEvent{{Finished: finish}}
	}
	// The final message and usage are read when the loop applies the event,
	// right after the result frame itself: at enqueue time a replay's frames
	// are still queued and the projection has not seen them.
	adapterEvent.LegacyApply = func() error {
		if gate != nil {
			if err := gate(); err != nil {
				return err
			}
		}
		if finish == nil {
			return nil
		}
		turnID, _ := sess.claudeAnsweredTurn(event.Line)
		if finish.TurnID = turnID; turnID == "" {
			return nil // a finish that names no turn finishes nothing
		}
		if sess.projection != nil {
			finish.FinalMessageID = sess.projection.FinalAgentItemID(turnID)
		}
		// The result line is this turn's own usage evidence. A narrowed replay
		// has no projection turn for the prompt to key live usage by.
		usage := sess.completedTokenUsage(turnID)
		var message map[string]json.RawMessage
		if json.Unmarshal(event.Line, &message) == nil {
			if lineUsage := normalizeTokenUsage(claudeResultUsagePayload(message)); lineUsage != nil {
				usage = lineUsage
			}
		}
		if usage != nil {
			finish.Usage = coreturns.Usage{InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens, OutputTokens: usage.OutputTokens}
		}
		return nil
	}
	core.ObserveAsync(adapterEvent)
}

// claudeAnsweredTurn is the durable turn of the prompt a Claude result
// answered. Claude names that prompt in the result's user_message_uuid: the
// submission id Nano stamped on its user line, which merged prompts share
// with their row's own. named is false when the result names no prompt, as
// for a turn the harness started; turnID is "" unless exactly one prompt of
// this thread carries the id.
func (sess *session) claudeAnsweredTurn(line []byte) (turnID string, named bool) {
	var result struct {
		UserMessageUUID string `json:"user_message_uuid"`
	}
	if json.Unmarshal(line, &result) != nil || result.UserMessageUUID == "" {
		return "", false
	}
	if sess.turns == nil || sess.hostedThreadID == "" {
		return "", true
	}
	records, err := sess.turns.ByEvent(context.Background(), sess.hostedThreadID, result.UserMessageUUID)
	if err != nil {
		log.Printf("Claude result on %s: its prompt is unreadable: %v", sess.hostedThreadID, err)
		return "", true
	}
	if len(records) != 1 {
		return "", true
	}
	return records[0].ProviderTurnID, true
}

func claudeShimEventKindFor(dir protocol.Direction, line []byte) (claudeShimEventKind, bool) {
	return wireclaude.EventKindFor(dir, line)
}

// close asks the shim to end the child gracefully and drops the connection.
// A failed RPC leaves termination uncertain, even for session_not_found:
// the shim removes its registry entry before it waits for the child to exit.
func (t *claudeShimTransport) close() error {
	t.closing.Store(true)
	params := map[string]any{"shimSessionId": t.shimSessionID}
	err := t.call(shimMethodClose, params, nil, claudeShimCloseTimeout)
	t.client.Close()
	if err == nil {
		t.owner.markExited()
	}
	return err
}

// closeDetachedClaude uses a control socket, never an attachment or replay.
func closeDetachedClaude(shimURL, shimID string) error {
	if shimURL == "" || shimID == "" {
		return errors.New("Claude stop unconfirmed: missing shim endpoint or exact process identity")
	}
	client, err := wireclaude.Dial(shimURL, claudeShimDialTimeout, func(string, json.RawMessage) {}, func(error) {})
	if err != nil {
		return err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), claudeShimCloseTimeout)
	defer cancel()
	return client.Call(ctx, shimMethodClose, map[string]string{"shimSessionId": shimID}, nil, claudeShimCloseTimeout)
}

// abandon drops a connection whose handshake failed without reporting a child
// exit — there is no child, and the caller is about to fall back.
func (t *claudeShimTransport) abandon() {
	t.abandoned.Store(true)
	t.closing.Store(true)
	t.client.Close()
}

func (t *claudeShimTransport) durableAttachment(nativeThreadID, model string) (attachmentRecord, bool) {
	return attachmentRecord{
		Provider:       "claude",
		NativeThreadID: nativeThreadID,
		Cwd:            t.owner.sess.cwd,
		Model:          model,
		Bin:            t.posture.Bin,
		Args:           t.posture.Args,
		ShimSessionID:  t.shimSessionID,
	}, true
}

func (t *claudeShimTransport) onNotify(method string, params json.RawMessage) {
	t.notifyMu.Lock()
	if !t.sessionReady {
		t.pendingNotifications = append(t.pendingNotifications, shimNotification{
			method: method,
			params: append(json.RawMessage(nil), params...),
		})
		t.notifyMu.Unlock()
		if method == shimMethodReplayDone {
			t.finishReplayIngestion()
		}
		return
	}
	t.notifyMu.Unlock()
	t.handleNotification(method, params)
}

func (t *claudeShimTransport) finishReplayIngestion() {
	if t.replayIngested == nil {
		return
	}
	t.replayIngestedOnce.Do(func() { close(t.replayIngested) })
}

func (t *claudeShimTransport) markSessionReady() {
	for {
		t.notifyMu.Lock()
		if t.sessionReady {
			t.notifyMu.Unlock()
			return
		}
		if len(t.pendingNotifications) == 0 {
			t.sessionReady = true
			t.notifyMu.Unlock()
			return
		}
		pending := t.pendingNotifications
		t.pendingNotifications = nil
		t.notifyMu.Unlock()
		for _, notification := range pending {
			t.handleNotification(notification.method, notification.params)
		}
	}
}

func (t *claudeShimTransport) handleNotification(method string, params json.RawMessage) {
	if t.abandoned.Load() {
		return
	}
	switch method {
	case shimMethodLine:
		line, ok := wireclaude.DecodeLine(params)
		if !ok {
			return
		}
		// The shim sequence is the replay idempotency key. Raw line content is
		// not: identical deltas and repeated user messages are legitimate.
		// Send lines only ever arrive during replay — the shim does not echo
		// live input, which SendTurn has already recorded.
		if line.Seq > 0 && !t.observeSeq(line.Seq) {
			return
		}
		if t.replayFacts != nil && t.isReplaying() && t.replayErr == nil {
			// A request retired by its own turn's result or by a cancel has
			// no answer to replay, but is no longer open either. A gap in the
			// replay fails the attach (attachClaudeSession).
			retired, err := t.replayFacts.observe(claudeSpoolFrame{Seq: line.Seq, Dir: line.Direction, Line: line.Bytes, TurnID: line.ProviderTurnID})
			t.replayErr = err
			for _, id := range retired {
				t.owner.control.noteReplayedAnswer(id)
			}
		}
		kind, hasTurnEvent := claudeShimEventKindFor(line.Direction, line.Bytes)
		turnEvent := claudeShimEvent{
			Kind: kind, ProviderTurnID: line.ProviderTurnID, Seq: line.Seq, At: line.At,
			Direction: line.Direction, Line: line.Bytes,
		}
		if hasTurnEvent && kind != claudeShimResult {
			t.emitTurnEvent(turnEvent)
		}
		frameTurn := claudeFrameTurn{id: line.ProviderTurnID, known: line.Seq > 0}
		// The session writer must apply the result to the transcript before it
		// persists the terminal turn fact, even while a send holds the frame.
		// That also makes the final item and token usage available to the
		// turn record.
		var then func()
		if hasTurnEvent && kind == claudeShimResult {
			then = func() { t.emitTurnEvent(turnEvent) }
		}
		t.owner.deliverFrame(t, line.Direction, line.Bytes, line.Seq, line.At, frameTurn, then)
	case shimMethodReplayDone:
		t.finishReplay()
	case shimMethodExited:
		t.owner.markExited()
	}
}

// onDisconnect fires when the connection's read loop ends. The shim owns the
// child. Losing this socket proves a lost connection, not that the child exited.
func (t *claudeShimTransport) onDisconnect(err error) {
	t.finishReplayIngestion() // no more replay frames can arrive on this socket
	t.finishReplay()          // a replay that will never arrive must not block attach
	if t.closing.Load() {
		return
	}
	log.Printf("claude shim connection lost for session %s (%v)", t.owner.sess.id, err)
	t.owner.exitOnce.Do(func() {
		close(t.owner.exited)
		t.owner.sess.observeConnection(lifecycle.EventLinkLost)
	})
}

func (t *claudeShimTransport) finishReplay() {
	t.replayOnce.Do(func() { close(t.replayDone) })
}

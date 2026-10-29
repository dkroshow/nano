// codex.go — the Codex provider: hosts all Codex sessions over one shared
// `codex app-server --listen ws://...` daemon connection. Per-session thread
// traffic is routed by threadId into each session's wire buffer.
package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/config"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/wire"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

const (
	codexHandshakeTimeout               = 60 * time.Second
	codexResumeConcurrency              = 4
	codexReconnectInitialBackoff        = 250 * time.Millisecond
	codexReconnectMaximumBackoff        = 30 * time.Second
	codexReconnectAttempts              = 6
	codexZeroTurnRecoveryAttempts       = 8
	codexZeroTurnRecoveryInitialBackoff = 50 * time.Millisecond
	codexZeroTurnRecoveryMaximumBackoff = 2 * time.Second
)

const codexZeroTurnRecoveryAttemptTimeout = 5 * time.Second

const (
	supervisedDaemonLabel = config.DefaultCodexSupervisorLabel
	fallbackSpawnDelay    = 30 * time.Second
	supervisorCheckRate   = time.Second
	supervisorWaitLogRate = time.Minute
)

var clientInfo = map[string]any{
	"clientInfo":   map[string]any{"name": "nano", "title": "nano inspector", "version": "0.1.0"},
	"capabilities": map[string]any{"experimentalApi": true},
}

// CodexDaemon owns the shared Codex connection, thread routes, and reconnection work.
type CodexDaemon struct {
	wsURL           string
	supervisorLabel string

	supervisedJobLoaded func() bool
	fallbackSpawnDelay  time.Duration
	newCommand          func(string, ...string) *exec.Cmd

	mu                     sync.Mutex
	client                 *protocol.Client
	cmd                    *exec.Cmd
	daemonUnavailableSince time.Time
	lastSupervisorCheck    time.Time
	supervisorLoaded       bool
	lastSupervisorWaitLog  time.Time
	sessions               map[string]*Session
	serverRequests         map[string]*Session
	reconnecting           map[string]bool
	reconnectWake          map[string]chan struct{}
	resumeSlots            chan struct{}
	resumeTimeout          time.Duration
	reconnectBackoff       time.Duration
	reconnectPending       bool
	recoveryRunning        bool
	models                 json.RawMessage
	modelsClient           *protocol.Client
	modelsFetch            *codexModelFetch
}

// NewCodexDaemon constructs the provider connection with its process-supervisor check.
func NewCodexDaemon(wsURL, supervisorLabel string, supervisorLoaded func() bool) *CodexDaemon {
	d := &CodexDaemon{
		wsURL:              wsURL,
		supervisorLabel:    supervisorLabel,
		fallbackSpawnDelay: fallbackSpawnDelay,
		newCommand:         exec.Command,
		sessions:           map[string]*Session{},
		serverRequests:     map[string]*Session{},
		reconnecting:       map[string]bool{},
		reconnectWake:      map[string]chan struct{}{},
		resumeSlots:        make(chan struct{}, codexResumeConcurrency),
	}
	d.supervisedJobLoaded = supervisorLoaded
	return d
}

func (d *CodexDaemon) ensure() error {
	return d.ensureContext(context.Background())
}

func (d *CodexDaemon) ensureContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.client != nil {
		select {
		case <-d.client.Done():
			d.client = nil
			d.reconnectPending = true
			for _, wake := range d.reconnectWake {
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		default:
			return nil
		}
	}

	client, err := d.startClientContext(ctx)
	if err != nil {
		dialErr := err
		if err := d.maybeStartFallbackLocked(time.Now()); err != nil {
			return fmt.Errorf("start codex app-server daemon: %w (dial %s failed: %v)", err, d.wsURL, dialErr)
		}
		client, err = d.waitForClientContext(ctx)
		if err != nil {
			return fmt.Errorf("connect codex app-server daemon %s: %w (initial dial: %v)", d.wsURL, err, dialErr)
		}
	}

	d.client = client
	d.daemonUnavailableSince = time.Time{}
	reconnect := d.reconnectPending
	d.reconnectPending = false
	go d.markExited(client)
	if reconnect {
		go d.reconnectSessions()
	}
	return nil
}

func (d *CodexDaemon) maybeStartFallbackLocked(now time.Time) error {
	state := protocol.FallbackState{
		ProcessRunning: d.cmd != nil, UnavailableSince: d.daemonUnavailableSince,
		LastSupervisorCheck: d.lastSupervisorCheck, SupervisorLoaded: d.supervisorLoaded,
		LastSupervisorWaitLog: d.lastSupervisorWaitLog,
	}
	err := protocol.MaybeStartFallback(now, &state, protocol.FallbackConfig{
		SpawnDelay: d.fallbackSpawnDelay, SupervisorRate: supervisorCheckRate,
		SupervisorLogRate: supervisorWaitLogRate, SupervisorLabel: d.supervisorLabel, WSURL: d.wsURL,
	}, d.supervisedJobLoaded, d.startProcessLocked)
	d.daemonUnavailableSince = state.UnavailableSince
	d.lastSupervisorCheck = state.LastSupervisorCheck
	d.supervisorLoaded = state.SupervisorLoaded
	d.lastSupervisorWaitLog = state.LastSupervisorWaitLog
	return err
}

func (d *CodexDaemon) startClient() (*protocol.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return d.startClientContext(ctx)
}

func (d *CodexDaemon) startClientContext(ctx context.Context) (*protocol.Client, error) {
	return protocol.StartClientContext(ctx, d.wsURL, clientInfo, protocol.ClientHandlers{
		ServerRequest: d.handleServerRequest,
		Notification:  d.handleNotification,
		WireLog:       d.routeConnectionWire,
	})
}

func (d *CodexDaemon) startProcessLocked() error {
	return protocol.StartProcessLocked(d.wsURL, d.supervisorLabel, d.newCommand, func(cmd *exec.Cmd) {
		d.cmd = cmd
	}, func(cmd *exec.Cmd, err error) {
		d.mu.Lock()
		if d.cmd == cmd {
			d.cmd = nil
		}
		d.mu.Unlock()
		if err != nil {
			log.Printf("codex app-server daemon exited: %v", err)
		}
	})
}

func (d *CodexDaemon) waitForClient(timeout time.Duration) (*protocol.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.waitForClientContext(ctx)
}

func (d *CodexDaemon) waitForClientContext(ctx context.Context) (*protocol.Client, error) {
	return protocol.WaitForClientContext(ctx, d.startClientContext, d.maybeStartFallbackLocked)
}

func (d *CodexDaemon) markExited(client *protocol.Client) {
	protocol.WaitForExit(client, func() {
		d.mu.Lock()
		if d.client != client {
			d.mu.Unlock()
			return
		}
		d.client = nil
		d.reconnectPending = true
		for _, wake := range d.reconnectWake {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
		startRecovery := !d.recoveryRunning
		d.recoveryRunning = true
		sessions := make([]*Session, 0, len(d.sessions))
		for _, sess := range d.sessions {
			sessions = append(sessions, sess)
		}
		d.mu.Unlock()
		for _, sess := range sessions {
			sess.stateMu.Lock()
			closed := sess.closed || sess.connectionStatusLocked() == StatusExited
			sess.stateMu.Unlock()
			if !closed {
				sess.observeConnection(lifecycle.EventLinkLost)
			}
		}
		if startRecovery {
			go d.recoverConnection()
		}
	})
}

// recoveryOwnsSessionFailure closes the race where a pending resume call sees
// the dropped socket before markExited has promoted its session from starting
// to reconnecting. Client.Done is the earliest transport-loss evidence; once
// it is closed, the old handshake must not publish exited over the replacement
// connection's recovery attempt.
func (d *CodexDaemon) recoveryOwnsSessionFailure(sess *Session) bool {
	d.mu.Lock()
	client := d.client
	recovering := d.reconnectPending || d.recoveryRunning
	if client != nil {
		select {
		case <-client.Done():
			recovering = true
		default:
		}
	}
	d.mu.Unlock()
	if !recovering {
		return false
	}
	sess.stateMu.Lock()
	closed := sess.closed
	sess.stateMu.Unlock()
	return !closed
}

// recoverConnection makes transport recovery autonomous. A reconnecting
// Session cannot trigger ensure through /turn because the readiness gate quite
// correctly rejects it, so waiting for an unrelated RPC leaves every existing
// pane stranded. One daemon-wide loop reconnects the socket, then ensureContext
// launches the per-thread resume handshakes already owned by reconnectSessions.
func (d *CodexDaemon) recoverConnection() {
	protocol.RecoverConnection(func() bool {
		d.mu.Lock()
		pending := d.reconnectPending && len(d.sessions) > 0
		if !pending {
			d.recoveryRunning = false
		}
		d.mu.Unlock()
		return pending
	}, d.ensureContext, func() bool {
		d.mu.Lock()
		recovered := d.client != nil && !d.reconnectPending
		if recovered {
			d.recoveryRunning = false
		}
		d.mu.Unlock()
		return recovered
	})
}

// reconnectSessions rebinds live Codex sessions to a replacement app-server
// connection. A socket drop is transport loss, not proof that every hosted
// thread exited; keeping the session rows alive lets the provider resume them
// without forcing a Nano restart or turning a pending send into a false 409.
func (d *CodexDaemon) reconnectSessions() {
	d.mu.Lock()
	if d.client == nil {
		d.mu.Unlock()
		return
	}
	unique := map[*Session]struct{}{}
	for _, sess := range d.sessions {
		unique[sess] = struct{}{}
	}
	work := make([]*Session, 0, len(unique))
	for sess := range unique {
		if sess == nil {
			continue
		}
		if d.reconnecting[sess.id] {
			continue
		}
		sess.stateMu.Lock()
		closed := sess.closed || sess.connectionStatusLocked() == StatusExited
		eligible := sess.connectionStatusLocked() == "ready" || sess.connectionStatusLocked() == "reconnecting" || sess.connectionStatusLocked() == "reconnect_parked" || sess.connectionStatusLocked() == "reconnect_failed"
		threadID := sess.threadID
		sess.stateMu.Unlock()
		if closed || !eligible || threadID == "" {
			continue
		}
		d.reconnecting[sess.id] = true
		d.reconnectWake[sess.id] = make(chan struct{}, 1)
		work = append(work, sess)
	}
	d.mu.Unlock()
	for _, sess := range work {
		go d.reconnectSessionLoop(sess)
	}
}

func (d *CodexDaemon) requestSessionRecovery(sess *Session) {
	d.mu.Lock()
	sess.stateMu.Lock()
	eligible := !sess.closed && (sess.connectionStatusLocked() == "reconnecting" || sess.connectionStatusLocked() == "reconnect_parked") && sess.threadID != ""
	sess.stateMu.Unlock()
	if !eligible || d.reconnecting[sess.id] {
		if eligible && d.reconnecting[sess.id] {
			sess.stateMu.Lock()
			parked := sess.connectionStatusLocked() == "reconnect_parked"
			sess.stateMu.Unlock()
			if parked {
				select {
				case d.reconnectWake[sess.id] <- struct{}{}:
				default:
				}
			}
		}
		d.mu.Unlock()
		return
	}
	d.reconnecting[sess.id] = true
	d.reconnectWake[sess.id] = make(chan struct{}, 1)
	d.mu.Unlock()
	go d.reconnectSessionLoop(sess)
}

func (d *CodexDaemon) reconnectSessionLoop(sess *Session) {
	defer func() {
		d.mu.Lock()
		rearm := false
		select {
		case <-d.reconnectWake[sess.id]:
			rearm = true
		default:
		}
		delete(d.reconnecting, sess.id)
		delete(d.reconnectWake, sess.id)
		d.mu.Unlock()
		if rearm {
			sess.stateMu.Lock()
			closed := sess.closed || sess.connectionStatusLocked() == StatusExited
			sess.stateMu.Unlock()
			if !closed {
				sess.observeConnection(lifecycle.EventRetryRequested)
				sess.observeConnection(lifecycle.EventLinkLost)
				d.requestSessionRecovery(sess)
			}
		}
	}()
	d.mu.Lock()
	wake := d.reconnectWake[sess.id]
	d.mu.Unlock()
	sess.observeConnection(lifecycle.EventRetryRequested)
	delay := codexReconnectInitialBackoff
	if d.reconnectBackoff > 0 {
		delay = d.reconnectBackoff
	}
	timeout := codexHandshakeTimeout
	if d.resumeTimeout > 0 {
		timeout = d.resumeTimeout
	}
	baseTimeout := timeout
	for attempt := 1; attempt <= codexReconnectAttempts; attempt++ {
		sess.stateMu.Lock()
		closed := sess.closed || sess.connectionStatusLocked() == StatusExited
		sess.stateMu.Unlock()
		if closed {
			return
		}
		progress, _ := json.Marshal(map[string]any{"attempts": attempt, "retrying": true, "timeoutMs": timeout.Milliseconds()})
		sess.setState("codexReconnect", progress)
		sess.observeConnection(lifecycle.EventLinkLost)
		req := ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: sess.ThreadID()}
		err := runCodexHandshakeWithTimeout(context.Background(), sess, d, req, timeout)
		if err == nil {
			sess.setState("codexReconnect", json.RawMessage(`{"retrying":false}`))
			return
		}
		sess.stateMu.Lock()
		model := sess.model
		closed = sess.closed || sess.connectionStatusLocked() == StatusExited
		sess.stateMu.Unlock()
		if closed {
			return
		}
		if codexNoRolloutError(err) && sess.codexMayBeZeroTurn() && sess.markCodexZeroTurnReady(model) {
			return
		}
		var rpcErr *protocol.RPCError
		terminal := (codexNoRolloutError(err) && !sess.codexZeroTurnRecoveryNeeded()) ||
			(errors.As(err, &rpcErr) && !codexNoRolloutError(err) && (rpcErr.Code == -32600 || rpcErr.Code == -32602))
		if terminal {
			if settlementErr := sess.failCodexReconnect(err.Error()); settlementErr != nil {
				err, terminal = settlementErr, false
			}
		}
		state, _ := json.Marshal(map[string]any{"error": err.Error(), "attempts": attempt, "retrying": !terminal && attempt < codexReconnectAttempts})
		sess.setState("codexReconnect", state)
		if terminal {
			return
		}
		if attempt == codexReconnectAttempts {
			sess.observeConnection(lifecycle.EventRetriesExhausted)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			base := codexHandshakeTimeout
			if d.resumeTimeout > 0 {
				base = d.resumeTimeout
			}
			timeout = min(timeout+base, base*3)
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-wake:
			timer.Stop()
			attempt = 0
			timeout = baseTimeout
			continue
		}
		if d.reconnectBackoff == 0 {
			delay = min(delay*2, codexReconnectMaximumBackoff)
		}
	}
}

// Call performs a request through the shared Codex connection.
func (d *CodexDaemon) Call(ctx context.Context, method string, params any, result any) error {
	if err := d.ensureContext(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	client := d.client
	d.mu.Unlock()
	return client.Call(ctx, method, params, result)
}

// callForSession performs a Codex request associated with one attachment.
func (d *CodexDaemon) callForSession(ctx context.Context, sess *Session, method string, params any, result any) error {
	_, err := d.callForSessionWithWireLog(ctx, sess, method, params, result, sess.wire.recordWire)
	return err
}

// callForSessionWithWireLog is callForSession with a request-pair-specific
// wire logger. Most callers retain the raw app-server exchange. Resume uses a
// compact logger because its returned turns are already rendered from Nano's
// durable transcript and can be multi-megabyte payloads. The channel closes
// when the connection that carried the call ends.
func (d *CodexDaemon) callForSessionWithWireLog(ctx context.Context, sess *Session, method string, params any, result any, wireLog protocol.WireLogger) (<-chan struct{}, error) {
	if err := d.ensureContext(ctx); err != nil {
		return nil, err
	}
	d.mu.Lock()
	client := d.client
	d.mu.Unlock()
	return client.Done(), client.CallWithWireLog(ctx, method, params, result, wireLog)
}

// Model reads are demand-driven. Explicit refreshes discover account/rollout
// changes without replacing the daemon; concurrent callers share one read.
type codexModelFetch struct {
	done   chan struct{}
	result json.RawMessage
	err    error
}

// FetchModels returns the Codex model catalog for the current connection.
func (d *CodexDaemon) FetchModels(refresh bool) (json.RawMessage, error) {
	d.mu.Lock()
	cached, cachedClient, client := d.models, d.modelsClient, d.client
	live := client != nil
	if live {
		select {
		case <-client.Done():
			live = false
		default:
		}
	}
	if !refresh && cached != nil && (!live || cachedClient == client) {
		d.mu.Unlock()
		return cached, nil
	}
	if pending := d.modelsFetch; pending != nil {
		d.mu.Unlock()
		<-pending.done
		if pending.err != nil && !refresh && cached != nil {
			return cached, nil
		}
		return pending.result, pending.err
	}
	pending := &codexModelFetch{done: make(chan struct{})}
	d.modelsFetch = pending
	d.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var result json.RawMessage
	err := d.ensureContext(ctx)
	if err == nil {
		d.mu.Lock()
		client = d.client
		d.mu.Unlock()
		err = client.Call(ctx, "model/list", map[string]any{}, &result)
	}
	d.mu.Lock()
	if err == nil && d.client == client {
		d.models, d.modelsClient = result, client
	}
	if err != nil {
		err = fmt.Errorf("model/list: %w", err)
	}
	pending.result, pending.err = result, err
	d.modelsFetch = nil
	close(pending.done)
	d.mu.Unlock()
	if err != nil && !refresh && cached != nil {
		return cached, nil
	}
	return result, err
}

// ValidateUltrafast checks that the selected model supports the requested Ultrafast tier.
// Ultrafast is an opt-in capability of the selected model and account.
// Never infer it from a model name or substitute the Fast tier.
func (d *CodexDaemon) ValidateUltrafast(model, tier string) error {
	if tier != "ultrafast" {
		return nil
	}
	if d == nil {
		return errors.New("Codex model capabilities are unavailable")
	}
	raw, err := d.FetchModels(true)
	if err != nil {
		return err
	}
	var catalog struct {
		Data []struct {
			Model        string `json:"model"`
			ID           string `json:"id"`
			ServiceTiers []struct {
				ID string `json:"id"`
			} `json:"serviceTiers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return err
	}
	for _, entry := range catalog.Data {
		if entry.Model == model || entry.ID == model {
			for _, supported := range entry.ServiceTiers {
				if supported.ID == tier {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("Ultrafast is not advertised for %s on this account; choose Standard or Fast first", model)
}

// ThreadAttachedError rejects attaching a second nano session to a thread
// another session already routes. handleSessionCreate maps it to the frozen 409
// common error envelope with existingSessionId in error.details so the
// frontend can jump to the existing session.
type ThreadAttachedError struct{ ExistingSessionID string }

// Error returns the error message exposed to the caller.
func (e *ThreadAttachedError) Error() string {
	return fmt.Sprintf("thread already attached to session %s", e.ExistingSessionID)
}

// claimThread atomically claims the routing slot for threadID before any wire
// call, so two resumes of one thread can never both reach the app-server. On
// success the claim doubles as the routing registration; releaseThread is the
// failure-path undo, and the thread/unsubscribe close path the normal release.
func (d *CodexDaemon) claimThread(threadID string, sess *Session) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if existing, ok := d.sessions[threadID]; ok && existing != sess {
		return &ThreadAttachedError{ExistingSessionID: existing.id}
	}
	d.sessions[threadID] = sess
	return nil
}

// releaseThread drops the routing entry if it still belongs to sess.
func (d *CodexDaemon) releaseThread(threadID string, sess *Session) {
	if threadID == "" {
		return
	}
	d.mu.Lock()
	if d.sessions[threadID] == sess {
		delete(d.sessions, threadID)
	}
	d.mu.Unlock()
}

// registerThread binds threadID to sess for wire routing. First binding wins:
// silently rebinding would reroute a live session's wire mid-stream, so a
// conflicting registration is logged and dropped (resume conflicts are already
// rejected earlier by claimThread; start mode mints fresh ids).
func (d *CodexDaemon) registerThread(threadID string, sess *Session) {
	if threadID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if existing, ok := d.sessions[threadID]; ok && existing != sess {
		log.Printf("thread %s already routed to session %s; keeping it over session %s", threadID, existing.id, sess.id)
		return
	}
	d.sessions[threadID] = sess
}

func (d *CodexDaemon) closeSession(sess *Session) {
	// Answer anything parked before letting go of the thread: the app-server
	// outlives nano, so a request left unanswered here holds a turn open with
	// nobody able to reach it (interactions.go).
	sess.closeInteractions()
	threadID := sess.ThreadID()
	if threadID == "" {
		return // no id yet: a fresh start's handshake owns the cleanup instead
	}
	d.unsubscribeThread(sess, threadID)
	d.releaseThread(threadID, sess)
}

// unsubscribeThread tells the app-server nano is done with a thread. Split out
// of closeSession for the fresh-start race: a thread/start answering after its
// Session was closed minted a thread the session never knew about, so nothing
// else is left to unsubscribe it (runCodexHandshake).
func (d *CodexDaemon) unsubscribeThread(sess *Session, threadID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var result json.RawMessage
	if err := d.callForSession(ctx, sess, "thread/unsubscribe", map[string]any{"threadId": threadID}, &result); err != nil {
		log.Printf("thread/unsubscribe %s: %v", threadID, err)
	}
}

func (d *CodexDaemon) routeConnectionWire(dir wire.Direction, line []byte) {
	var msg protocol.Message
	if err := json.Unmarshal(line, &msg); err != nil {
		return
	}
	if msg.IsResponse() && msg.ID != nil {
		key := string(*msg.ID)
		d.mu.Lock()
		sess := d.serverRequests[key]
		delete(d.serverRequests, key)
		d.mu.Unlock()
		if sess != nil {
			sess.wire.recordWire(dir, line)
		}
		return
	}

	threadID := threadIDFromParams(msg.Params)
	if threadID == "" {
		return
	}
	d.mu.Lock()
	sess := d.sessions[threadID]
	if msg.IsRequest() && msg.ID != nil && sess != nil {
		d.serverRequests[string(*msg.ID)] = sess
	}
	d.mu.Unlock()
	if sess != nil {
		sess.wire.recordWire(dir, line)
	}
}

func threadIDFromParams(params json.RawMessage) string {
	var obj map[string]json.RawMessage
	if len(params) == 0 || json.Unmarshal(params, &obj) != nil {
		return ""
	}
	if id := stringField(obj, "threadId"); id != "" {
		return id
	}
	// The legacy approval pair (execCommandApproval / applyPatchApproval) names
	// the same thing `conversationId`. Without this the pair routes nowhere, so
	// it never reaches a session's wire buffer and never becomes a dialog.
	if id := stringField(obj, "conversationId"); id != "" {
		return id
	}
	var thread map[string]json.RawMessage
	if raw := obj["thread"]; len(raw) > 0 && json.Unmarshal(raw, &thread) == nil {
		return stringField(thread, "id")
	}
	return ""
}

func stringField(obj map[string]json.RawMessage, key string) string {
	var s string
	if raw := obj[key]; len(raw) > 0 && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func codexThreadParams(req ProviderRequest) map[string]any {
	p := map[string]any{
		"cwd":            req.Cwd,
		"approvalPolicy": req.Approval,
	}
	p["sandbox"] = req.Sandbox
	if req.Model != "" {
		p["model"] = req.Model
	}
	// serviceTier is honored at thread/start (verified 2026-07-07; the doc's
	// probe corrected this file's earlier claim that it was turn-level only).
	if req.ServiceTier != "" {
		p["serviceTier"] = req.ServiceTier
	}
	if req.Personality != "" {
		p["personality"] = req.Personality
	}
	if req.ApprovalsReviewer != "" {
		p["approvalsReviewer"] = req.ApprovalsReviewer
	}
	if req.Ephemeral {
		p["ephemeral"] = true
	}
	if req.BaseInstructions != "" {
		p["baseInstructions"] = req.BaseInstructions
	}
	if dev := codexDeveloperInstructions(req); dev != "" {
		p["developerInstructions"] = dev
	}
	if req.ModelProvider != "" {
		p["modelProvider"] = req.ModelProvider
	}
	// config is a per-thread config.toml override object; decode so it rides
	// the wire as a real object rather than an embedded string.
	if len(req.Config) > 0 {
		var cfg map[string]any
		if err := json.Unmarshal(req.Config, &cfg); err == nil && len(cfg) > 0 {
			p["config"] = cfg
		}
	}
	if req.ThreadSource != "" {
		p["threadSource"] = req.ThreadSource
	}
	if req.SessionStartSource != "" {
		p["sessionStartSource"] = req.SessionStartSource
	}
	if req.ServiceName != "" {
		p["serviceName"] = req.ServiceName
	}
	return p
}

// codexDeveloperInstructions merges the caller's developer-role text with
// Nano's identity block. Caller text stays first and verbatim; the block is
// purely additive. baseInstructions is never touched — it replaces Codex's
// built-in system prompt, so appending there would change its meaning.
func codexDeveloperInstructions(req ProviderRequest) string {
	dev := req.DeveloperInstructions
	if !identityInjectable(req) {
		return dev
	}
	block := nanoIdentityContext(req.HostedThreadID, req.Provider, req.APIOrigin)
	if dev == "" {
		return block
	}
	return dev + "\n\n" + block
}

// codexResumeParams builds the exact thread/resume params for a resolved
// spawn request. Resume is intentionally narrow: it sends no caller
// CWD/model/sandbox overrides. It does send the thread's service tier, because
// Codex otherwise resumes at its config's tier.
func codexResumeParams(req ProviderRequest) map[string]any {
	p := map[string]any{"threadId": req.ThreadID}
	if req.ServiceTier != "" {
		p["serviceTier"] = req.ServiceTier
	}
	return p
}

// CodexSessionRequest builds the Codex start or resume request from resolved provider inputs.
func CodexSessionRequest(req ProviderRequest) (string, map[string]any) {
	if req.Mode == "resume" {
		return "thread/resume", codexResumeParams(req)
	}
	return "thread/start", codexThreadParams(req)
}

// CodexTurnOverrides builds the params merged into every turn/start. Effort
// has no thread/start field (schema-confirmed) so it rides here, with the
// reasoning-summary level. Model has a thread/start field but is also a
// per-turn override ("Override the model for this turn and subsequent turns",
// TurnStartParams), which is what lets the pane switch model mid-thread.
// serviceTier has the same turn-and-subsequent-turns contract, so the pane's
// Standard/Priority control and /fast command share this path. Empty leaves the
// thread's own posture alone.
func CodexTurnOverrides(model, effort, serviceTier, summary string) map[string]any {
	o := map[string]any{}
	if model != "" {
		o["model"] = model
	}
	if effort != "" {
		o["effort"] = effort
	}
	if serviceTier != "" {
		o["serviceTier"] = serviceTier
	}
	if summary != "" {
		o["summary"] = summary
	}
	return o
}

// codexInterruptTimeout bounds the wait for a turn/interrupt reply. It is short
// on purpose: a live turn is acknowledged in milliseconds (empty
// TurnInterruptResponse), and a turn that has already finished is **never
// acknowledged at all** — probed 2026-07-30 against the running daemon, where
// interrupting a completed turn id simply produced no response, not an error.
// Waiting out a normal 30s RPC timeout on that path would turn the completion
// race into a frozen button and then a 502.
const codexInterruptTimeout = 5 * time.Second

// interruptTurn asks the app-server to stop turnID on threadID. The response is
// empty (TurnInterruptResponse), so only the error matters — and a deadline is
// one of the expected outcomes, not a fault (see the timeout above). A reply
// that arrives after the deadline is dropped rather than recorded, which is
// correct: nothing is waiting for it and the turn's own turn/completed line
// already carries the outcome.
func (d *CodexDaemon) interruptTurn(ctx context.Context, sess *Session, threadID, turnID string) error {
	ctx, cancel := context.WithTimeout(ctx, codexInterruptTimeout)
	defer cancel()
	var result json.RawMessage
	return d.callForSession(ctx, sess, "turn/interrupt",
		map[string]any{"threadId": threadID, "turnId": turnID}, &result)
}

// startCodexSession starts or resumes a thread on the shared app-server
// daemon. Neither mode waits for its handshake: the call can take up to
// codexHandshakeTimeout when the daemon is slow, and a spawn that blocks that
// long is a hung dialog rather than a spawn. Both return once the session can
// receive the handshake's traffic, and both surface a failure through
// failSpawn (Slice R3).
// seedCodexTurnOverrides is retained only for legacy rollback-parser tests.
// Production resume always calls seedCodexCanonicalOverrides below, which is
// keyed by the hosted identity in control.sqlite.
func (sess *Session) seedCodexTurnOverrides(folded map[string]hostedstore.PostureEntry, legacyServiceTier string) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	if folded == nil {
		if legacyServiceTier != "" {
			sess.turnServiceTier = legacyServiceTier
		}
		return
	}
	overlayFold(folded, postureFieldModel, &sess.turnModel)
	overlayFold(folded, postureFieldEffort, &sess.turnEffort)
	overlayFold(folded, postureFieldServiceTier, &sess.turnServiceTier)
}

func startCodexSession(sess *Session, daemon *CodexDaemon, req ProviderRequest) error {
	sess.codexZeroTurnEligible = req.Mode == "start" || req.CodexZeroTurnResume
	// Set before any handshake runs: close() reaches the daemon through this
	// field, and a session torn down mid-handshake must be able to unsubscribe
	// and release whatever it holds.
	sess.codex = daemon
	if req.Mode == "resume" && sess.metadata != nil {
		if view, err := sess.metadata.View(sess.hostedThreadID); err == nil {
			sess.seedCodexCanonicalOverrides(view)
		}
	}
	if req.Mode == "resume" {
		// Resume knows its thread id before any wire call: pre-claim the
		// routing slot so a second resume of the same thread fails right here
		// with no thread/resume ever sent (single-nano-attachment guard). The
		// claim doubles as the wire-routing registration, so daemon traffic
		// for the thread reaches this session's buffer from this moment on —
		// including everything the background handshake produces.
		if err := daemon.claimThread(req.ThreadID, sess); err != nil {
			return err
		}
		sess.stateMu.Lock()
		sess.threadID = req.ThreadID
		sess.stateMu.Unlock()
		sess.setProjectionThreadID(req.ThreadID)
		go func() {
			if err := runCodexHandshake(sess, daemon, req); err != nil {
				// A supervised daemon keeps a newly started, zero-turn thread in
				// memory across a nano restart, but Codex has no rollout for
				// thread/resume to load yet. Boot re-attach may keep that exact
				// thread routed so its delayed first turn can create the rollout.
				// Manual resumes and every other error retain the normal exited
				// contract below.
				if sess.codexMayBeZeroTurn() && codexNoRolloutError(err) &&
					sess.markCodexZeroTurnReady(req.codexResumeModel) {
					log.Printf("boot re-attach kept zero-turn codex thread %s in session %s", req.ThreadID, sess.id)
					return
				}
				// markExited promotes an attached resume from starting to
				// reconnecting before the dead client's pending Call returns. The
				// replacement handshake now owns this session; the old goroutine must
				// not release its routing claim or overwrite recovery with exited.
				sess.stateMu.Lock()
				statusRecovery := !sess.closed && (sess.connectionStatusLocked() == "reconnecting" || sess.connectionStatusLocked() == "ready")
				sess.stateMu.Unlock()
				recoveryOwnsFailure := statusRecovery || daemon.recoveryOwnsSessionFailure(sess) ||
					(req.BootReattach != nil && (errors.Is(err, context.DeadlineExceeded) || codexThreadClosingError(err)))
				if recoveryOwnsFailure {
					sess.observeConnection(lifecycle.EventLinkLost)
					daemon.requestSessionRecovery(sess)
					log.Printf("resume thread %s (session %s): old connection ended during recovery: %v", req.ThreadID, sess.id, err)
					return
				}
				// Release before surfacing the failure: once status "exited"
				// is observable the thread must already be resumable again.
				daemon.releaseThread(req.ThreadID, sess)
				log.Printf("resume thread %s (session %s): %v", req.ThreadID, sess.id, err)
				sess.failSpawn(err.Error())
			}
		}()
		return nil
	}
	// A fresh start has no thread id to pre-claim — thread/start mints one — so
	// there is nothing to route until the handshake answers, and nothing to
	// release if it never does. runCodexHandshake registers the minted id
	// itself, and drops it again if the session was closed in the meantime.
	go func() {
		if err := runCodexHandshake(sess, daemon, req); err != nil {
			log.Printf("start thread (session %s): %v", sess.id, err)
			sess.failSpawn(err.Error())
		}
	}()
	return nil
}

func (sess *Session) seedCodexCanonicalOverrides(view hostedstore.MetadataView) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	// A cleared/unknown canonical value must not leave a stale in-memory
	// override from an earlier attachment generation.
	sess.turnModel, sess.turnEffort, sess.turnServiceTier = "", "", ""
	for field, current := range view.Fields {
		value, ok := hostedstore.ResumeString(current)
		if !ok {
			continue
		}
		switch field {
		case postureFieldModel:
			sess.turnModel = value
		case postureFieldEffort:
			sess.turnEffort = value
		case postureFieldServiceTier:
			sess.turnServiceTier = value
		}
	}
}

// codexNoRolloutError identifies the app-server's zero-turn resume failure by
// its typed RPC error. The numeric code has varied across test and CLI builds;
// the stable provider message is the useful discriminator.
func codexNoRolloutError(err error) bool {
	var rpcErr *protocol.RPCError
	return errors.As(err, &rpcErr) && strings.Contains(strings.ToLower(rpcErr.Message), "no rollout found for thread")
}

// codexThreadClosingError is the app-server refusing a resume while it still
// closes the thread after Nano's connection dropped: its listener's command
// channel is closed, or the thread is closing and it asks for a retry once
// closed. A later resume succeeds, so boot hands it to session recovery's
// bounded retries rather than leaving the thread exited.
func codexThreadClosingError(err error) bool {
	var rpcErr *protocol.RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	message := strings.ToLower(rpcErr.Message)
	return strings.Contains(message, "thread listener command channel is closed") ||
		strings.Contains(message, "retry thread/resume after the thread is closed")
}

// markCodexZeroTurnReady turns a boot resume's zero-rollout failure into the
// usable daemon-resident state. A concurrent explicit close wins and leaves
// the caller to run the ordinary failure cleanup.
func (sess *Session) markCodexZeroTurnReady(model string) bool {
	sess.stateMu.Lock()
	if sess.closed || sess.connectionStatusLocked() == StatusExited {
		sess.stateMu.Unlock()
		return false
	}
	sess.transitionLifecycleLocked(lifecycle.EventReadyIdle)
	sess.model = model
	sess.spawnError = ""
	sess.codexZeroTurnResume = true
	sess.stateMu.Unlock()
	sess.setState("codexReconnect", json.RawMessage(`{"retrying":false,"zeroTurn":true}`))
	sess.PublishProjectionRuntime()
	sess.NotifyRuntime()
	sess.updateHostedThread("ready", "")
	sess.kickQueuedTurnDrain()
	return true
}

func (sess *Session) codexZeroTurnRecoveryNeeded() bool {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return !sess.closed && sess.codexZeroTurnResume
}

func (sess *Session) finishCodexZeroTurnRecovery() {
	sess.stateMu.Lock()
	sess.codexZeroTurnResume = false
	sess.stateMu.Unlock()
}

func (sess *Session) startCodexZeroTurnRecovery() {
	if sess == nil || !ProviderLifecycleCapabilities(sess.provider).ResumeWithoutTranscript || sess.codex == nil {
		return
	}
	sess.stateMu.Lock()
	if sess.closed || !sess.codexZeroTurnResume || sess.codexZeroTurnRecoveryRunning {
		sess.stateMu.Unlock()
		return
	}
	sess.codexZeroTurnRecoveryRunning = true
	sess.stateMu.Unlock()
	go sess.runCodexZeroTurnRecovery()
}

func (sess *Session) runCodexZeroTurnRecovery() {
	defer func() {
		sess.stateMu.Lock()
		sess.codexZeroTurnRecoveryRunning = false
		sess.stateMu.Unlock()
	}()
	backoff := time.Duration(0)
	for attempt := 1; attempt <= codexZeroTurnRecoveryAttempts; attempt++ {
		if !sess.codexZeroTurnRecoveryNeeded() {
			return
		}
		if backoff > 0 {
			timer := time.NewTimer(backoff)
			<-timer.C
			if !sess.codexZeroTurnRecoveryNeeded() {
				return
			}
		}
		req := ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: sess.ThreadID()}
		err := runCodexHandshakeWithTimeout(context.Background(), sess, sess.codex, req, codexZeroTurnRecoveryAttemptTimeout)
		if err == nil || !sess.codexZeroTurnRecoveryNeeded() {
			return
		}
		log.Printf("zero-turn recovery resume %d/%d for thread %s (session %s): %v",
			attempt, codexZeroTurnRecoveryAttempts, req.ThreadID, sess.id, err)
		sess.observeConnection(lifecycle.EventLinkLost)
		var rpcErr *protocol.RPCError
		if !errors.As(err, &rpcErr) {
			sess.codex.requestSessionRecovery(sess)
			// Transport loss owns its replacement connection and ordinary resume.
			// The shared success path below clears the zero-turn flag if it wins.
			return
		}
		if backoff == 0 {
			backoff = codexZeroTurnRecoveryInitialBackoff
		} else {
			backoff *= 2
			if backoff > codexZeroTurnRecoveryMaximumBackoff {
				backoff = codexZeroTurnRecoveryMaximumBackoff
			}
		}
	}
	log.Printf("zero-turn recovery exhausted for thread %s (session %s); leaving session reconnecting",
		sess.ThreadID(), sess.id)
	sess.codex.requestSessionRecovery(sess)
}

// runCodexHandshake performs the thread/start or thread/resume call and
// captures the response for the Responses Input panel. Both start and resume
// launch it on a background goroutine after the starting session is published,
// so the ready transition re-checks under stateMu that the session was not
// closed while the handshake was in flight.
func runCodexHandshake(sess *Session, daemon *CodexDaemon, req ProviderRequest) error {
	return runCodexHandshakeContext(context.Background(), sess, daemon, req)
}

func runCodexHandshakeContext(parent context.Context, sess *Session, daemon *CodexDaemon, req ProviderRequest) error {
	timeout := codexHandshakeTimeout
	if req.Mode == "resume" && daemon.resumeTimeout > 0 {
		timeout = daemon.resumeTimeout
	}
	return runCodexHandshakeWithTimeout(parent, sess, daemon, req, timeout)
}

func runCodexHandshakeWithTimeout(parent context.Context, sess *Session, daemon *CodexDaemon, req ProviderRequest, timeout time.Duration) error {
	sess.codexHandshakeMu.Lock()
	defer sess.codexHandshakeMu.Unlock()
	releaseSlot := func() {}
	if req.Mode == "resume" {
		select {
		case daemon.resumeSlots <- struct{}{}:
			var release sync.Once
			releaseSlot = func() { release.Do(func() { <-daemon.resumeSlots }) }
			defer releaseSlot()
		case <-parent.Done():
			return parent.Err()
		}
		sess.stateMu.Lock()
		closed := sess.closed || sess.connectionStatusLocked() == StatusExited
		sess.stateMu.Unlock()
		if closed {
			return nil
		}
	}
	if req.Mode == "resume" {
		// A resume without a tier takes the tier of Codex's own config, not
		// the thread's: it would run at that tier until its next turn sends the
		// override, and report it as the thread's confirmed tier.
		_, _, req.ServiceTier = sess.turnOptions()
	}
	sess.beginTurnRestore()
	defer sess.cancelTurnRestore()
	sess.forgetCodexTurnInputs()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	method, params := CodexSessionRequest(req)
	// This snapshot must be taken after the exact params are built and before
	// the RPC is possible.  It is the only desired-state causation a response
	// is allowed to use.
	sent := map[string]string{}
	for _, pair := range []struct{ field, param string }{
		{postureFieldModel, "model"}, {postureFieldServiceTier, "serviceTier"},
		{"cwd", "cwd"}, {postureFieldPermissionMode, "approvalPolicy"},
	} {
		if value, ok := params[pair.param].(string); ok && value != "" {
			sent[pair.field] = value
		}
	}
	causation := sess.snapshotProviderCausation(sent)
	var threadResp json.RawMessage
	var decoded *codexResumeResponse
	wireLog := protocol.WireLogger(sess.wire.recordWire)
	if method == "thread/resume" {
		wireLog = func(dir wire.Direction, line []byte) {
			if response := sess.recordResumeWire(dir, line); response != nil {
				decoded = response
			}
		}
	}
	var responseEpoch, responseSequence uint64
	recordWire := wireLog
	wireLog = func(dir wire.Direction, line []byte) {
		if dir == protocol.Recv {
			var message protocol.Message
			if json.Unmarshal(line, &message) == nil && message.IsResponse() {
				responseEpoch, responseSequence = sess.evidenceCoordinates()
			}
		}
		recordWire(dir, line)
	}
	transport, err := daemon.callForSessionWithWireLog(ctx, sess, method, params, &threadResp, wireLog)
	// The slot bounds concurrent resume calls at the app server, so it ends
	// with the call: the post-reply work below waits on Nano's own stores, and
	// other resumes must not queue behind it.
	releaseSlot()
	if err != nil {
		// A typed RPC error is an explicit provider answer. Transport loss and
		// timeout are uncertainty, so they leave the requested creation intent
		// pending rather than fabricating a failed provider state. A failed
		// resume answers for the thread (closing, not found, no rollout), not
		// for the tier it carried.
		var rpcErr *protocol.RPCError
		if errors.As(err, &rpcErr) && method != "thread/resume" {
			sess.recordProviderRejection(method+"_rejected", causation)
		}
		return fmt.Errorf("%s: %w", method, err)
	}
	// Read the in-flight turn and hydrate the bounded normalized tail BEFORE the
	// turns array is discarded. See inFlightTurn and seedCodexResume.
	if decoded == nil {
		decoded, err = decodeCodexResumeResponse(threadResp)
		if err != nil {
			return err
		}
	}
	inFlight, resuming := decoded.inFlightTurn()
	compactResponse := decoded.compact()

	var thread struct {
		Model           string          `json:"model"`
		Cwd             string          `json:"cwd"`
		ServiceTier     string          `json:"serviceTier"`
		ReasoningEffort string          `json:"reasoningEffort"`
		ApprovalPolicy  json.RawMessage `json:"approvalPolicy"`
		Thread          struct {
			ID   string `json:"id"`
			Cwd  string `json:"cwd"`
			Path string `json:"path"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(compactResponse, &thread); err != nil {
		return err
	}
	if thread.Thread.ID == "" {
		return fmt.Errorf("%s response missing thread.id", method)
	}
	// The response names the rollout; boot's saved-catalog path stands in
	// only until this confirms or replaces it.
	sess.learnCatalogSourcePath(thread.Thread.Path)
	sess.setProjectionThreadID(thread.Thread.ID)
	if method == "thread/resume" {
		// Boot and recovery resumes both pass here, before readiness.
		sess.recordRestartStep(restartAttached)
	}
	if method == "thread/resume" && sess.projection != nil {
		// Restore action-bearing rows before the lossy provider baseline so a
		// returned user item promotes the same logical row instead of duplicating
		// it. The outbox is scrubbed only after durable/provider confirmation.
		sess.restoreSubmissionOutbox()
		seedCodexResume(sess.projection, decoded)
		sess.observeCodexResume(decoded)
		// A resume payload is explicitly lossy and can lag the active rollout.
		// Settle the open turn from its saved copy; later catalog changes
		// repeat this repair.
		sess.ReconcileDurableTurn(ctx)
		// This is the one successful-resume transition for both the session-owned
		// zero-turn task and an ordinary daemon reconnect that wins the race.

	}
	sess.setState("thread", compactResponse)
	sess.stateMu.Lock()
	if sess.connectionStatusLocked() == "exited" {
		// The session was closed (or the daemon died) while the background
		// handshake ran: do not resurrect it or its routing claim.
		sess.stateMu.Unlock()
		daemon.releaseThread(req.ThreadID, sess)
		if req.Mode != "resume" {
			// This very call minted the thread, and close() ran before there was
			// an id for it to unsubscribe — nothing else will ever let go of it.
			daemon.unsubscribeThread(sess, thread.Thread.ID)
		}
		return nil
	}
	sess.threadID = thread.Thread.ID
	sess.model = thread.Model
	if thread.Cwd != "" {
		sess.cwd = thread.Cwd
	} else if thread.Thread.Cwd != "" {
		sess.cwd = thread.Thread.Cwd
	}
	sess.stateMu.Unlock()
	// AskForApproval is a string-or-granular-object union in the checked-in v2
	// schema.  Nano only has a lossless posture representation for the string
	// form; an object remains deliberately unknown rather than failing start.
	var approval string
	_ = json.Unmarshal(thread.ApprovalPolicy, &approval)
	cwd := thread.Cwd
	if cwd == "" {
		cwd = thread.Thread.Cwd
	}
	providerValues := map[string]string{postureFieldModel: thread.Model, postureFieldEffort: thread.ReasoningEffort, postureFieldServiceTier: thread.ServiceTier, "cwd": cwd}
	if approval != "" {
		providerValues[postureFieldPermissionMode] = approval
	}
	sess.recordProviderEvidence(method+"_response", providerValues, metadataFreshnessLive, causation)
	// The native id is authoritative from here: a dialog-supplied session name
	// can finally reach the names store (start and resume both land here).
	sess.applyPendingName(thread.Thread.ID)
	// starting → ready is a runtime-visible transition (frozen R3 contract).
	// Re-attaching to a thread whose turn is still running: seed both the
	// working state and its anchor from the response's own turns array. Nothing
	// replays turn/started, so without this the row reads attached-idle while
	// the turn streams — a gap that predates the timers work and that the
	// timers make visible.
	_ = resuming // Current() chooses work from the full provider response.
	sess.stateMu.Lock()
	if sess.closed || sess.connectionStatusLocked() == StatusExited {
		sess.stateMu.Unlock()
		return nil
	}
	// Readiness is published only after replay and current provider evidence settle.
	sess.spawnError = ""
	sess.stateMu.Unlock()
	if method == "thread/resume" && !sess.awaitSavedCatchUp(transport, nil) {
		if sess.ended() {
			daemon.releaseThread(req.ThreadID, sess)
			return nil
		}
		return fmt.Errorf("%s: the app-server connection closed while the saved file caught up", method)
	}
	sess.settleProviderEvidence(codexLifecycleAdapter{epoch: responseEpoch, sequence: responseSequence, response: decoded, nativeID: thread.Thread.ID, settled: true}, inFlight.ID, inFlight.startedAtMs())
	// Persist the durable attachment now that the thread id is authoritative:
	// a restart that kills this session leaves the record behind, so the
	// thread returns as re-attachable (attachments.go). Covers start and resume
	// alike — both reach ready here.
	if sess.rememberAttachment != nil {
		sess.rememberDurableAttachment(hostedstore.Attachment{
			Provider:       "codex",
			NativeThreadID: thread.Thread.ID,
			Cwd:            sess.cwd,
			Model:          thread.Model,
		})
	}
	// A resume can lose its link while it waits for its saved catch-up, so
	// the record takes the status the settle left, not a fixed "ready".
	sess.stateMu.Lock()
	status := sess.connectionStatusLocked()
	sess.stateMu.Unlock()
	sess.updateHostedThread(status, "")
	if req.ThreadID != thread.Thread.ID {
		// Start mode (no pre-claim), or a resume answered with a different id
		// than requested (not observed in practice): route the real id and
		// drop any stale claim. A matching resume needs neither — its
		// pre-claim already IS the registration.
		daemon.registerThread(thread.Thread.ID, sess)
		daemon.releaseThread(req.ThreadID, sess)
		sess.stateMu.Lock()
		exited := sess.connectionStatusLocked() == "exited"
		sess.stateMu.Unlock()
		if exited {
			// close() slipped between the ready transition and the rebind;
			// don't leave a routing entry for a torn-down session.
			daemon.releaseThread(thread.Thread.ID, sess)
		}
	}
	if req.Mode == "resume" {
		sess.finishCodexZeroTurnRecovery()
	}
	return nil
}

// recordResumeWire keeps the outgoing thread/resume request and every error
// reply verbatim. A successful response's historical turns, however, are not
// used by any Nano view: the conversation has already loaded from the durable
// transcript. Remove them before the wire event reaches the spool, SSE, or
// browser, and leave an explicit marker in the raw Event Feed payload.
func (sess *Session) recordResumeWire(dir wire.Direction, line []byte) *codexResumeResponse {
	if dir != protocol.Recv {
		sess.wire.recordWire(dir, line)
		return nil
	}
	envelope, err := decodeCodexResumeResponse(line)
	if err != nil {
		sess.wire.recordWire(dir, line)
		return nil
	}
	if _, hasError := envelope.fields["error"]; hasError {
		sess.wire.recordWire(dir, line)
		return nil
	}
	fields, ok := envelope.fields["result"].(map[string]any)
	if !ok {
		sess.wire.recordWire(dir, line)
		return nil
	}
	response := newCodexResumeResponse(fields)
	if _, present := response.thread["turns"]; !present {
		sess.wire.recordWire(dir, line)
		return response
	}
	compact := response.compactFields()
	compact["nanoOmittedFields"] = map[string]any{
		"fields": []string{"thread.turns"},
		"reason": "Nano renders historical turns from its durable transcript.",
	}
	envelope.fields["result"] = compact
	compactLine, err := json.Marshal(envelope.fields)
	if err != nil {
		sess.wire.recordWire(dir, line)
		return response
	}
	sess.wire.recordWire(dir, compactLine)
	return response
}

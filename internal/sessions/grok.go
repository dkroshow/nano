package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	coresession "github.com/dkroshow/nano/internal/core/session"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/lifecycle"
	threadvalues "github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/wire"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
	wiregrok "github.com/dkroshow/nano/internal/wire/grok"
)

const (
	grokPermissionMethod = "session/request_permission"
	// The leader's two mid-turn reverse requests. Both park the turn on the
	// client's answer; refusing either by name is read as "client disconnected
	// mid-approval" and cancels the whole turn (observed 2026-08-13: a hosted
	// session died on every exit_plan_mode). Neither is documented — the
	// response shapes below were pinned by probing a live 1.0.3 leader.
	grokExitPlanMethod    = "_x.ai/exit_plan_mode"
	grokAskQuestionMethod = "_x.ai/ask_user_question"
	// GrokJSONRPCVersion is the JSON-RPC version used by the Grok transport.
	GrokJSONRPCVersion = "2.0"
	// GrokProtocolVersion is the supported Grok ACP protocol version.
	GrokProtocolVersion    = 1
	grokHandshakeTimeout   = 30 * time.Second
	grokBootHandshakeLimit = 4
	grokBootLoadAttempts   = 3
	grokBootBackoffBase    = 250 * time.Millisecond
	grokDisableUpdater     = "GROK_DISABLE_AUTOUPDATER=1"
)

// grokSessionCallError keeps the failed ACP method typed so retry policy does
// not infer a session/load timeout from error text.
type grokSessionCallError struct {
	method string
	err    error
}

// Error returns the error message exposed to the caller.
func (e *grokSessionCallError) Error() string { return e.method + ": " + e.err.Error() }

// Unwrap returns the underlying transport error.
func (e *grokSessionCallError) Unwrap() error { return e.err }

// grokSession is the connection-bound ACP proxy. The Grok leader, not this
// child, owns the native session and continues turns when Nano restarts.
type grokSession struct {
	sess   *Session
	client *protocol.Client

	mu              sync.Mutex
	active          *grokPrompt
	sending         bool
	seenPromptIDs   map[string]struct{}
	seenPromptOrder []string
	closed          bool
}

type grokPrompt struct {
	accepted chan string
	finished chan grokPromptOutcome
	once     sync.Once
	mu       sync.Mutex
	id       string
	text     string
	actionID string
}

type grokPromptResponse struct {
	StopReason string `json:"stopReason"`
	Meta       struct {
		PromptID string `json:"promptId"`
	} `json:"_meta"`
}

type grokPromptOutcome struct {
	response grokPromptResponse
	err      error
}

func newGrokPrompt(text, actionID string) *grokPrompt {
	return &grokPrompt{
		accepted: make(chan string, 1), finished: make(chan grokPromptOutcome, 1),
		text: text, actionID: actionID,
	}
}

func (p *grokPrompt) accept(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	p.once.Do(func() {
		p.mu.Lock()
		p.id = id
		p.mu.Unlock()
		p.accepted <- id
	})
}

// ResolveGrokBin resolves the configured or environment-selected Grok executable.
func ResolveGrokBin(override string) string {
	if value := strings.TrimSpace(override); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("CODEX_NANO_GROK_BIN")); value != "" {
		return value
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, candidate := range []string{
			filepath.Join(home, ".grok", "bin", "grok"),
			filepath.Join(home, ".local", "bin", "grok"),
		} {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate
			}
		}
	}
	if path, err := exec.LookPath("grok"); err == nil {
		return path
	}
	return "grok"
}

// GrokCommand builds the executable and arguments for a resolved Grok request.
func GrokCommand(req ProviderRequest) []string {
	bin := ResolveGrokBin(req.GrokBin)
	args := []string{"--sandbox", "off"}
	args = append(args, "agent")
	if req.Mode != "resume" {
		if model := strings.TrimSpace(req.Model); model != "" {
			args = append(args, "--model", model)
		}
		if effort := strings.TrimSpace(req.Effort); effort != "" {
			args = append(args, "--reasoning-effort", effort)
		}
	}
	args = append(args, "--leader")
	if socket := strings.TrimSpace(req.GrokLeaderSocket); socket != "" {
		args = append(args, "--leader-socket", socket)
	}
	args = append(args, "stdio")
	return append([]string{bin}, args...)
}

// GrokSessionRequest is shared by the live handshake and New Window preview.
// The durable leader creates the native session from session/new, so effort
// must ride this request rather than only the short-lived stdio client's argv.
// Grok 1.0.3 exposes only auto and always-approve as per-session ACP options;
// the other permission modes remain configuration/CLI concerns.
func GrokSessionRequest(req ProviderRequest) (string, map[string]any) {
	if req.Mode == "resume" {
		return "session/load", map[string]any{
			"cwd": req.Cwd, "mcpServers": []any{}, "sessionId": req.ThreadID,
		}
	}
	params := map[string]any{"cwd": req.Cwd, "mcpServers": []any{}}
	meta := map[string]any{}
	if effort := strings.TrimSpace(req.Effort); effort != "" {
		meta["reasoningEffort"] = effort
	}
	switch req.PermissionMode {
	case "auto":
		meta["autoMode"] = true
	case "bypassPermissions":
		meta["yoloMode"] = true
	}
	if len(meta) > 0 {
		params["_meta"] = meta
	}
	return "session/new", params
}

// grokSelectedEffort reads the provider's session-scoped selection. The model
// catalog's reasoningEffort is only a default and is not execution evidence.
func grokSelectedEffort(raw json.RawMessage) (string, error) {
	var body struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", fmt.Errorf("decode Grok session response: %w", err)
	}
	var config struct {
		Options []struct {
			Category string `json:"category"`
			ID       string `json:"id"`
			Selected bool   `json:"selected"`
		} `json:"options"`
	}
	if err := json.Unmarshal(body.Meta["x.ai/sessionConfig"], &config); err != nil {
		return "", errors.New("Grok session response omitted x.ai/sessionConfig")
	}
	selected := ""
	for _, option := range config.Options {
		if option.Category != "mode" || !option.Selected {
			continue
		}
		if selected != "" {
			return "", fmt.Errorf("Grok session response selected multiple reasoning efforts: %s and %s", selected, option.ID)
		}
		selected = strings.TrimSpace(option.ID)
	}
	if selected == "" {
		return "", errors.New("Grok session response reported no selected reasoning effort")
	}
	return selected, nil
}

// grokVerifyRequestedEffort rejects a fresh session whose provider-selected
// effort differs from Nano's request. A resumed session is observed but not
// compared with creation intent because it already has provider-owned state.
func grokVerifyRequestedEffort(req ProviderRequest, raw json.RawMessage) (string, error) {
	selected, err := grokSelectedEffort(raw)
	if err != nil {
		return "", err
	}
	requested := strings.TrimSpace(req.Effort)
	if req.Mode != "resume" && requested != "" && selected != requested {
		return selected, fmt.Errorf("Grok selected reasoning effort %s, requested %s", selected, requested)
	}
	return selected, nil
}

// GrokProcessEnv prevents a short-lived Nano client from upgrading the shared
// Grok installation as a side effect of connecting. The supervised leader has
// its own --no-auto-update flag, but agent stdio does not expose that flag; the
// process-scoped environment variable is Grok's supported equivalent.
func GrokProcessEnv() []string {
	const prefix = "GROK_DISABLE_AUTOUPDATER="
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, grokDisableUpdater)
}

// GrokProbeCwd returns an existing directory for provider-wide ACP probes.
// Model discovery and billing do not create or load a session, so they must
// not fail merely because Nano was deployed from a checkout that was later
// removed. Real session starts still use and validate their requested cwd.
func GrokProbeCwd(configured string) string {
	home, _ := os.UserHomeDir()
	for _, candidate := range []string{strings.TrimSpace(configured), home} {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return string(filepath.Separator)
}

func startGrokSession(sess *Session, req ProviderRequest) {
	go func() {
		defer req.BootReattach.holdHandshakeSlot(sess)()
		if grokSessionClosed(sess) {
			sess.stateOwner.recordRestartFailure("closed")
			return
		}
		err := runGrokHandshake(sess, req)
		if req.BootReattach != nil {
			err = retryGrokBootLoad(sess, req, err)
		}
		if err == nil {
			sess.stateOwner.recordRestartStep(restartAttached)
		}
		if err != nil {
			if errors.Is(err, errSpawnSessionClosed) {
				sess.stateOwner.recordRestartFailure("closed")
				return
			}
			log.Printf("start grok session %s: %v", sess.id, err)
			sess.stateOwner.failSpawn(err.Error())
		}
	}()
}

// retryGrokBootLoad is deliberately narrower than general Grok recovery. A
// fresh start, manual resume, initialize failure, or provider refusal keeps the
// existing one-attempt behavior.
func retryGrokBootLoad(sess *Session, req ProviderRequest, firstErr error) error {
	err := firstErr
	for attempt := 1; attempt <= grokBootLoadAttempts; attempt++ {
		if err == nil || !grokLoadTimedOut(err) {
			return err
		}
		if attempt == 1 {
			sess.stateOwner.observeConnection(lifecycle.EventLinkLost)
		}
		if attempt == grokBootLoadAttempts {
			return err
		}
		backoff := grokBootLoadBackoff(attempt, req.BootReattach)
		log.Printf("grok boot re-attach session %s: session/load timed out; retry %d/%d in %s", sess.id, attempt+1, grokBootLoadAttempts, backoff)
		time.Sleep(backoff)
		if grokSessionClosed(sess) {
			return errSpawnSessionClosed
		}
		err = runGrokHandshake(sess, req)
	}
	return err
}

func grokLoadTimedOut(err error) bool {
	var callErr *grokSessionCallError
	return errors.As(err, &callErr) && callErr.method == "session/load" && errors.Is(callErr, context.DeadlineExceeded)
}

func grokBootLoadBackoff(attempt int, control *BootControl) time.Duration {
	// Only failures one and two sleep. The production base therefore bounds the
	// three-attempt schedule at 500 milliseconds; tests use a shorter base.
	base := grokBootBackoffBase
	if control != nil && control.retryBackoffBase > 0 {
		base = control.retryBackoffBase
	}
	return base << (attempt - 1)
}

func grokSessionClosed(sess *Session) bool {
	sess.stateOwner.stateMu.Lock()
	defer sess.stateOwner.stateMu.Unlock()
	return sess.stateOwner.closed
}

func runGrokHandshake(sess *Session, req ProviderRequest) error {
	sess.stateOwner.beginTurnRestore()
	defer sess.stateOwner.cancelTurnRestore()
	command := GrokCommand(req)
	if len(command) == 0 || command[0] == "" {
		return errors.New("grok executable is not configured")
	}
	var grok *grokSession
	client, err := wiregrok.StartProcess(wiregrok.ProcessConfig{
		Command: command,
		Dir:     req.Cwd,
		Env:     GrokProcessEnv(),
	}, protocol.NewClientCmd, func(client *protocol.Client) {
		client.JSONRPCVersion = GrokJSONRPCVersion
		grok = &grokSession{sess: sess, client: client, seenPromptIDs: make(map[string]struct{})}
		client.WireLog = grok.recordWire
		client.OnServerRequest = grok.handleServerRequest
	})
	if err != nil {
		return fmt.Errorf("start grok ACP proxy: %w", err)
	}

	sess.stateOwner.stateMu.Lock()
	if sess.stateOwner.closed {
		sess.stateOwner.stateMu.Unlock()
		_ = client.Close()
		return errSpawnSessionClosed
	}
	sess.stateOwner.grok = grok
	if req.Mode == "resume" {
		sess.stateOwner.threadID = req.ThreadID
		sess.stateOwner.model = req.GrokResumeModel
	}
	sess.stateOwner.stateMu.Unlock()
	if req.Mode == "resume" {
		sess.stateOwner.setProjectionThreadID(req.ThreadID)
		sess.stateOwner.restoreSubmissionOutbox()
	}

	ctx, cancel := context.WithTimeout(context.Background(), grokHandshakeTimeout)
	defer cancel()
	var initialize json.RawMessage
	if err := client.Call(ctx, "initialize", map[string]any{
		"protocolVersion": GrokProtocolVersion,
		// Declaring terminal or fs capabilities delegates those effects to Nano.
		// Nano does not implement them, so V1 deliberately advertises none.
		"clientCapabilities": map[string]any{},
	}, &initialize); err != nil {
		_ = client.Close()
		return fmt.Errorf("initialize grok ACP: %w", err)
	}
	initialize = sanitizeGrokWire(initialize)
	sess.stateOwner.setState("grokInitialize", initialize)

	method, params := GrokSessionRequest(req)
	sentPosture := map[string]string{}
	if req.Mode != "resume" {
		if model := strings.TrimSpace(req.Model); model != "" {
			sentPosture[postureFieldModel] = model
		}
		if effort := strings.TrimSpace(req.Effort); effort != "" {
			sentPosture[postureFieldEffort] = effort
		}
	}
	causation := sess.stateOwner.snapshotProviderCausation(sentPosture)
	var sessionResult json.RawMessage
	var responseEpoch, responseSequence uint64
	loadCtx := ctx
	loadCancel := func() {}
	if req.BootReattach != nil && req.BootReattach.loadTimeout > 0 {
		loadCtx, loadCancel = context.WithTimeout(ctx, req.BootReattach.loadTimeout)
	}
	defer loadCancel()
	if err := client.CallWithWireLog(loadCtx, method, params, &sessionResult, func(dir wire.Direction, line []byte) {
		// Bind ordering to this request's response, even when load omits its id.
		if dir == protocol.Recv {
			responseEpoch, responseSequence = sess.stateOwner.evidenceCoordinates()
		}
		grok.recordWire(dir, line)
	}); err != nil {
		_ = client.Close()
		return &grokSessionCallError{method: method, err: err}
	}
	sessionResult = sanitizeGrokWire(sessionResult)
	sess.stateOwner.setState("grokSession", sessionResult)

	threadID := grokSessionID(sessionResult)
	if req.Mode == "resume" && threadID != "" && threadID != req.ThreadID {
		_ = client.Close()
		return fmt.Errorf("%s returned a different sessionId", method)
	}
	if threadID == "" {
		threadID = req.ThreadID
	}
	if threadID == "" {
		_ = client.Close()
		return fmt.Errorf("%s returned no sessionId", method)
	}
	if req.Mode == "resume" {
		sess.runtime.reconcileGrokLoadedTurn(sessionResult)
	}
	providerModel := grokCurrentModel(sessionResult)
	selectedEffort, effortErr := grokVerifyRequestedEffort(req, sessionResult)
	model := threadvalues.FirstNonEmpty(providerModel, grokCurrentModel(initialize), req.GrokResumeModel, req.Model)

	sess.stateOwner.stateMu.Lock()
	if sess.stateOwner.closed {
		sess.stateOwner.stateMu.Unlock()
		_ = client.Close()
		return errSpawnSessionClosed
	}
	sess.stateOwner.threadID = threadID
	sess.stateOwner.model = model
	sess.stateOwner.turnModel = ""
	sess.stateOwner.stateMu.Unlock()
	providerPosture := map[string]string{}
	if providerModel != "" {
		providerPosture[postureFieldModel] = providerModel
	}
	if selectedEffort != "" {
		providerPosture[postureFieldEffort] = selectedEffort
	}
	sess.stateOwner.recordProviderEvidence("grok_"+strings.ReplaceAll(method, "/", "_")+"_response", providerPosture, metadataFreshnessLive, causation)
	if effortErr != nil {
		_ = client.Close()
		return fmt.Errorf("verify %s reasoning effort: %w", method, effortErr)
	}
	sess.stateOwner.setProjectionThreadID(threadID)
	sess.stateOwner.applyPendingName(threadID)
	sess.stateOwner.rememberDurableAttachment(hostedstore.Attachment{
		Provider: "grok", NativeThreadID: threadID, Cwd: req.Cwd, Model: model,
		AttachedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	// Watched before any wait, so a proxy that exits while the saved file
	// catches up is a lost link, never a ready thread.
	go func() {
		wiregrok.WatchDisconnect(client, func() {
			grok.mu.Lock()
			closed := grok.closed
			grok.mu.Unlock()
			if !closed {
				log.Printf("grok ACP proxy for session %s exited", sess.id)
				sess.stateOwner.observeConnection(lifecycle.EventLinkLost)
			}
		})
	}()
	neverPrompted := func() bool { return sess.runtime.grokNeverPrompted(ctx) }
	if req.Mode == "resume" && !sess.stateOwner.awaitSavedCatchUp(client.Done(), neverPrompted) {
		if sess.stateOwner.ended() {
			return errSpawnSessionClosed
		}
		return nil // the lost link is already observed
	}
	if !sess.stateOwner.settleProviderEvidence(grokLifecycleAdapter{epoch: responseEpoch, sequence: responseSequence, response: sessionResult, nativeID: threadID, settled: true}, grokRunningPromptID(sessionResult), 0) {
		log.Printf("grok session %s: %s response did not settle lifecycle evidence", sess.id, method)
	}
	sess.delivery.reconcileAgentOperations()
	return nil
}

// grokNeverPrompted reports positive evidence that no client has prompted
// this session: Nano created it (its session/new answer named this exact
// native session), and the delivery journal of that native session holds no
// attempt that ever left the queue. A session Nano did not create, or any
// attempt sent, parked or of unknown outcome, proves nothing.
func (sess *Runtime) grokNeverPrompted(ctx context.Context) bool {
	native := sess.session.stateOwner.ThreadID()
	if sess.session.delivery.turns == nil || sess.session.metadata == nil || native == "" {
		return false
	}
	attempts, err := sess.session.delivery.turns.ListThreadAttempts(ctx, "grok", native)
	if err != nil {
		return false
	}
	for _, entry := range attempts {
		if entry.Attempt.State != prompts.JournalQueued || entry.OutboxState != prompts.Queued {
			return false
		}
	}
	for after := int64(0); ; {
		page, err := sess.session.metadata.EventsAfter(sess.session.hostedThreadID, after, 200, hostedstore.MetadataEventFilter{Source: postureSourceProvider})
		if err != nil || len(page.Events) == 0 {
			return false
		}
		for _, event := range page.Events {
			if event.Evidence == "grok_session_new_response" && event.NativeThreadID == native {
				return true
			}
		}
		after = page.NextCursor
	}
}

func grokSessionID(raw json.RawMessage) string {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	if id := wire.String(body["sessionId"]); id != "" {
		return id
	}
	// session/load puts its identity in _meta; session/new uses the top level.
	var meta map[string]json.RawMessage
	if json.Unmarshal(body["_meta"], &meta) == nil {
		if id := wire.String(meta["sessionId"]); id != "" {
			return id
		}
		var detail map[string]json.RawMessage
		if json.Unmarshal(meta["x.ai/sessionDetail"], &detail) == nil {
			if id := wire.String(detail["sessionId"]); id != "" {
				return id
			}
		}
	}
	var info map[string]json.RawMessage
	if json.Unmarshal(body["info"], &info) == nil {
		return wire.String(info["id"])
	}
	return ""
}

func grokCurrentModel(raw json.RawMessage) string {
	var body struct {
		Meta struct {
			ModelState struct {
				CurrentModelID string `json:"currentModelId"`
			} `json:"modelState"`
		} `json:"_meta"`
		Models struct {
			CurrentModelID string `json:"currentModelId"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	if model := threadvalues.FirstNonEmpty(body.Meta.ModelState.CurrentModelID, body.Models.CurrentModelID); model != "" {
		return model
	}
	var top map[string]json.RawMessage
	var meta map[string]json.RawMessage
	var detail map[string]json.RawMessage
	if json.Unmarshal(raw, &top) == nil && json.Unmarshal(top["_meta"], &meta) == nil &&
		json.Unmarshal(meta["x.ai/sessionDetail"], &detail) == nil {
		return wire.String(detail["currentModelId"])
	}
	return ""
}

// FetchGrokModels reads the provider model catalog for a resolved request.
func FetchGrokModels(ctx context.Context, request ProviderRequest) (json.RawMessage, error) {
	command := GrokCommand(request)
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = GrokProbeCwd(request.Cwd)
	cmd.Env = GrokProcessEnv()
	client := protocol.NewClientCmd(cmd)
	client.JSONRPCVersion = GrokJSONRPCVersion
	if err := client.StartContext(ctx); err != nil {
		return nil, fmt.Errorf("start Grok model probe: %w", err)
	}
	defer client.Close()
	var initialize json.RawMessage
	if err := client.Call(ctx, "initialize", map[string]any{
		"protocolVersion": GrokProtocolVersion, "clientCapabilities": map[string]any{},
	}, &initialize); err != nil {
		return nil, err
	}
	return normalizeGrokModelCatalog(initialize), nil
}

func normalizeGrokModelCatalog(initialize json.RawMessage) json.RawMessage {
	var top map[string]json.RawMessage
	var meta map[string]json.RawMessage
	var state map[string]json.RawMessage
	if json.Unmarshal(initialize, &top) != nil || json.Unmarshal(top["_meta"], &meta) != nil || json.Unmarshal(meta["modelState"], &state) != nil {
		return json.RawMessage(`{"data":[]}`)
	}
	current := wire.String(state["currentModelId"])
	var available []map[string]json.RawMessage
	_ = json.Unmarshal(state["availableModels"], &available)
	data := make([]map[string]any, 0, len(available))
	for _, model := range available {
		id := threadvalues.FirstNonEmpty(wire.String(model["modelId"]), wire.String(model["model"]), wire.String(model["id"]))
		if id == "" {
			continue
		}
		var modelMeta map[string]json.RawMessage
		_ = json.Unmarshal(model["_meta"], &modelMeta)
		var rawEfforts []map[string]json.RawMessage
		if json.Unmarshal(modelMeta["reasoningEfforts"], &rawEfforts) != nil {
			_ = json.Unmarshal(model["reasoningEfforts"], &rawEfforts)
		}
		efforts := make([]map[string]any, 0, len(rawEfforts))
		defaultEffort := threadvalues.FirstNonEmpty(wire.String(modelMeta["reasoningEffort"]), wire.String(model["reasoningEffort"]))
		for _, rawEffort := range rawEfforts {
			effort := threadvalues.FirstNonEmpty(wire.String(rawEffort["reasoningEffort"]), wire.String(rawEffort["id"]), wire.String(rawEffort["value"]))
			if effort == "" {
				continue
			}
			efforts = append(efforts, map[string]any{
				"reasoningEffort": effort,
				"label":           threadvalues.FirstNonEmpty(wire.String(rawEffort["label"]), wire.String(rawEffort["name"]), effort),
			})
			if defaultEffort == "" && (wire.Bool(rawEffort["isDefault"]) || wire.Bool(rawEffort["default"])) {
				defaultEffort = effort
			}
		}
		data = append(data, map[string]any{
			"model": id, "displayName": threadvalues.FirstNonEmpty(wire.String(model["name"]), wire.String(model["displayName"]), id),
			"isDefault": id == current, "defaultReasoningEffort": defaultEffort,
			"supportedReasoningEfforts": efforts,
		})
	}
	out, _ := json.Marshal(map[string]any{"data": data})
	return out
}

// grokRunningPromptID is the provider's authoritative resume-time activity
// signal. Historical updates can replay before or after session/load returns,
// so an unfinished prompt in that replay is not proof that the current leader
// still owns it. Grok 1.0.3 exposes the live prompt only in this response field.
func grokRunningPromptID(raw json.RawMessage) string {
	var top map[string]json.RawMessage
	var meta map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil || json.Unmarshal(top["_meta"], &meta) != nil {
		return ""
	}
	return wire.String(meta["x.ai/runningPromptId"])
}

func (sess *Runtime) reconcileGrokLoadedTurn(sessionResult json.RawMessage) {
	if runningPromptID := grokRunningPromptID(sessionResult); runningPromptID != "" {
		if grokSyntheticPromptID(runningPromptID) {
			// A provider-generated child-completion continuation is not an
			// operator turn. Its terminal is raw-only, so restoring it as active
			// would create a working state that no admitted event could clear.
			sess.session.stateOwner.setTurnActive(false, "", 0, false)
			return
		}
		sess.session.stateOwner.setTurnActive(true, runningPromptID, 0, false)
		return
	}

	// The provider says there is no running prompt in this leader. Clear any
	// process-local state and settle durable operations without inventing a turn
	// end time. Late replay cannot reassert activity because recordWireLive
	// excludes Grok replay from present-tense lifecycle evidence.
	sess.session.stateOwner.setTurnActive(false, "", 0, false)
	if sess.session.agentExecutions != nil && sess.session.hostedThreadID != "" {
		err := sess.session.agentExecutions.MarkSessionTurns(sess.session.hostedThreadID, agentTurnInterrupted, &threadvalues.ExecutionError{
			Source: "transport", Code: "grok_prompt_missing_after_reload",
			Message: "Grok no longer has the prompt that was unfinished in session replay",
		})
		if err != nil {
			log.Printf("settle missing Grok prompt for hosted thread %s: %v", sess.session.hostedThreadID, err)
		}
	}
}

func (g *grokSession) recordWire(dir wire.Direction, line []byte) {
	// Grok 1.0.3 multiplexes child traffic on the parent's ACP connection.
	// Keep it inspectable, but never let another native session mutate parent
	// conversation, runtime, delivery, or hosted-operation state.
	if dir == protocol.Recv && !g.wireMayAffectParentSemantics(line) {
		g.sess.wire.recordWireRaw(dir, sanitizeGrokWire(line))
		return
	}
	if dir == protocol.Recv {
		g.mu.Lock()
		active := g.active
		candidate := ""
		if active != nil {
			candidate = grokAcceptancePromptID(line, active.text, g.sess.stateOwner.ThreadID())
		}
		observed := grokPromptIDFromWire(line)
		_, previouslySeen := g.seenPromptIDs[observed]
		if observed != "" {
			g.rememberPromptIDLocked(observed)
		}
		g.mu.Unlock()
		if active != nil && candidate != "" && !previouslySeen {
			active.accept(candidate)
		}
	}
	live := true
	if dir == protocol.Recv && grokWireIsReplay(line) {
		live = false
	}
	g.sess.wire.recordWireLive(dir, sanitizeGrokWire(line), live)
}

// wireMayAffectParentSemantics is the ingress gate for every Grok method that
// can otherwise reach parent projection, lifecycle, delivery, operation, or
// reverse-request code. A missing session id is not parent evidence for any
// Session-scoped notification. The one exception is prompt_complete, which
// Grok can send without a session id; it is safe only after this attachment
// already bound that exact prompt to its active hosted operation.
func (g *grokSession) wireMayAffectParentSemantics(line []byte) bool {
	if !g.wireBelongsToAttachedSession(line) {
		return false
	}
	var envelope struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &envelope) != nil {
		return true
	}
	if _, scoped := grokWireSessionID(line); scoped {
		if promptID, _, _, terminal := grokTerminalFromWire(line); terminal && grokSyntheticPromptID(promptID) {
			return false
		}
		return true
	}
	switch envelope.Method {
	case "session/update", "_x.ai/session/update", "_x.ai/session_notification", "_x.ai/queue/changed":
		return false
	case "_x.ai/session/prompt_complete":
		var params struct {
			PromptID string `json:"promptId"`
		}
		if json.Unmarshal(envelope.Params, &params) != nil || params.PromptID == "" {
			return false
		}
		g.mu.Lock()
		active := g.active
		g.mu.Unlock()
		if active == nil {
			return false
		}
		active.mu.Lock()
		bound := active.id
		active.mu.Unlock()
		return bound != "" && bound == params.PromptID
	default:
		if promptID, _, _, terminal := grokTerminalFromWire(line); terminal && grokSyntheticPromptID(promptID) {
			return false
		}
		return true
	}
}

// wireBelongsToAttachedSession is the single ingress predicate for every
// Session-scoped Grok wire method. Methods without a session id remain raw and
// may reach parent semantics only through their separately-bound prompt rules.
func (g *grokSession) wireBelongsToAttachedSession(line []byte) bool {
	sessionID, scoped := grokWireSessionID(line)
	if !scoped {
		return true
	}
	attached := g.sess.stateOwner.ThreadID()
	// Session creation/load tests have not attached the returned native id
	// yet. Production parent traffic always has one, so this is only a safe
	// pre-attachment compatibility path.
	return attached == "" || sessionID == attached
}

func grokWireSessionID(line []byte) (string, bool) {
	return wiregrok.SessionID(line)
}

func grokWireIsReplay(line []byte) bool {
	return wiregrok.IsReplay(line)
}

const maxSeenGrokPromptIDs = 256

func (g *grokSession) rememberPromptIDLocked(promptID string) {
	promptID = strings.TrimSpace(promptID)
	if promptID == "" {
		return
	}
	if g.seenPromptIDs == nil {
		g.seenPromptIDs = make(map[string]struct{})
	}
	if _, exists := g.seenPromptIDs[promptID]; exists {
		return
	}
	g.seenPromptIDs[promptID] = struct{}{}
	g.seenPromptOrder = append(g.seenPromptOrder, promptID)
	if len(g.seenPromptOrder) > maxSeenGrokPromptIDs {
		oldest := g.seenPromptOrder[0]
		g.seenPromptOrder = g.seenPromptOrder[1:]
		delete(g.seenPromptIDs, oldest)
	}
}

// Prompt acceptance is deliberately narrower than general prompt identity.
// Grok's queue event carries both the native id and the exact submitted text,
// which keeps replayed historical updates from binding a new API operation.
func grokAcceptancePromptID(line []byte, submittedText, attachedSessionID string) string {
	return wiregrok.AcceptancePromptID(line, submittedText, attachedSessionID)
}

func grokSyntheticPromptID(id string) bool {
	return wiregrok.IsSyntheticPromptID(id)
}

// sanitizeGrokWire keeps the inspectable protocol shape while removing fields
// known to duplicate unbounded command output or carry host/account identity.
// The ACP client still processes the original line; only Nano's feed/spool is
// redacted.
func sanitizeGrokWire(line []byte) []byte {
	return wiregrok.Sanitize(line)
}

func grokPromptIDFromWire(line []byte) string {
	var envelope struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(line, &envelope) != nil {
		return ""
	}
	if len(envelope.Result) > 0 {
		var result struct {
			Meta struct {
				PromptID string `json:"promptId"`
			} `json:"_meta"`
		}
		if json.Unmarshal(envelope.Result, &result) == nil && result.Meta.PromptID != "" {
			return result.Meta.PromptID
		}
	}
	if envelope.Method == "_x.ai/queue/changed" {
		var params struct {
			RunningPromptID string `json:"runningPromptId"`
			Entries         []struct {
				ID string `json:"id"`
			} `json:"entries"`
		}
		if json.Unmarshal(envelope.Params, &params) == nil {
			if params.RunningPromptID != "" && !grokSyntheticPromptID(params.RunningPromptID) {
				return params.RunningPromptID
			}
			for _, entry := range params.Entries {
				if entry.ID != "" && !grokSyntheticPromptID(entry.ID) {
					return entry.ID
				}
			}
		}
	}
	if prompt, _ := grokEvidencePrompt(grokWireEvidence(line)); prompt != "" {
		return prompt
	}
	if envelope.Method == "_x.ai/session/prompt_complete" {
		var params struct {
			PromptID string `json:"promptId"`
		}
		_ = json.Unmarshal(envelope.Params, &params)
		return params.PromptID
	}
	return ""
}

func grokTerminalFromWire(line []byte) (promptID, status string, usage json.RawMessage, ok bool) {
	if observation, reason, found := grokEvidenceTerminal(grokWireEvidence(line)); found {
		if observation.Scope.NativeTurnID != nil {
			promptID = *observation.Scope.NativeTurnID
		}
		// Preserve the provider's usage object for the operational API; the
		// module separately validates counters and their contribution grain.
		var detail struct {
			Usage json.RawMessage `json:"usage"`
		}
		_ = json.Unmarshal(observation.Detail, &detail)
		return promptID, grokTurnStatus(reason), detail.Usage, true
	}
	var envelope struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &envelope) != nil || envelope.Method != "_x.ai/session/prompt_complete" {
		return "", "", nil, false
	}
	var params struct {
		PromptID   string `json:"promptId"`
		StopReason string `json:"stopReason"`
	}
	if json.Unmarshal(envelope.Params, &params) != nil || params.PromptID == "" {
		return "", "", nil, false
	}
	return params.PromptID, grokTurnStatus(params.StopReason), nil, true
}

func grokTurnStatus(stopReason string) string {
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "end_turn", "completed":
		return agentTurnCompleted
	case "cancelled", "canceled", "interrupted":
		return agentTurnInterrupted
	default:
		return agentTurnFailed
	}
}

// SendTurn delivers prompt text through this provider transport.
func (g *grokSession) SendTurn(ctx context.Context, text, actionID string) (json.RawMessage, error) {
	prompt := newGrokPrompt(text, actionID)
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, errors.New("grok ACP proxy is closed")
	}
	if g.active != nil || g.sending {
		g.mu.Unlock()
		return nil, errors.New("grok already has a prompt in flight")
	}
	g.sending = true
	g.mu.Unlock()

	go func() {
		var response grokPromptResponse
		err := g.client.CallWithWriteAck(context.Background(), "session/prompt", map[string]any{
			"sessionId": g.sess.stateOwner.ThreadID(),
			"prompt": []map[string]any{{"type": "text", "text": text,
				"_meta": map[string]string{"x.nano/submissionId": SubmissionEventID(g.sess.hostedThreadID, actionID)}}},
		}, &response, func() {
			g.mu.Lock()
			if !g.closed {
				g.active = prompt
			}
			g.sending = false
			g.mu.Unlock()
		})
		prompt.accept(response.Meta.PromptID)
		prompt.finished <- grokPromptOutcome{response: response, err: err}
		g.mu.Lock()
		g.sending = false
		if g.active == prompt {
			g.active = nil
		}
		g.mu.Unlock()
	}()

	select {
	case promptID := <-prompt.accepted:
		go g.observePromptOutcome(prompt)
		result, _ := json.Marshal(map[string]any{"turn": map[string]string{"id": promptID}})
		return result, nil
	case outcome := <-prompt.finished:
		if outcome.err == nil {
			prompt.mu.Lock()
			promptID := prompt.id
			prompt.mu.Unlock()
			if promptID != "" {
				go g.observeResolvedPromptOutcome(promptID, outcome)
				result, _ := json.Marshal(map[string]any{"turn": map[string]string{"id": promptID}})
				return result, nil
			}
			outcome.err = errors.New("grok accepted the prompt without a promptId")
		}
		return nil, outcome.err
	case <-ctx.Done():
		go g.observeTimedOutPrompt(prompt, actionID)
		return nil, ctx.Err()
	}
}

func (g *grokSession) observeTimedOutPrompt(prompt *grokPrompt, actionID string) {
	outcome := <-prompt.finished
	prompt.mu.Lock()
	promptID := prompt.id
	prompt.mu.Unlock()
	if promptID != "" {
		result, _ := json.Marshal(map[string]any{"turn": map[string]string{"id": promptID}})
		g.sess.delivery.recordAgentTurnDelivery(actionID, result, nil, false)
	} else {
		var rpcErr *protocol.RPCError
		if errors.As(outcome.err, &rpcErr) {
			g.sess.delivery.recordAgentTurnDelivery(actionID, nil, outcome.err, false)
		}
	}
	g.observeResolvedPromptOutcome(promptID, outcome)
}

func (g *grokSession) observePromptOutcome(prompt *grokPrompt) {
	outcome := <-prompt.finished
	prompt.mu.Lock()
	promptID := prompt.id
	prompt.mu.Unlock()
	g.observeResolvedPromptOutcome(promptID, outcome)
}

// grokQueuedOutcomeBound caps how long a completed RPC outcome waits behind a
// stalled or closed session mailbox before it completes the operation directly.
var grokQueuedOutcomeBound = 30 * time.Second

func (g *grokSession) observeResolvedPromptOutcome(promptID string, outcome grokPromptOutcome) {
	if promptID == "" {
		return
	}
	if outcome.err != nil {
		var rpcErr *protocol.RPCError
		if !errors.As(outcome.err, &rpcErr) {
			// Transport loss is not provider failure. The native leader may still
			// be running; session exit owns the delivery_unknown transition and a
			// later session/load replay may settle it.
			return
		}
		detail, _ := json.Marshal(map[string]string{"error": outcome.err.Error()})
		g.sess.delivery.observeGrokAgentTerminal(g.sess.stateOwner.ThreadID(), promptID, agentTurnFailed, "prompt_error", detail)
		return
	}
	if strings.TrimSpace(outcome.response.StopReason) != "" {
		status := grokTurnStatus(outcome.response.StopReason)
		threadID := g.sess.stateOwner.ThreadID()
		apply := func() {
			g.sess.delivery.observeGrokAgentTerminal(threadID, promptID, status, outcome.response.StopReason, nil)
		}
		core := g.sess.runtime.coreSession()
		if status != agentTurnCompleted || core == nil {
			apply()
			return
		}
		// The reader hands this RPC result straight to SendTurn, but the
		// notifications before it, including the final agent_message_chunk
		// frames, still wait in the core mailbox. Completing now would read a
		// projection without them and keep a partial finalResponse,
		// so queue the completion behind them. The first of the queued apply
		// and the bound wins; markTurnTerminal ignores any later terminal.
		var once sync.Once
		timer := time.AfterFunc(grokQueuedOutcomeBound, func() { once.Do(apply) })
		core.ObserveAsync(coresession.AdapterEvent{LegacyApply: func() error {
			timer.Stop()
			once.Do(apply)
			return nil
		}})
	}
}

func (g *grokSession) interrupt() error {
	return g.client.Notify("session/cancel", map[string]any{"sessionId": g.sess.stateOwner.ThreadID()})
}

// Close closes the provider transport and releases its attachment resources.
func (g *grokSession) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	g.mu.Unlock()
	return g.client.Close()
}

func (g *grokSession) handleServerRequest(requestID, method string, params json.RawMessage) (any, *protocol.RPCError) {
	switch method {
	case grokPermissionMethod, grokExitPlanMethod, grokAskQuestionMethod:
	default:
		return nil, &protocol.RPCError{Code: -32601, Message: "nano does not implement Grok request " + method}
	}
	var body struct {
		SessionID   string `json:"sessionId"`
		ToolCallID  string `json:"toolCallId"` // ext methods carry it top-level
		PlanContent string `json:"planContent"`
		ToolCall    struct {
			ToolCallID string          `json:"toolCallId"`
			Title      string          `json:"title"`
			RawInput   json.RawMessage `json:"rawInput"`
			Meta       struct {
				Tool struct {
					Name  string `json:"name"`
					Label string `json:"label"`
				} `json:"x.ai/tool"`
			} `json:"_meta"`
		} `json:"toolCall"`
	}
	if json.Unmarshal(params, &body) != nil || body.SessionID == "" {
		return nil, &protocol.RPCError{Code: -32602, Message: "Grok request " + method + " is missing sessionId"}
	}
	if current := g.sess.stateOwner.ThreadID(); current != "" && current != body.SessionID {
		log.Printf("Grok request %s for unrouted session %q on %q — rejecting", method, body.SessionID, current)
		return grokInteractionResult(interact.PendingInteraction{Method: method, Params: params}, decisionDecline, nil)
	}
	kind := kindToolPermission
	visibleParams := sanitizeGrokWire(params)
	var visible map[string]any
	if json.Unmarshal(visibleParams, &visible) == nil {
		switch method {
		case grokExitPlanMethod:
			// Rendered by the toolPermission card: approve exits plan mode and
			// starts implementing, deny sends the agent back to planning with
			// the operator's notes. The plan itself is the thing approved, so
			// it is the card's monospace block.
			visible["tool_name"] = "exit_plan_mode"
			visible["display_name"] = "Exit plan mode"
			visible["description"] = "Approve the agent's plan to start implementing. Deny to send the agent back to planning."
			visible["input"] = map[string]any{"plan": body.PlanContent}
		case grokAskQuestionMethod:
			// The questions are already top-level in the params, which is
			// exactly where the userInput card looks for them. Grok keys
			// answers by question text, the same dialect as claude.
			kind = kindUserInput
		default:
			name := threadvalues.FirstNonEmpty(body.ToolCall.Meta.Tool.Label, body.ToolCall.Meta.Tool.Name, body.ToolCall.Title)
			visible["tool_name"] = body.ToolCall.Meta.Tool.Name
			visible["display_name"] = name
			visible["description"] = body.ToolCall.Title
			if len(body.ToolCall.RawInput) > 0 {
				var input any
				if json.Unmarshal(body.ToolCall.RawInput, &input) == nil {
					visible["input"] = input
				}
			}
		}
		if encoded, err := json.Marshal(visible); err == nil {
			visibleParams = encoded
		}
	}
	pending := interact.PendingInteraction{
		ID: requestID, Method: method, Kind: kind,
		ThreadID: body.SessionID, ItemID: threadvalues.FirstNonEmpty(body.ToolCall.ToolCallID, body.ToolCallID),
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Params:    visibleParams,
	}
	if method == grokPermissionMethod && g.sess.stateOwner.permissionModeForEvent() == "bypassPermissions" {
		// Grok 1.0.3 can still forward a permission request after accepting
		// _meta.yoloMode. Nano's thread-level bypass selection is literal: record
		// both audit events before allowing the request. If the audit store is
		// unavailable, fail closed to the ordinary operator card instead.
		if err := g.sess.delivery.recordPermissionRequest(pending); err == nil {
			if err := g.sess.delivery.recordPermissionResolution(pending, decisionAccept, "nano_bypass", "autoApproved"); err == nil {
				return grokInteractionResult(pending, decisionAccept, nil)
			} else {
				log.Printf("session %s: record Grok bypass resolution %s: %v", g.sess.id, requestID, err)
			}
		} else {
			log.Printf("session %s: record Grok bypass request %s: %v", g.sess.id, requestID, err)
		}
	}
	g.sess.stateOwner.addPendingInteraction(pending)
	return protocol.Deferred, nil
}

func grokInteractionResult(p interact.PendingInteraction, decision string, answers json.RawMessage) (any, *protocol.RPCError) {
	switch p.Method {
	case grokExitPlanMethod:
		return grokPlanDecision(decision, answers)
	case grokAskQuestionMethod:
		return grokQuestionDecision(decision, answers)
	}
	if decision == decisionCancel {
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
	}
	var body struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(p.Params, &body)
	wantAllow := decision == decisionAccept || decision == decisionAcceptForSession
	wantSession := decision == decisionAcceptForSession
	optionID := ""
	for pass := 0; pass < 2 && optionID == ""; pass++ {
		for _, option := range body.Options {
			kind := strings.ToLower(strings.ReplaceAll(option.Kind, "-", "_"))
			if wantAllow {
				isSession := strings.Contains(kind, "always") || strings.Contains(kind, "session")
				if strings.HasPrefix(kind, "allow") && (pass == 1 || isSession == wantSession) {
					optionID = option.OptionID
					break
				}
			} else if strings.HasPrefix(kind, "reject") {
				optionID = option.OptionID
				break
			}
		}
	}
	if optionID == "" {
		if wantAllow {
			optionID = "allow-once"
		} else {
			optionID = "reject-once"
		}
	}
	return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optionID}}, nil
}

// grokPlanDecision speaks ExitPlanModeExtResponse: {"outcome": …, "feedback": …}.
// The leader recognises exactly "approved" and "abandoned"; ANY other response,
// including one whose fields it cannot parse, reads as "revise the plan" with
// the optional feedback echoed to the model. That fail-soft is why nano's old
// wrong answers never errored — and why these shapes were pinned against a
// live 1.0.3 leader (2026-08-13) instead of documentation, which does not exist.
func grokPlanDecision(decision string, answers json.RawMessage) (any, *protocol.RPCError) {
	switch decision {
	case decisionAccept, decisionAcceptForSession:
		return map[string]any{"outcome": "approved"}, nil
	case decisionDecline, decisionCancel:
		// Decline keeps plan mode active and sends the agent back to planning.
		// Cancel (session closing) gets the same answer: it is the only verb
		// that neither starts implementation nor destroys the plan.
		out := map[string]any{"outcome": "revised"}
		if feedback := grokPlanFeedback(answers); feedback != "" {
			out["feedback"] = feedback
		}
		return out, nil
	}
	return nil, &protocol.RPCError{Code: -32602, Message: "unknown decision " + decision}
}

// grokPlanFeedback accepts the operator's revision notes as either a bare
// string or {"feedback": "…"} — the two shapes an external client would
// plausibly send. Empty means "no notes": the leader then tells the model to
// ask what should change.
func grokPlanFeedback(answers json.RawMessage) string {
	if len(answers) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(answers, &text) == nil {
		return strings.TrimSpace(text)
	}
	var body struct {
		Feedback string `json:"feedback"`
	}
	if json.Unmarshal(answers, &body) == nil {
		return strings.TrimSpace(body.Feedback)
	}
	return ""
}

// grokQuestionDecision speaks AskUserQuestionExtResponse:
// {"outcome": "accepted", "answers": {question text: [labels…]}}. Like claude,
// grok keys answers by the question's own text — there are no question ids on
// the wire. Declining or cancelling answers nothing, which the leader reports
// to the model as the user not answering; unlike exit_plan_mode this response
// is parsed strictly, so both fields are always present.
func grokQuestionDecision(decision string, answers json.RawMessage) (any, *protocol.RPCError) {
	chosen := map[string]any{}
	if decision == decisionAccept || decision == decisionAcceptForSession {
		var loose map[string][]string
		if len(answers) == 0 || json.Unmarshal(answers, &loose) != nil || len(loose) == 0 {
			return nil, &protocol.RPCError{Code: -32602, Message: "answers must be an object of label lists keyed by question"}
		}
		for question, labels := range loose {
			if len(labels) > 0 {
				chosen[question] = labels
			}
		}
	}
	return map[string]any{"outcome": "accepted", "answers": chosen}, nil
}

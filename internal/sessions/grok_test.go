package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/wire"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func TestGrokProcessEnvForcesAutoUpdaterOff(t *testing.T) {
	t.Setenv("GROK_DISABLE_AUTOUPDATER", "0")
	count := 0
	for _, entry := range GrokProcessEnv() {
		if strings.HasPrefix(entry, "GROK_DISABLE_AUTOUPDATER=") {
			count++
			if entry != grokDisableUpdater {
				t.Fatalf("auto-updater env = %q, want %q", entry, grokDisableUpdater)
			}
		}
	}
	if count != 1 {
		t.Fatalf("auto-updater env appeared %d times, want once", count)
	}
}

func TestGrokProbeCwdFallsBackWhenConfiguredDirectoryDisappears(t *testing.T) {
	existing := t.TempDir()
	if got := GrokProbeCwd(existing); got != existing {
		t.Fatalf("existing probe cwd = %q, want %q", got, existing)
	}

	missing := filepath.Join(t.TempDir(), "removed-release-checkout")
	got := GrokProbeCwd(missing)
	if got == missing {
		t.Fatalf("missing probe cwd was retained: %q", got)
	}
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Fatalf("fallback probe cwd %q is not an existing directory: info=%v err=%v", got, info, err)
	}
}

func TestGrokSessionRequestCarriesEffortAndPermissionModeOnlyOnNewSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		meta map[string]any
	}{
		{name: "inherit"},
		{name: "auto", mode: "auto", meta: map[string]any{"autoMode": true}},
		{name: "bypass", mode: "bypassPermissions", meta: map[string]any{"yoloMode": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method, params := GrokSessionRequest(ProviderRequest{Mode: "start", Cwd: "/work", Effort: "xhigh", PermissionMode: tc.mode})
			meta, hasMeta := params["_meta"]
			wantMeta := map[string]any{"reasoningEffort": "xhigh"}
			for key, value := range tc.meta {
				wantMeta[key] = value
			}
			metaMatches := hasMeta && reflect.DeepEqual(meta, wantMeta)
			if method != "session/new" || params["cwd"] != "/work" || !metaMatches {
				t.Fatalf("request = %q %#v, want meta %#v", method, params, wantMeta)
			}
		})
	}
	method, params := GrokSessionRequest(ProviderRequest{Mode: "resume", Cwd: "/work", ThreadID: "native", PermissionMode: "bypassPermissions"})
	if method != "session/load" || params["sessionId"] != "native" || params["_meta"] != nil {
		t.Fatalf("resume request = %q %#v", method, params)
	}
}

func TestGrokSelectedEffortRequiresOneSelectedSessionMode(t *testing.T) {
	selected := json.RawMessage(`{"_meta":{"x.ai/sessionConfig":{"options":[` +
		`{"category":"mode","id":"xhigh","selected":true},` +
		`{"category":"mode","id":"high","selected":false}]}}}`)
	if got, err := grokSelectedEffort(selected); err != nil || got != "xhigh" {
		t.Fatalf("selected effort = %q, %v; want xhigh", got, err)
	}

	ambiguous := json.RawMessage(`{"_meta":{"x.ai/sessionConfig":{"options":[` +
		`{"category":"mode","id":"xhigh","selected":true},` +
		`{"category":"mode","id":"high","selected":true}]}}}`)
	if _, err := grokSelectedEffort(ambiguous); err == nil {
		t.Fatal("multiple selected effort modes were accepted")
	}
}

func TestGrokRequestedEffortMustMatchProviderSelection(t *testing.T) {
	selectedHigh := json.RawMessage(`{"_meta":{"x.ai/sessionConfig":{"options":[` +
		`{"category":"mode","id":"xhigh","selected":false},` +
		`{"category":"mode","id":"high","selected":true}]}}}`)
	if _, err := grokVerifyRequestedEffort(ProviderRequest{Mode: "start", Effort: "xhigh"}, selectedHigh); err == nil || !strings.Contains(err.Error(), "requested xhigh") {
		t.Fatalf("mismatch error = %v", err)
	}
	if got, err := grokVerifyRequestedEffort(ProviderRequest{Mode: "resume", Effort: "xhigh"}, selectedHigh); err != nil || got != "high" {
		t.Fatalf("resume observation = %q, %v; want high without creation rejection", got, err)
	}
}

func TestNormalizeGrokModelCatalog(t *testing.T) {
	efforts := `[{"id":"xhigh","label":"Extra High"},{"id":"high","label":"High","default":true},` +
		`{"id":"medium","label":"Medium"},{"id":"low","label":"Low"}]`
	raw := json.RawMessage(`{"_meta":{"modelState":{"currentModelId":"grok-4.7","availableModels":[` +
		`{"modelId":"grok-4.7","name":"Grok 4.7","_meta":{"reasoningEffort":"high","reasoningEfforts":` + efforts + `}},` +
		`{"modelId":"grok-4.7-build-fast","name":"Grok 4.7 Fast","_meta":{"reasoningEffort":"high","reasoningEfforts":` + efforts + `}},` +
		`{"modelId":"grok-4.6","name":"Grok 4.6","_meta":{"reasoningEffort":"high","reasoningEfforts":` + efforts + `}},` +
		`{"modelId":"grok-4.5","name":"Grok 4.5","_meta":{"reasoningEffort":"high","reasoningEfforts":[` +
		`{"id":"high","label":"High","default":true},{"id":"medium","label":"Medium"},{"id":"low","label":"Low"}]}}]}}}`)
	var body struct {
		Data []struct {
			Model                     string `json:"model"`
			DisplayName               string `json:"displayName"`
			IsDefault                 bool   `json:"isDefault"`
			DefaultReasoningEffort    string `json:"defaultReasoningEffort"`
			SupportedReasoningEfforts []struct {
				ReasoningEffort string `json:"reasoningEffort"`
			} `json:"supportedReasoningEfforts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(normalizeGrokModelCatalog(raw), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 4 || body.Data[0].Model != "grok-4.7" || body.Data[0].DisplayName != "Grok 4.7" || !body.Data[0].IsDefault ||
		body.Data[0].DefaultReasoningEffort != "high" || len(body.Data[0].SupportedReasoningEfforts) != 4 ||
		body.Data[0].SupportedReasoningEfforts[0].ReasoningEffort != "xhigh" ||
		body.Data[1].Model != "grok-4.7-build-fast" || body.Data[1].IsDefault ||
		body.Data[2].Model != "grok-4.6" || body.Data[2].IsDefault ||
		body.Data[3].Model != "grok-4.5" || body.Data[3].IsDefault ||
		len(body.Data[3].SupportedReasoningEfforts) != 3 ||
		body.Data[3].SupportedReasoningEfforts[0].ReasoningEffort != "high" {
		t.Fatalf("catalog = %+v", body.Data)
	}
}

func TestGrokLoadReconcilesReplayAgainstRunningPrompt(t *testing.T) {
	replayed := []byte(`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"unfinished"}},"_meta":{"promptId":"old-prompt","isReplay":true}}}`)
	replayedTerminal := []byte(`{"method":"_x.ai/session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"older-prompt","stop_reason":"end_turn"},"_meta":{"eventId":"old-terminal","isReplay":true}}}`)

	stale := sessionWithOwners(&Session{provider: "grok",
		stateOwner: State{
			state:          map[string]json.RawMessage{},
			lifecycleState: testLifecycleSnapshot("ready", false)}})
	(&grokSession{sess: stale}).recordWire(protocol.Recv, replayed)
	if stale.stateOwner.workActiveLocked() {
		t.Fatal("historical replay became present-tense activity")
	}
	stale.runtime.reconcileGrokLoadedTurn(json.RawMessage(`{"_meta":{}}`))
	(&grokSession{sess: stale}).recordWire(protocol.Recv, replayed)
	if stale.stateOwner.workActiveLocked() || stale.stateOwner.turnID != "" || stale.stateOwner.lastTurnEndedAt != "" {
		t.Fatalf("late stale replay survived load reconciliation: active=%v id=%q ended=%q", stale.stateOwner.workActiveLocked(), stale.stateOwner.turnID, stale.stateOwner.lastTurnEndedAt)
	}

	running := sessionWithOwners(&Session{provider: "grok",
		stateOwner: State{
			state:          map[string]json.RawMessage{},
			lifecycleState: testLifecycleSnapshot("ready", false)}})
	(&grokSession{sess: running}).recordWire(protocol.Recv, replayed)
	running.runtime.reconcileGrokLoadedTurn(json.RawMessage(`{"_meta":{"x.ai/runningPromptId":"old-prompt"}}`))
	(&grokSession{sess: running}).recordWire(protocol.Recv, replayedTerminal)
	if !running.stateOwner.workActiveLocked() || running.stateOwner.turnID != "old-prompt" {
		t.Fatalf("late replay disturbed live resumed prompt = active %v id %q", running.stateOwner.workActiveLocked(), running.stateOwner.turnID)
	}
}

func TestGrokLoadDoesNotRestoreSyntheticContinuation(t *testing.T) {
	sess := sessionWithOwners(&Session{provider: "grok",
		stateOwner: State{
			turnID:         "stale-operator",
			lifecycleState: testLifecycleSnapshot("ready", true)}})
	sess.runtime.reconcileGrokLoadedTurn(json.RawMessage(`{"_meta":{"x.ai/runningPromptId":"subagent-completed-child-1"}}`))
	if sess.stateOwner.workActiveLocked() || sess.stateOwner.turnID != "" {
		t.Fatalf("synthetic continuation restored operator runtime: active=%v turn=%q", sess.stateOwner.workActiveLocked(), sess.stateOwner.turnID)
	}
}

func TestGrokReplayFlagIsNotLiveEvidence(t *testing.T) {
	replayed := []byte(`{"method":"session/update","params":{"_meta":{"isReplay":true}}}`)
	live := []byte(`{"method":"session/update","params":{"_meta":{"isReplay":false}}}`)
	if !grokWireIsReplay(replayed) || grokWireIsReplay(live) {
		t.Fatal("Grok replay marker was not classified")
	}
}

func TestGrokCurrentModelUnderstandsSessionResult(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{
		"initialize":     json.RawMessage(`{"_meta":{"modelState":{"currentModelId":"grok-init"}}}`),
		"session models": json.RawMessage(`{"models":{"currentModelId":"grok-session"}}`),
		"session detail": json.RawMessage(`{"_meta":{"x.ai/sessionDetail":{"currentModelId":"grok-detail"}}}`),
	} {
		if got := grokCurrentModel(raw); got == "" {
			t.Fatalf("%s model was not projected", name)
		}
	}
}

func TestGrokPromptAcceptanceRequiresMatchingQueueText(t *testing.T) {
	matching := []byte(`{"jsonrpc":"2.0","method":"_x.ai/queue/changed","params":{"sessionId":"s1","entries":[{"id":"new-prompt","kind":"prompt","text":"new request"}]}}`)
	if got := grokAcceptancePromptID(matching, "new request", "s1"); got != "new-prompt" {
		t.Fatalf("matching acceptance = %q", got)
	}
	if got := grokAcceptancePromptID(matching, "different request", "s1"); got != "" {
		t.Fatalf("mismatched historical queue entry accepted as %q", got)
	}
	replayed := []byte(`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"old"}},"_meta":{"promptId":"old-prompt"}}}`)
	if got := grokAcceptancePromptID(replayed, "new request", "s1"); got != "" {
		t.Fatalf("replayed update accepted as %q", got)
	}
	child := []byte(`{"jsonrpc":"2.0","method":"_x.ai/queue/changed","params":{"sessionId":"child","entries":[{"id":"child-prompt","kind":"prompt","text":"new request"}]}}`)
	if got := grokAcceptancePromptID(child, "new request", "s1"); got != "" {
		t.Fatalf("child queue accepted parent request as %q", got)
	}
	synthetic := []byte(`{"jsonrpc":"2.0","method":"_x.ai/queue/changed","params":{"sessionId":"s1","entries":[{"id":"subagent-completed-child-1","kind":"prompt","text":"new request"}]}}`)
	if got := grokAcceptancePromptID(synthetic, "new request", "s1"); got != "" {
		t.Fatalf("synthetic subagent prompt accepted parent request as %q", got)
	}
}

func TestGrokWireIngressRejectsMultiplexedChildSession(t *testing.T) {
	sess := sessionWithOwners(&Session{provider: "grok",
		stateOwner: State{
			threadID:       "parent",
			lifecycleState: testLifecycleSnapshot("ready", false)}})
	grok := &grokSession{sess: sess}
	child := []byte(`{"method":"session/update","params":{"sessionId":"child","update":{"sessionUpdate":"agent_message_chunk"}}}`)
	if grok.wireBelongsToAttachedSession(child) {
		t.Fatal("child session update reached parent semantics")
	}
	parent := []byte(`{"method":"session/update","params":{"sessionId":"parent","update":{"sessionUpdate":"agent_message_chunk"}}}`)
	if !grok.wireBelongsToAttachedSession(parent) {
		t.Fatal("parent session update was rejected")
	}
	for _, line := range [][]byte{
		[]byte(`{"method":"_x.ai/queue/changed","params":{"sessionId":"child"}}`),
		[]byte(`{"method":"_x.ai/session_notification","params":{"sessionId":"child","update":{"sessionUpdate":"turn_completed"}}}`),
		[]byte(`{"method":"_x.ai/session/prompt_complete","params":{"sessionId":"child","promptId":"child-prompt"}}`),
	} {
		if grok.wireMayAffectParentSemantics(line) {
			t.Fatalf("child event reached parent semantics: %s", line)
		}
	}
	missingSession := []byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk"}}}`)
	if grok.wireMayAffectParentSemantics(missingSession) {
		t.Fatal("sessionless update reached parent semantics")
	}
	promptComplete := []byte(`{"method":"_x.ai/session/prompt_complete","params":{"promptId":"parent-prompt"}}`)
	if grok.wireMayAffectParentSemantics(promptComplete) {
		t.Fatal("unbound sessionless prompt completion reached parent semantics")
	}
	grok.active = newGrokPrompt("parent text", "")
	grok.active.accept("parent-prompt")
	if !grok.wireMayAffectParentSemantics(promptComplete) {
		t.Fatal("bound sessionless prompt completion was rejected")
	}
}

func TestGrokInteractionSelectsNativeOptions(t *testing.T) {
	pending := interact.PendingInteraction{Params: json.RawMessage(`{"options":[{"optionId":"always","kind":"allow_always"},{"optionId":"yes","kind":"allow_once"},{"optionId":"no","kind":"reject_once"}]}`)}
	accepted, rpcErr := grokInteractionResult(pending, decisionAccept, nil)
	if rpcErr != nil || grokSelectedOption(accepted) != "yes" {
		t.Fatalf("accept = %#v, %v", accepted, rpcErr)
	}
	declined, rpcErr := grokInteractionResult(pending, decisionDecline, nil)
	if rpcErr != nil || grokSelectedOption(declined) != "no" {
		t.Fatalf("decline = %#v, %v", declined, rpcErr)
	}
	forSession, rpcErr := grokInteractionResult(pending, decisionAcceptForSession, nil)
	if rpcErr != nil || grokSelectedOption(forSession) != "always" {
		t.Fatalf("accept for session = %#v, %v", forSession, rpcErr)
	}
	cancelled, rpcErr := grokInteractionResult(pending, decisionCancel, nil)
	raw, _ := json.Marshal(cancelled)
	if rpcErr != nil || !strings.Contains(string(raw), `"outcome":"cancelled"`) {
		t.Fatalf("cancel = %#v, %v", cancelled, rpcErr)
	}
}

func TestGrokPermissionRequestUsesExistingInteractionShape(t *testing.T) {
	sess := sessionWithOwners(&Session{id: "nano",
		provider: "grok",
		stateOwner: State{
			threadID:       "s1",
			interactions:   map[string]interact.PendingInteraction{},
			lifecycleState: testLifecycleSnapshot("ready", false)},
		wire: Wire{
			subs: map[chan wire.Event]struct{}{}}})
	grok := &grokSession{sess: sess}
	params := json.RawMessage(`{"sessionId":"s1","hostname":"private-host","toolCall":{"toolCallId":"call-1","title":"Run tests",` +
		`"rawInput":{"command":"go test ./..."},"_meta":{"x.ai/tool":{"name":"run_shell","label":"Shell"}}},` +
		`"options":[{"optionId":"yes","kind":"allow_once"},{"optionId":"no","kind":"reject_once"}]}`)
	result, rpcErr := grok.handleServerRequest(`"request-1"`, grokPermissionMethod, params)
	if rpcErr != nil || result != protocol.Deferred {
		t.Fatalf("permission result = %#v, %v", result, rpcErr)
	}
	pending := sess.delivery.PendingInteractionList()
	if len(pending) != 1 || pending[0].ItemID != "call-1" {
		t.Fatalf("pending = %#v", pending)
	}
	var visible map[string]any
	if err := json.Unmarshal(pending[0].Params, &visible); err != nil {
		t.Fatal(err)
	}
	if visible["display_name"] != "Shell" || visible["description"] != "Run tests" {
		t.Fatalf("visible params = %#v", visible)
	}
	if visible["hostname"] == "private-host" {
		t.Fatalf("permission params were not sanitized: %#v", visible)
	}
}

// The exit_plan_mode reverse request must park as an approvable interaction —
// refusing it by name is what the leader reads as a client disconnect, and it
// cancels the whole turn (the 2026-08-13 hosted-session failure).
func TestGrokExitPlanRequestParksPlanApproval(t *testing.T) {
	sess := sessionWithOwners(&Session{id: "nano",
		provider: "grok",
		stateOwner: State{
			threadID:       "s1",
			interactions:   map[string]interact.PendingInteraction{},
			lifecycleState: testLifecycleSnapshot("ready", false)},
		wire: Wire{
			subs: map[chan wire.Event]struct{}{}}})
	grok := &grokSession{sess: sess}
	params := json.RawMessage(`{"sessionId":"s1","toolCallId":"call-9","planContent":"# The plan\n"}`)
	result, rpcErr := grok.handleServerRequest(`"request-9"`, grokExitPlanMethod, params)
	if rpcErr != nil || result != protocol.Deferred {
		t.Fatalf("exit plan result = %#v, %v", result, rpcErr)
	}
	pending := sess.delivery.PendingInteractionList()
	if len(pending) != 1 || pending[0].ItemID != "call-9" || pending[0].Method != grokExitPlanMethod {
		t.Fatalf("pending = %#v", pending)
	}
	if pending[0].Kind != kindToolPermission {
		t.Fatalf("kind = %q", pending[0].Kind)
	}
	var visible struct {
		Input struct {
			Plan string `json:"plan"`
		} `json:"input"`
	}
	if err := json.Unmarshal(pending[0].Params, &visible); err != nil || visible.Input.Plan != "# The plan\n" {
		t.Fatalf("visible params = %s (%v)", pending[0].Params, err)
	}
}

func TestGrokAskQuestionRequestParksAsUserInput(t *testing.T) {
	sess := sessionWithOwners(&Session{id: "nano",
		provider: "grok",
		stateOwner: State{
			threadID:       "s1",
			interactions:   map[string]interact.PendingInteraction{},
			lifecycleState: testLifecycleSnapshot("ready", false)},
		wire: Wire{
			subs: map[chan wire.Event]struct{}{}}})
	grok := &grokSession{sess: sess}
	params := json.RawMessage(`{"sessionId":"s1","toolCallId":"call-3",` +
		`"questions":[{"question":"Pick A or B","options":[{"label":"A"},{"label":"B"}],"multiSelect":null}]}`)
	result, rpcErr := grok.handleServerRequest(`"request-3"`, grokAskQuestionMethod, params)
	if rpcErr != nil || result != protocol.Deferred {
		t.Fatalf("ask result = %#v, %v", result, rpcErr)
	}
	pending := sess.delivery.PendingInteractionList()
	if len(pending) != 1 || pending[0].Kind != kindUserInput || pending[0].ItemID != "call-3" {
		t.Fatalf("pending = %#v", pending)
	}
	// The userInput card reads params.questions directly; they must survive.
	if !strings.Contains(string(pending[0].Params), `"Pick A or B"`) {
		t.Fatalf("questions lost from params: %s", pending[0].Params)
	}
}

// Response shapes pinned against a live grok 1.0.3 leader on 2026-08-13:
// "approved"/"abandoned" are the only outcomes exit_plan_mode recognises, and
// anything else — including a malformed body — silently reads as "revise".
func TestGrokPlanDecisionSpeaksVerifiedOutcomes(t *testing.T) {
	pending := interact.PendingInteraction{Method: grokExitPlanMethod}
	accepted, rpcErr := grokInteractionResult(pending, decisionAccept, nil)
	if rpcErr != nil || !reflect.DeepEqual(accepted, map[string]any{"outcome": "approved"}) {
		t.Fatalf("accept = %#v, %v", accepted, rpcErr)
	}
	declined, rpcErr := grokInteractionResult(pending, decisionDecline, json.RawMessage(`{"feedback":"tighten scope"}`))
	if rpcErr != nil || !reflect.DeepEqual(declined, map[string]any{"outcome": "revised", "feedback": "tighten scope"}) {
		t.Fatalf("decline = %#v, %v", declined, rpcErr)
	}
	declinedBare, rpcErr := grokInteractionResult(pending, decisionDecline, json.RawMessage(`"use tabs"`))
	if rpcErr != nil || !reflect.DeepEqual(declinedBare, map[string]any{"outcome": "revised", "feedback": "use tabs"}) {
		t.Fatalf("decline bare = %#v, %v", declinedBare, rpcErr)
	}
	cancelled, rpcErr := grokInteractionResult(pending, decisionCancel, nil)
	if rpcErr != nil || !reflect.DeepEqual(cancelled, map[string]any{"outcome": "revised"}) {
		t.Fatalf("cancel = %#v, %v", cancelled, rpcErr)
	}
}

func TestGrokQuestionDecisionKeysAnswersByQuestionText(t *testing.T) {
	pending := interact.PendingInteraction{Method: grokAskQuestionMethod}
	accepted, rpcErr := grokInteractionResult(pending, decisionAccept, json.RawMessage(`{"Pick A or B":["A"]}`))
	if rpcErr != nil {
		t.Fatalf("accept err = %v", rpcErr)
	}
	want := map[string]any{"outcome": "accepted", "answers": map[string]any{"Pick A or B": []any{"A"}}}
	raw, _ := json.Marshal(accepted)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("accept = %#v", got)
	}
	if _, rpcErr = grokInteractionResult(pending, decisionAccept, nil); rpcErr == nil {
		t.Fatal("accept without answers must be rejected so the operator can retry")
	}
	skipped, rpcErr := grokInteractionResult(pending, decisionDecline, nil)
	if rpcErr != nil || !reflect.DeepEqual(skipped, map[string]any{"outcome": "accepted", "answers": map[string]any{}}) {
		t.Fatalf("decline = %#v, %v", skipped, rpcErr)
	}
}

func grokSelectedOption(value any) string {
	raw, _ := json.Marshal(value)
	var body struct {
		Outcome struct {
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	_ = json.Unmarshal(raw, &body)
	return body.Outcome.OptionID
}

func TestSanitizeGrokWireBoundsKnownSensitiveFields(t *testing.T) {
	line := []byte(`{"method":"session/update","params":{"hostname":"mac","update":{"rawOutput":{"output":[1,2,3]},"signature":"opaque","content":{"type":"text","text":"keep"}}}}`)
	got := string(sanitizeGrokWire(line))
	for _, secret := range []string{"mac", "opaque", "[1,2,3]"} {
		if strings.Contains(got, secret) {
			t.Fatalf("sanitized line retained %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "keep") || !strings.Contains(got, "redacted") {
		t.Fatalf("sanitized line lost useful shape: %s", got)
	}
}

func TestGrokUnroutedPermissionFailsClosedWithoutParking(t *testing.T) {
	sess := sessionWithOwners(&Session{id: "nano",
		provider: "grok",
		stateOwner: State{
			threadID:       "parent",
			permissionMode: "bypassPermissions",
			interactions:   map[string]interact.PendingInteraction{},
			lifecycleState: testLifecycleSnapshot("ready", false)},
		wire: Wire{
			subs: map[chan wire.Event]struct{}{}}})
	grok := &grokSession{sess: sess}
	params := json.RawMessage(`{"sessionId":"child","toolCall":{"toolCallId":"call-1"},"options":[{"optionId":"yes","kind":"allow_once"},{"optionId":"no","kind":"reject_once"}]}`)
	result, rpcErr := grok.handleServerRequest(`"request-child"`, grokPermissionMethod, params)
	if rpcErr != nil || grokSelectedOption(result) != "no" {
		t.Fatalf("unrouted permission did not fail closed: %#v, %v", result, rpcErr)
	}
	if pending := sess.delivery.PendingInteractionList(); len(pending) != 0 {
		t.Fatalf("unrouted permission parked on parent: %#v", pending)
	}
}

func TestGrokCommandUsesDurableLeaderAndFixedSandbox(t *testing.T) {
	req := ProviderRequest{Mode: "start", GrokBin: "/opt/grok", GrokLeaderSocket: "/tmp/grok.sock",
		Model: "grok-4.7", Effort: "high", PermissionMode: "default"}
	want := []string{
		"/opt/grok", "--sandbox", "off", "agent",
		"--model", "grok-4.7", "--reasoning-effort", "high", "--leader",
		"--leader-socket", "/tmp/grok.sock", "stdio",
	}
	if got := GrokCommand(req); !reflect.DeepEqual(got, want) {
		t.Fatalf("grokCommand = %#v, want %#v", got, want)
	}

	req.Mode = "resume"
	got := strings.Join(GrokCommand(req), " ")
	if strings.Contains(got, "--model") || strings.Contains(got, "--reasoning-effort") {
		t.Fatalf("resume command overrides native posture: %s", got)
	}
}

func TestGrokPromptIdentityAndTerminalEvidence(t *testing.T) {
	queue := []byte(`{"jsonrpc":"2.0","method":"_x.ai/queue/changed","params":{"sessionId":"s1","entries":[{"id":"p1","kind":"prompt"}]}}`)
	if got := grokPromptIDFromWire(queue); got != "p1" {
		t.Fatalf("queue prompt id = %q", got)
	}
	chunk := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}},"_meta":{"promptId":"p1","eventId":"s1-1","agentTimestampMs":42}}}`)
	if got := grokPromptIDFromWire(chunk); got != "p1" {
		t.Fatalf("chunk prompt id = %q", got)
	}
	terminal := []byte(`{"jsonrpc":"2.0","method":"_x.ai/session_notification","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":"end_turn","usage":{"inputTokens":3}},"_meta":{"eventId":"s1-2"}}}`)
	promptID, status, usage, ok := grokTerminalFromWire(terminal)
	if !ok || promptID != "p1" || status != agentTurnCompleted || !strings.Contains(string(usage), "inputTokens") {
		t.Fatalf("terminal = (%q, %q, %s, %v)", promptID, status, usage, ok)
	}
	active, turnID, _, changed := turnActiveTransition("grok", protocol.Recv, queue)
	if !changed || !active || turnID != "p1" {
		t.Fatalf("queue transition = (%v, %q, %v)", active, turnID, changed)
	}
	active, turnID, _, changed = turnActiveTransition("grok", protocol.Recv, terminal)
	if !changed || active || turnID != "p1" {
		t.Fatalf("terminal transition = (%v, %q, %v)", active, turnID, changed)
	}
}

func TestGrokPhase0WireFixtureKeepsEveryRawFrame(t *testing.T) {
	raw, err := os.ReadFile("../transcriptparse/grok/testdata/phase0-parent-wire.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	sess := sessionWithOwners(&Session{provider: "grok",
		stateOwner: State{
			threadID:       "parent-1",
			state:          map[string]json.RawMessage{},
			lifecycleState: testLifecycleSnapshot("ready", false)},
		view: View{
			projection: newDeliveryTestProjection("grok")},
		wire: Wire{
			subs: map[chan wire.Event]struct{}{}}})
	sess.view.projection.setNativeThreadID("parent-1")
	g := &grokSession{sess: sess, seenPromptIDs: map[string]struct{}{}}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, line := range lines {
		var fixture struct {
			Msg json.RawMessage `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &fixture); err != nil {
			t.Fatal(err)
		}
		g.recordWire(protocol.Recv, fixture.Msg)
	}
	if len(sess.wire.events) != len(lines) {
		t.Fatalf("raw events = %d, want %d", len(sess.wire.events), len(lines))
	}
}

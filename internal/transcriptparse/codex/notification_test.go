package codex

import (
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexMetadataNotificationsRetainNativeEvidenceWithoutTurns(t *testing.T) {
	cases := []string{
		"wire-account-login-completed", "wire-account-ratelimits-updated", "wire-account-updated",
		"wire-app-list-updated", "wire-externalagentconfig-import-completed", "wire-externalagentconfig-import-progress",
		"wire-fs-changed", "wire-fuzzyfilesearch-sessioncompleted", "wire-fuzzyfilesearch-sessionupdated",
		"wire-hook-completed", "wire-hook-started", "wire-mcpserver-oauthlogin-completed", "wire-mcpserver-startupstatus-updated",
		"wire-model-rerouted", "wire-model-safetybuffering-updated", "wire-model-verification", "wire-remotecontrol-status-changed",
		"wire-skills-changed", "wire-thread-archived", "wire-thread-closed", "wire-thread-deleted", "wire-thread-goal-cleared",
		"wire-thread-goal-updated", "wire-thread-name-updated", "wire-thread-settings-updated", "wire-thread-started",
		"wire-thread-status-changed", "wire-thread-unarchived", "wire-turn-moderationmetadata", "wire-windowssandbox-setupcompleted",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			raw := fixture(t, name)
			var capture map[string]json.RawMessage
			if err := json.Unmarshal(raw, &capture); err != nil {
				t.Fatal(err)
			}
			params := rawObject(rawObject(capture, "msg"), "params")
			input := inputFor(raw)
			callerTurn := "unrelated-caller-turn"
			input.Scope.NativeTurnID = &callerTurn
			state, batch, err := parseWireView(parsedView{}, input)
			if err != nil || len(batch.Diagnostics) != 0 || len(batch.Observations) != 1 {
				t.Fatalf("metadata: %v %+v", err, batch.Diagnostics)
			}
			if len(state.Items) != 0 || len(state.Turns) != 0 || len(state.Continuation.NativeThreads) != 0 {
				t.Error("metadata invented conversation or a remembered default")
			}
			obs := batch.Observations[0]
			var detail map[string]json.RawMessage
			if json.Unmarshal(obs.Detail, &detail) != nil || rpcFingerprint(rawObject(detail, "params")) != rpcFingerprint(params) {
				t.Error("native metadata fields were discarded")
			}
			if obs.Scope.NativeTurnID != nil && *obs.Scope.NativeTurnID == callerTurn {
				t.Error("metadata inherited an unrelated turn")
			}
			if rawString(params, "threadId") == "" && name != "wire-thread-started" && obs.Scope.Kind != model.ScopeConnection {
				t.Error("connection metadata gained thread authority")
			}
			if name == "wire-thread-goal-updated" || name == "wire-account-ratelimits-updated" {
				if len(batch.Changes) != 0 {
					t.Error("goal/rate counters became token usage")
				}
			}
		})
	}
}

func TestCodexWireNoticesAndCompactionDoNotCompleteTurns(t *testing.T) {
	for _, name := range []string{"wire-error", "wire-warning", "wire-configwarning", "wire-deprecationnotice", "wire-guardianwarning", "wire-windows-worldwritablewarning", "wire-thread-compacted"} {
		t.Run(name, func(t *testing.T) {
			state, batch, err := parseWireView(parsedView{}, inputFor(fixture(t, name)))
			items := orderedItems(state)
			if err != nil || len(batch.Diagnostics) != 0 || len(items) != 1 {
				t.Fatalf("notice: %v %+v", err, batch.Diagnostics)
			}
			if !items[0].Complete || items[0].Outcome != model.OutcomeUnknown || items[0].IsFinalAnswer {
				t.Error("notice body completed an action")
			}
			if name == "wire-thread-compacted" && items[0].Type != model.ItemCompaction {
				t.Error("compaction lost its type")
			}
			if name == "wire-error" && items[0].Text != "Example text." {
				t.Error("native error text is not inspectable")
			}
			for _, turn := range state.Turns {
				if turn.Outcome != model.OutcomeUnknown || len(turn.Completion) != 0 {
					t.Error("notice completed its parent")
				}
			}
		})
	}
}

func TestCodexUnknownNotificationRetainsBoundedEnvelope(t *testing.T) {
	state, batch, err := parseWireView(parsedView{}, inputFor([]byte(`{"method":"thread/futureNative","nativeOrdinal":99,"params":{"threadId":"thread","future":{"value":"retained"}}}`)))
	if err != nil || len(state.Items) != 0 || len(state.Turns) != 0 || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "unknown_method" {
		t.Fatalf("unknown method: %v %+v", err, batch.Diagnostics)
	}
	var detail map[string]json.RawMessage
	if len(batch.Observations) != 1 || json.Unmarshal(batch.Observations[0].Detail, &detail) != nil || string(detail["nativeOrdinal"]) != "99" || rawString(rawObject(rawObject(detail, "params"), "future"), "value") != "retained" {
		t.Error("unknown envelope lost native detail")
	}
}

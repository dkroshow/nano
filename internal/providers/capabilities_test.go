package providers_test

import (
	"testing"

	"github.com/dkroshow/nano/internal/providers"
	"github.com/dkroshow/nano/internal/providers/claude"
	"github.com/dkroshow/nano/internal/providers/codex"
	"github.com/dkroshow/nano/internal/providers/grok"
)

func setting(value string) *string { return &value }

func TestPermissionCapabilityUsesDeclaredLaunchField(t *testing.T) {
	posture := providers.Posture{PermissionMode: "permission-mode", Approval: "approval-policy"}
	if codex.Declaration().Turns.PermissionValue(posture) != posture.Approval {
		t.Fatal("approval policy was replaced by the unrelated permission-mode field")
	}
	for _, declaration := range []providers.Declaration{claude.Declaration(), grok.Declaration()} {
		if declaration.Turns.PermissionValue(posture) != posture.PermissionMode {
			t.Fatal("permission-mode provider inherited an approval policy")
		}
	}
}

func TestSettingCapabilitiesRespectWhenChangesTakeEffect(t *testing.T) {
	for _, test := range []struct {
		name        string
		declaration providers.Declaration
		moment      providers.SettingMoment
		settings    providers.Settings
		allowed     bool
	}{
		{"creation supports model and effort", grok.Declaration(), providers.AtCreation, providers.Settings{Model: setting("model"), Effort: setting("high")}, true},
		{"next turn overrides", codex.Declaration(), providers.AtNextTurn, providers.Settings{Model: setting(""), Effort: setting("high"), ServiceTier: setting("priority")}, true},
		{"live model control", claude.Declaration(), providers.InSession, providers.Settings{Model: setting("")}, true},
		{"effort fixed at creation", claude.Declaration(), providers.InSession, providers.Settings{Effort: setting("high")}, false},
		{"native session model fixed", grok.Declaration(), providers.AtNextTurn, providers.Settings{Model: setting("other")}, false},
		{"service tier unsupported", claude.Declaration(), providers.AtCreation, providers.Settings{ServiceTier: setting("priority")}, false},
		{"wrong change moment", codex.Declaration(), providers.InSession, providers.Settings{Model: setting("other")}, false},
		{"unknown moment", codex.Declaration(), "unknown", providers.Settings{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.declaration.Turns.CheckSettings(test.moment, test.settings)
			if (err == nil) != test.allowed {
				t.Fatalf("allowed=%t: %v", test.allowed, err)
			}
		})
	}
}

func TestInteractionCapabilitiesRejectUnsupportedKinds(t *testing.T) {
	for _, declaration := range []providers.Declaration{codex.Declaration(), claude.Declaration(), grok.Declaration()} {
		for _, kind := range declaration.Turns.Interactions {
			if err := declaration.Turns.CheckInteraction(kind); err != nil {
				t.Fatal(err)
			}
		}
		if err := declaration.Turns.CheckInteraction("unknown"); err == nil {
			t.Fatal("unknown interaction accepted")
		}
	}
	if claude.Declaration().Turns.CheckInteraction(providers.CommandApproval) == nil {
		t.Fatal("tool permission silently gained a session-wide command approval")
	}
}

func TestCapabilityDeclarationsPreserveEvidenceGaps(t *testing.T) {
	cx, cl, gr := codex.Declaration(), claude.Declaration(), grok.Declaration()
	if !cx.Lifecycle.ResumeWithoutTranscript || !cx.Lifecycle.NativeRecovery || !gr.Lifecycle.ResumeWorkSnapshot {
		t.Fatal("native recovery capability was lost")
	}
	if cl.Lifecycle.NativeTurnID || cl.Lifecycle.NativeTurnStart {
		t.Fatal("Claude gained invented native turn or weaker confirmation authority")
	}
	if !cx.Turns.InterruptedWithoutUser || cl.Turns.InterruptedWithoutUser || gr.Turns.InterruptedWithoutUser {
		t.Fatal("interrupted-without-user settlement support changed")
	}
	if cx.Turns.Inventory != providers.InventoryQuery || cl.Turns.Inventory != providers.InventoryInitialization || gr.Turns.Inventory != providers.InventoryUnavailable {
		t.Fatal("inventory query, initialization and absence were collapsed")
	}
	if cl.Turns.Instructions != providers.InstructionsExpected || cx.Turns.Instructions != providers.InstructionsNative || gr.Turns.Instructions != providers.InstructionsUnavailable {
		t.Fatal("expected instruction files became proof of loaded input")
	}
	if gr.Turns.MetadataAppend || !gr.Turns.LineageSidecar || gr.Turns.Metadata != providers.SessionSnapshot {
		t.Fatal("replacement sidecar metadata became an append stream")
	}
}

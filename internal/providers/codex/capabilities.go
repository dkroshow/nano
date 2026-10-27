package codex

import (
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/providers"
)

func Declaration() providers.Declaration {
	return providers.Declaration{
		Lifecycle: lifecycle.Capabilities{NativeTurnID: true, NativeTurnStart: true, ResumeWorkSnapshot: true, NativeRecovery: true, ResumeWithoutTranscript: true},
		Turns: providers.Capabilities{
			InterruptedWithoutUser: true, RunningTurnInput: true,
			ModelChange: providers.AtNextTurn, EffortChange: providers.AtNextTurn, ServiceTier: true, ApprovalPolicy: true,
			Interactions: []providers.InteractionKind{providers.CommandApproval, providers.FileChangeApproval, providers.Permissions, providers.UserInput, providers.Elicitation},
			Instructions: providers.InstructionsNative, Inventory: providers.InventoryQuery,
			Metadata: providers.NativeMetadataEvents, MetadataAppend: true,
		},
	}
}

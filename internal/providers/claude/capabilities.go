package claude

import (
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/providers"
)

func Declaration() providers.Declaration {
	return providers.Declaration{
		Lifecycle: lifecycle.Capabilities{ReadyWithoutInit: true, ResumeWorkSnapshot: true},
		Turns: providers.Capabilities{
			ModelChange: providers.InSession, EffortChange: providers.AtCreation,
			Interactions: []providers.InteractionKind{providers.ToolPermission, providers.UserInput, providers.Elicitation},
			Instructions: providers.InstructionsExpected, Inventory: providers.InventoryInitialization,
			Metadata: providers.InitializationAndResult, MetadataAppend: true,
			// A prompt sent while a turn works is read by the CLI between its
			// steps; the saved transcript proves which turn absorbed it.
			RunningTurnInput: true,
		},
	}
}

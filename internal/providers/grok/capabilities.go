package grok

import (
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/providers"
)

func Declaration() providers.Declaration {
	return providers.Declaration{
		Lifecycle: lifecycle.Capabilities{NativeTurnID: true, ReadyWithoutInit: true, ResumeWorkSnapshot: true},
		Turns: providers.Capabilities{
			ModelChange: providers.AtCreation, EffortChange: providers.AtCreation,
			Interactions: []providers.InteractionKind{providers.ToolPermission, providers.UserInput},
			Instructions: providers.InstructionsUnavailable, Inventory: providers.InventoryUnavailable,
			Metadata: providers.SessionSnapshot, LineageSidecar: true,
		},
	}
}

package app

import (
	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/threads"
)

const (
	sidebarPlacementStandalone = threads.SidebarPlacementStandalone
	agentTurnCompleted         = threads.AgentTurnCompleted
)

const (
	turnOriginAgent = string(prompts.AgentOrigin)
)

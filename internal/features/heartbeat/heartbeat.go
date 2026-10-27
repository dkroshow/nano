package heartbeat

import (
	"fmt"
	"strings"
)

const (
	Available     = "available"
	Busy          = "busy"
	AwaitingHuman = "awaiting-human"
	Unreachable   = "unreachable"
)

func PausedWakePrompt(reason, arrived string) string {
	return fmt.Sprintf("This heartbeat is paused because: %s\n\nWhat arrived:\n%s\n\nHandle what arrived, then restate the outstanding wait.", strings.TrimSpace(reason), strings.TrimSpace(arrived))
}

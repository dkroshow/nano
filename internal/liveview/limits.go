package liveview

const (
	// StateTurnCap bounds retained live turns and their per-turn session evidence.
	StateTurnCap = 256
	// TurnTail is the number of recent turns returned by a live status tail.
	TurnTail = 5
)

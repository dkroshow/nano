package hosted

// TurnRequestHash includes next-turn intent in an otherwise stable prompt hash.
func TurnRequestHash(text string, nextTurn bool) string {
	if nextTurn {
		return "next_turn:" + AgentHash([]byte(text))
	}
	return AgentHash([]byte(text))
}

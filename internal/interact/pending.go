package interact

import "encoding/json"

// PendingInteraction is one parked provider request exposed to a person or agent.
// Params retain the provider payload so the decision card has the original evidence.
type PendingInteraction struct {
	ID        string          `json:"id"`     // the JSON-RPC request id, verbatim
	Method    string          `json:"method"` // the schema method name
	Kind      string          `json:"kind"`
	ThreadID  string          `json:"threadId"`
	TurnID    string          `json:"turnId,omitempty"`
	ItemID    string          `json:"itemId,omitempty"`
	CreatedAt string          `json:"createdAt"`
	ExpiresAt string          `json:"expiresAt,omitempty"` // autoResolutionMs deadline
	Params    json.RawMessage `json:"params"`
}

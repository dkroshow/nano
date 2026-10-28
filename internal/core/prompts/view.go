package prompts

// View is one prompt as the delivery journal serves it: its stored
// state and the actions and label promptPresentation gives it, the fields the
// live stream carries, with the identity of its delivery.
type View struct {
	ClientActionID     string   `json:"clientActionId"`
	Provider           string   `json:"provider"`
	NativeThreadID     string   `json:"nativeThreadId"`
	TextHash           string   `json:"textHash"`
	Text               string   `json:"text,omitempty"`
	State              string   `json:"state"`
	AllowedActions     []string `json:"allowedActions,omitempty"`
	Label              string   `json:"label,omitempty"`
	CreatedAt          string   `json:"createdAt"`
	UpdatedAt          string   `json:"updatedAt"`
	ProviderTurnID     string   `json:"providerTurnId,omitempty"`
	CanonicalMessageID string   `json:"canonicalMessageId,omitempty"`
	IdentityConflict   string   `json:"identityConflict,omitempty"`
}

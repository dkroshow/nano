package hosted

import (
	"encoding/json"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

// CreationRequest is the public hosted-thread creation body. Its field order
// preserves the canonical request encoding used by creation idempotency.
type CreationRequest struct {
	Provider              string                  `json:"provider"`
	Mode                  string                  `json:"mode"`
	ThreadID              string                  `json:"threadId"`
	Cwd                   string                  `json:"cwd"`
	Model                 string                  `json:"model"`
	Effort                string                  `json:"effort"`
	ServiceTier           string                  `json:"serviceTier"`
	Sandbox               string                  `json:"sandbox"`
	Approval              string                  `json:"approval"`
	PermissionMode        string                  `json:"permissionMode"`
	AllowedTools          string                  `json:"allowedTools"`
	Personality           string                  `json:"personality"`
	ApprovalsReviewer     string                  `json:"approvalsReviewer"`
	Ephemeral             bool                    `json:"ephemeral"`
	BaseInstructions      string                  `json:"baseInstructions"`
	DeveloperInstructions string                  `json:"developerInstructions"`
	ModelProvider         string                  `json:"modelProvider"`
	Config                json.RawMessage         `json:"config"`
	ThreadSource          string                  `json:"threadSource"`
	SessionStartSource    string                  `json:"sessionStartSource"`
	ServiceName           string                  `json:"serviceName"`
	ClientSessionID       string                  `json:"clientSessionId,omitempty"`
	InvokerThreadRef      string                  `json:"invokerThreadRef,omitempty"`
	Subscriptions         subscriptions.Choices   `json:"subscriptions,omitempty"`
	Assignment            *hostedstore.Assignment `json:"assignment,omitempty"`
	PresentationMode      string                  `json:"presentationMode,omitempty"`
	SidebarPlacement      string                  `json:"sidebarPlacement,omitempty"`
	Name                  string                  `json:"name"`
}

// CreationPlan supplies normalized admission fields and one bounded attachment action.
type CreationPlan struct {
	ClientSessionID, Provider, Mode, PresentationMode, SidebarPlacement, Name string
	Attach                                                                    func(record ownedthreads.SessionRecord, resume bool) (hostedID string, err error)
}

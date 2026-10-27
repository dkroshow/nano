package providers

import (
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type SettingMoment string

const (
	AtCreation SettingMoment = "creation"
	AtNextTurn SettingMoment = "next_turn"
	InSession  SettingMoment = "in_session"
)

type InteractionKind string

const (
	CommandApproval    InteractionKind = "commandApproval"
	FileChangeApproval InteractionKind = "fileChangeApproval"
	Permissions        InteractionKind = "permissions"
	UserInput          InteractionKind = "userInput"
	Elicitation        InteractionKind = "elicitation"
	ToolPermission     InteractionKind = "toolPermission"
)

type InstructionEvidence string

const (
	InstructionsUnavailable InstructionEvidence = "unavailable"
	InstructionsNative      InstructionEvidence = "native_reported"
	InstructionsExpected    InstructionEvidence = "documented_expected"
)

type InventorySource string

const (
	InventoryUnavailable    InventorySource = "unavailable"
	InventoryQuery          InventorySource = "runtime_query"
	InventoryInitialization InventorySource = "initialization_snapshot"
)

type MetadataSource string

const (
	NativeMetadataEvents    MetadataSource = "native_events"
	InitializationAndResult MetadataSource = "initialization_and_result"
	SessionSnapshot         MetadataSource = "session_snapshot"
)

// Declaration separates lifecycle evidence from turn/feature support. Concrete
// adapters expose these same values through the two capability methods.
type Declaration struct {
	Lifecycle lifecycle.Capabilities
	Turns     Capabilities
}

// Capabilities states provider differences, not permissions or cleanup proof.
// Capture fields belong to history/read surfaces, not new adapter turn verbs.
type Capabilities struct {
	InterruptedWithoutUser bool
	ModelChange            SettingMoment
	EffortChange           SettingMoment
	ServiceTier            bool
	ApprovalPolicy         bool
	Interactions           []InteractionKind
	Instructions           InstructionEvidence
	Inventory              InventorySource
	Metadata               MetadataSource
	MetadataAppend         bool
	LineageSidecar         bool
	// RunningTurnInput accepts an ordinary prompt into the running turn.
	RunningTurnInput bool
}

// PermissionValue preserves which launch field supplies permission evidence.
// It reports the requested value without authorizing or changing it.
func (c Capabilities) PermissionValue(posture Posture) string {
	if c.ApprovalPolicy {
		return posture.Approval
	}
	return posture.PermissionMode
}

func (c Capabilities) CheckSettings(moment SettingMoment, settings Settings) error {
	if moment != AtCreation && moment != AtNextTurn && moment != InSession {
		return unsupported("unsupported_setting_moment", "setting change moment is unknown")
	}
	for _, field := range []struct {
		value     *string
		supported SettingMoment
	}{
		{settings.Model, c.ModelChange}, {settings.Effort, c.EffortChange},
	} {
		if field.value == nil {
			continue
		}
		known := field.supported == AtCreation || field.supported == AtNextTurn || field.supported == InSession
		if !known || (moment != AtCreation && moment != field.supported) {
			return unsupported("unsupported_setting", "setting is unavailable at the requested change moment")
		}
	}
	if settings.ServiceTier != nil && (!c.ServiceTier || moment == InSession) {
		return unsupported("unsupported_setting", "service tier is unavailable at the requested change moment")
	}
	return nil
}

// CheckSubmissionSettings validates the settings carried by Submit at the
// moment each provider actually applies them. A live model control and a
// next-turn override can both be coordinated by Submit; creation-only values
// cannot. The capability, rather than a provider-name branch, selects the
// behavior.
func (c Capabilities) CheckSubmissionSettings(settings Settings) error {
	for _, field := range []struct {
		value     *string
		supported SettingMoment
	}{
		{settings.Model, c.ModelChange}, {settings.Effort, c.EffortChange},
	} {
		if field.value == nil {
			continue
		}
		if field.supported != AtNextTurn && field.supported != InSession {
			return unsupported("unsupported_setting", "setting is unavailable during submission")
		}
	}
	if settings.ServiceTier != nil && !c.ServiceTier {
		return unsupported("unsupported_setting", "service tier is unavailable during submission")
	}
	return nil
}

func (c Capabilities) CheckInteraction(kind InteractionKind) error {
	for _, supported := range c.Interactions {
		if kind == supported {
			return nil
		}
	}
	return unsupported("unsupported_interaction", "interaction kind is not supported")
}

func unsupported(code, message string) error {
	return model.Diagnostic{Code: code, Severity: "error", Message: message}
}

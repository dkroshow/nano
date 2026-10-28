package hosted

import "encoding/json"

const (
	// PostureFieldModel names the canonical model field.
	PostureFieldModel = "model"
	// PostureFieldEffort names the canonical reasoning-effort field.
	PostureFieldEffort = "effort"
	// PostureFieldPermissionMode names the canonical permission-mode field.
	PostureFieldPermissionMode = "permissionMode"
	// PostureFieldServiceTier names the canonical service-tier field.
	PostureFieldServiceTier = "serviceTier"

	// PostureSourceCreation marks evidence from the creation request.
	PostureSourceCreation = "creation"
	// PostureSourceOperator marks evidence from an operator change.
	PostureSourceOperator = "operator"
	// PostureSourceProvider marks evidence reported by the provider.
	PostureSourceProvider = "provider"

	// PostureEvidenceSpawnRequest identifies the provider spawn request.
	PostureEvidenceSpawnRequest = "spawn_request"
	// PostureEvidenceSystemInit identifies the Claude initialization event.
	PostureEvidenceSystemInit = "system.init"
	// PostureEvidenceSetModel identifies a model-setting request.
	PostureEvidenceSetModel = "set_model"
	// PostureEvidenceTurnOptions identifies options carried by a turn request.
	PostureEvidenceTurnOptions = "turn_options"
	// PostureEvidenceResumeOverlay identifies settings restored for resume.
	PostureEvidenceResumeOverlay = "resume_overlay"
)

// PostureFromFields projects the canonical display values into one posture.
func PostureFromFields(fields map[string]MetadataCurrent) Posture {
	p := Posture{}
	for field, entry := range fields {
		var decoded string
		if len(entry.DisplayValue) == 0 || json.Unmarshal(entry.DisplayValue, &decoded) != nil {
			continue
		}
		switch field {
		case PostureFieldModel:
			p.Model = decoded
		case PostureFieldEffort:
			p.Effort = decoded
		case PostureFieldPermissionMode:
			p.PermissionMode = decoded
		case PostureFieldServiceTier:
			p.ServiceTier = decoded
		}
	}
	return p
}

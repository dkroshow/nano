package main

const (
	postureFieldModel          = "model"
	postureFieldEffort         = "effort"
	postureFieldPermissionMode = "permissionMode"
	postureFieldServiceTier    = "serviceTier"

	postureSourceCreation = "creation"
	postureSourceOperator = "operator"
	postureSourceProvider = "provider"

	postureEvidenceSpawnRequest  = "spawn_request"
	postureEvidenceSystemInit    = "system.init"
	postureEvidenceSetModel      = "set_model"
	postureEvidenceTurnOptions   = "turn_options"
	postureEvidenceResumeOverlay = "resume_overlay"
)

// postureEntry is one canonical posture decision written to control.sqlite.
// Empty Value means the decision cleared the field.
type postureEntry struct {
	Field    string
	Value    string
	Source   string
	Evidence string
	At       string
}

type postureFieldValue struct{ field, value string }

func postureEntriesFor(source, evidence string, fields ...postureFieldValue) []postureEntry {
	entries := make([]postureEntry, 0, len(fields))
	for _, field := range fields {
		if field.value == "" {
			continue
		}
		entries = append(entries, postureEntry{
			Field: field.field, Value: field.value, Source: source, Evidence: evidence,
		})
	}
	return entries
}

func creationPostureEntries(req spawnRequest) []postureEntry {
	permission := req.PermissionMode
	if req.Provider == "codex" {
		permission = req.Approval
	}
	return postureEntriesFor(postureSourceCreation, postureEvidenceSpawnRequest,
		postureFieldValue{postureFieldModel, req.Model},
		postureFieldValue{postureFieldEffort, req.Effort},
		postureFieldValue{postureFieldPermissionMode, permission},
		postureFieldValue{postureFieldServiceTier, req.ServiceTier},
		postureFieldValue{"cwd", req.Cwd},
	)
}

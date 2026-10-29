package sessions

import (
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

const (
	postureFieldModel            = hostedstore.PostureFieldModel
	postureFieldEffort           = hostedstore.PostureFieldEffort
	postureFieldPermissionMode   = hostedstore.PostureFieldPermissionMode
	postureFieldServiceTier      = hostedstore.PostureFieldServiceTier
	postureSourceCreation        = hostedstore.PostureSourceCreation
	postureSourceOperator        = hostedstore.PostureSourceOperator
	postureSourceProvider        = hostedstore.PostureSourceProvider
	postureEvidenceSpawnRequest  = hostedstore.PostureEvidenceSpawnRequest
	postureEvidenceSystemInit    = hostedstore.PostureEvidenceSystemInit
	postureEvidenceSetModel      = hostedstore.PostureEvidenceSetModel
	postureEvidenceTurnOptions   = hostedstore.PostureEvidenceTurnOptions
	postureEvidenceResumeOverlay = hostedstore.PostureEvidenceResumeOverlay
)

// postureEntry is one canonical posture decision written to control.sqlite.
// Empty Value means the decision cleared the field.
type postureEntry = hostedstore.PostureEntry

type postureFieldValue = hostedstore.FieldValue

func postureEntriesFor(source, evidence string, fields ...postureFieldValue) []postureEntry {
	entries := make([]postureEntry, 0, len(fields))
	for _, field := range fields {
		if field.Value == "" {
			continue
		}
		entries = append(entries, postureEntry{
			Field: field.Field, Value: field.Value, Source: source, Evidence: evidence,
		})
	}
	return entries
}

func creationPostureEntries(req ProviderRequest) []postureEntry {
	permission := req.PermissionMode
	if req.Provider == "codex" {
		permission = req.Approval
	}
	return postureEntriesFor(postureSourceCreation, postureEvidenceSpawnRequest,
		postureFieldValue{Field: postureFieldModel, Value: req.Model},
		postureFieldValue{Field: postureFieldEffort, Value: req.Effort},
		postureFieldValue{Field: postureFieldPermissionMode, Value: permission},
		postureFieldValue{Field: postureFieldServiceTier, Value: req.ServiceTier},
		postureFieldValue{Field: "cwd", Value: req.Cwd},
	)
}

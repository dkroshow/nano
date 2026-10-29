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

func postureEntriesFor(source, evidence string, fields ...hostedstore.FieldValue) []hostedstore.PostureEntry {
	entries := make([]hostedstore.PostureEntry, 0, len(fields))
	for _, field := range fields {
		if field.Value == "" {
			continue
		}
		entries = append(entries, hostedstore.PostureEntry{
			Field: field.Field, Value: field.Value, Source: source, Evidence: evidence,
		})
	}
	return entries
}

func creationPostureEntries(req ProviderRequest) []hostedstore.PostureEntry {
	permission := req.PermissionMode
	if req.Provider == "codex" {
		permission = req.Approval
	}
	return postureEntriesFor(postureSourceCreation, postureEvidenceSpawnRequest,
		hostedstore.FieldValue{Field: postureFieldModel, Value: req.Model},
		hostedstore.FieldValue{Field: postureFieldEffort, Value: req.Effort},
		hostedstore.FieldValue{Field: postureFieldPermissionMode, Value: permission},
		hostedstore.FieldValue{Field: postureFieldServiceTier, Value: req.ServiceTier},
		hostedstore.FieldValue{Field: "cwd", Value: req.Cwd},
	)
}

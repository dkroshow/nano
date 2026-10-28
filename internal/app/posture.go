package app

import "github.com/dkroshow/nano/internal/hosted"

const (
	postureFieldModel            = hosted.PostureFieldModel
	postureFieldEffort           = hosted.PostureFieldEffort
	postureFieldPermissionMode   = hosted.PostureFieldPermissionMode
	postureFieldServiceTier      = hosted.PostureFieldServiceTier
	postureSourceCreation        = hosted.PostureSourceCreation
	postureSourceOperator        = hosted.PostureSourceOperator
	postureSourceProvider        = hosted.PostureSourceProvider
	postureEvidenceSpawnRequest  = hosted.PostureEvidenceSpawnRequest
	postureEvidenceSystemInit    = hosted.PostureEvidenceSystemInit
	postureEvidenceSetModel      = hosted.PostureEvidenceSetModel
	postureEvidenceTurnOptions   = hosted.PostureEvidenceTurnOptions
	postureEvidenceResumeOverlay = hosted.PostureEvidenceResumeOverlay
)

// postureEntry is one canonical posture decision written to control.sqlite.
// Empty Value means the decision cleared the field.
type postureEntry = hosted.PostureEntry

type postureFieldValue = hosted.FieldValue

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

func creationPostureEntries(req providerRequest) []postureEntry {
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

package sessions

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

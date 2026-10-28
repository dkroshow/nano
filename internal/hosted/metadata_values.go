package hosted

import "encoding/json"

// Metadata dispositions and freshness values are shared by evidence producers and readers.
const (
	// DispositionRequested records desired intent before provider confirmation.
	DispositionRequested = "requested"
	// DispositionConfirmed records a provider-confirmed value.
	DispositionConfirmed = "confirmed"
	// DispositionRejected records a provider-rejected value.
	DispositionRejected = "rejected"
	// DispositionCleared removes a previous desired override.
	DispositionCleared = "cleared"
	// DispositionObserved records a value observed from the provider.
	DispositionObserved = "observed"
	// DispositionUnknown marks a value without conclusive evidence.
	DispositionUnknown = "unknown"

	// FreshnessLive marks evidence from the active attachment.
	FreshnessLive = "live"
	// FreshnessHistorical marks evidence retained from an earlier attachment.
	FreshnessHistorical = "historical"
	// FreshnessReplayed marks evidence replayed from saved events.
	FreshnessReplayed = "replayed"

	// SourceMigration identifies evidence imported during migration.
	SourceMigration = "migration"
)

// MetadataString consumes the server-authored display projection.
// It deliberately returns no value for unknown rather than choosing a process
// default. Resume uses ResumeString instead because it intentionally
// preserves historical desired intent.
func MetadataString(current MetadataCurrent) (string, bool) {
	var value string
	if len(current.DisplayValue) == 0 || json.Unmarshal(current.DisplayValue, &value) != nil || value == "" {
		return "", false
	}
	return value, true
}

// ResumeString preserves an explicit operator/creation choice across
// a new attachment, including a provider-confirmed or rejected attempt. A
// cleared choice deliberately sends no override. Only a field with no desired
// event falls back to the provider's effective value.
func ResumeString(current MetadataCurrent) (string, bool) {
	encoded := current.EffectiveValue
	if current.DesiredEventID != nil {
		if current.DesiredDisposition == DispositionCleared {
			return "", false
		}
		encoded = current.DesiredValue
	}
	var value string
	if len(encoded) == 0 || json.Unmarshal(encoded, &value) != nil || value == "" {
		return "", false
	}
	return value, true
}

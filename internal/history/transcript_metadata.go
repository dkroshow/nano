package history

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func newTranscriptMetadataState(provider Provider, source SourceCandidate, previous *ThreadProjection) projectionState {
	state := projectionState{projection: initialProjection(provider, source, previous), source: source}
	// Discovery can carry an already-proven native parent edge from a separate
	// sidecar. A summary or update stream need not repeat that edge.
	if source.ParentNativeID != "" {
		state.projection.ParentNativeID = source.ParentNativeID
	}
	return state
}

// Provider readers supply typed values. This reducer never decodes native
// detail, branches on provider names or infers identity from source paths.
func applyTranscriptMetadata(state *projectionState, observation model.Observation) {
	if state.identityConflicted {
		return
	}
	if !catalogObservation(observation) || !catalogSourceScope(state, observation) {
		return
	}
	fields := observation.Metadata
	if identity := fields[model.MetadataNativeThreadID]; identity.State == model.MetadataKnown {
		if !setNativeIdentity(state, identity.Value, int(observation.Source.Line)) {
			return
		}
	}
	for _, clock := range observation.Times {
		if clock.Class != model.TimeNative {
			continue
		}
		switch clock.Meaning {
		case model.TimeActivity:
			observeActivity(state, catalogClockValue(clock))
		case model.TimeConversation:
			observeMessage(state, catalogClockValue(clock))
		case model.TimeThreadActivity:
			observeTimestamp(&state.projection, catalogClockValue(clock))
		}
	}
	stamp := metadataNativeTime(observation)
	for field, value := range fields {
		var target *MetadataField
		switch field {
		case model.MetadataCWD:
			target = &state.projection.Metadata.CWD
		case model.MetadataModel:
			target = &state.projection.Metadata.Model
		case model.MetadataEffort:
			target = &state.projection.Metadata.Effort
		case model.MetadataPriority:
			target = &state.projection.Metadata.Priority
		case model.MetadataPermission:
			target = &state.projection.Metadata.Permission
		case model.MetadataHistoricalBranch:
			target = &state.projection.Metadata.HistoricalBranch
		case model.MetadataOrigin:
			target = &state.projection.Metadata.Origin
		}
		if target != nil {
			switch value.State {
			case model.MetadataKnown:
				setKnown(target, value.Value, stamp)
			case model.MetadataUnknown:
				*target = UnknownField()
			case model.MetadataUnsupported:
				*target = UnsupportedField()
			}
			continue
		}
		if value.State != model.MetadataKnown {
			continue
		}
		switch field {
		case model.MetadataParentNativeID:
			state.projection.ParentNativeID = value.Value
		case model.MetadataSourceKind:
			state.projection.Kind = ThreadKind(value.Value)
		case model.MetadataTitle:
			setText(&state.projection.Title, value.Value)
		case model.MetadataPreview:
			setText(&state.projection.Preview, value.Value)
		case model.MetadataProviderVersion:
			setText(&state.projection.ProviderVersion, value.Value)
		}
	}
}

func catalogObservation(obs model.Observation) bool {
	if len(obs.Metadata) != 0 {
		return true
	}
	for _, clock := range obs.Times {
		if clock.Meaning == model.TimeActivity || clock.Meaning == model.TimeConversation || clock.Meaning == model.TimeThreadActivity {
			return true
		}
	}
	return false
}

// Source discovery owns filename identity and the root/worker selection. All
// facts inspected here are normalized scope/metadata; native envelopes remain
// the provider reader's responsibility.
func catalogSourceScope(state *projectionState, obs model.Observation) bool {
	if state.source.Kind == ThreadSubagent {
		base := strings.TrimSuffix(filepathBase(state.source.Path), filepathExt(state.source.Path))
		if strings.HasPrefix(base, "agent-") {
			expected := strings.TrimSpace(strings.TrimPrefix(base, "agent-"))
			if expected != "" && obs.Scope.NativeThreadID != nil && *obs.Scope.NativeThreadID != expected {
				addDiagnostic(&state.diagnostics, &state.summary, Diagnostic{Kind: DiagnosticIdentityConflict, Line: int(obs.Source.Line), Message: "native agent identity does not match its source filename"})
				return false
			}
		}
		return true
	}
	if obs.Scope.NativeParentCallID != nil {
		return false
	}
	if obs.Scope.ParentNativeID != nil && obs.Metadata[model.MetadataSourceKind].Value != string(ThreadFork) {
		return obs.Scope.NativeThreadID != nil && state.projection.NativeThreadID != "" && *obs.Scope.NativeThreadID == state.projection.NativeThreadID
	}
	return true
}

// A write clock attributes changed fields. Native event/start clocks provide
// fallbacks when a source has no valid write time. Capture/receipt clocks never
// become the catalog's native observation time.
func metadataNativeTime(observation model.Observation) *time.Time {
	for _, meaning := range []model.TimeMeaning{model.TimeWrite, model.TimeEvent, model.TimeStart} {
		for _, stamp := range observation.Times {
			if stamp.Class == model.TimeNative && stamp.Meaning == meaning && stamp.Value != nil {
				return catalogClockValue(stamp)
			}
		}
	}
	return nil
}

// The module normalizes instants to UTC. The existing catalog API preserves a
// native string's offset, so restore that representation only when the retained
// scalar proves the exact same instant. This never decodes a provider envelope.
func catalogClockValue(clock model.TimeObservation) *time.Time {
	if clock.Value == nil {
		return nil
	}
	var raw string
	if json.Unmarshal(clock.Raw, &raw) == nil {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err == nil && parsed.Equal(*clock.Value) {
			return &parsed
		}
	}
	return cloneTime(clock.Value)
}

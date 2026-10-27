package history

import (
	"bytes"
	"encoding/json"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// transcriptAliasTarget is a persisted observation's native alias. The store
// supplies candidates; resolving them requires no provider interpretation.
type transcriptAliasTarget struct {
	Scope         model.Scope
	Namespace     string
	Value         []byte
	ObservationID string
}

func resolveTranscriptRelations(batch model.Batch, targets []transcriptAliasTarget) model.Batch {
	next := batch
	next.Changes = append([]model.Change(nil), batch.Changes...)
	next.Diagnostics = append([]model.Diagnostic(nil), batch.Diagnostics...)
	for i, change := range next.Changes {
		if change.Relation == nil || change.Relation.ToNativeObservation == nil || change.Relation.ToObservation != "" {
			continue
		}
		relation := *change.Relation
		ref := relation.ToNativeObservation
		value, valid := transcriptNativeScalar(ref.Value)
		matches := map[string]bool{}
		if valid && ref.Namespace != "" && ref.Scope.Provider != "" && ref.Scope.Source != "" && ref.Scope.Incarnation != "" && ref.Scope.ItemID == "" && ref.Scope.TurnKey == "" {
			for _, target := range targets {
				other, ok := transcriptNativeScalar(target.Value)
				if target.ObservationID != "" && target.Namespace == ref.Namespace && ok && value == other && transcriptReferenceScopeMatches(ref.Scope, target.Scope) {
					matches[target.ObservationID] = true
				}
			}
		}
		if len(matches) == 1 {
			for id := range matches {
				relation.ToObservation = id
				relation.ObservationRefs = append(append([]string(nil), relation.ObservationRefs...), id)
			}
			next.Changes[i].Relation = &relation
			continue
		}
		code := "unresolved_native_reference"
		if len(matches) > 1 {
			code = "ambiguous_native_reference"
		}
		diagnostic := model.Diagnostic{Code: code, Severity: "warning", Scope: relation.Scope, Message: "Native reference has no unique persisted observation; incoming evidence remains standalone"}
		for _, observation := range batch.Observations {
			if observation.ID == relation.FromObservation {
				diagnostic.Scope, diagnostic.Source = observation.Scope, observation.Source
				break
			}
		}
		next.Diagnostics = append(next.Diagnostics, diagnostic)
	}
	return next
}

func transcriptNativeScalar(raw []byte) (string, bool) {
	if !json.Valid(raw) {
		return "", false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return "", false
	}
	switch value.(type) {
	case string, json.Number, bool:
		encoded, _ := json.Marshal(value)
		return string(encoded), true
	default:
		return "", false
	}
}

func transcriptReferenceScopeMatches(ref, target model.Scope) bool {
	if ref.Provider != target.Provider || ref.Source != target.Source || ref.Incarnation != target.Incarnation || ref.ConnectionID != "" && ref.ConnectionID != target.ConnectionID {
		return false
	}
	for _, pair := range [][2]*string{{ref.NativeThreadID, target.NativeThreadID}, {ref.NativeTurnID, target.NativeTurnID}, {ref.ParentNativeID, target.ParentNativeID}, {ref.NativeParentCallID, target.NativeParentCallID}} {
		if pair[0] != nil && (pair[1] == nil || *pair[0] != *pair[1]) {
			return false
		}
	}
	return true
}

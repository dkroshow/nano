package assemble

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Catalog remembers only the last value and its native attribution per field.
// Readers need this small continuation to emit changes and preserve native
// first-title/first-preview rules. It is not an emitted thread snapshot.
type Catalog map[model.MetadataField]CatalogFact

type CatalogFact struct {
	Value      model.MetadataValue `json:"value"`
	NativeTime string              `json:"nativeTime,omitempty"`
}

func (prior Catalog) Changes(fields model.Metadata, nativeTime *time.Time) (Catalog, model.Metadata) {
	next := make(Catalog, len(prior)+len(fields))
	for field, fact := range prior {
		next[field] = fact
	}
	var changes model.Metadata
	for field, value := range fields {
		fact := CatalogFact{Value: value}
		// MetadataField.ObservedAt is part of the existing catalog contract:
		// a new native attribution is evidence even when the text is unchanged.
		switch field {
		case model.MetadataCWD, model.MetadataModel, model.MetadataEffort, model.MetadataPriority, model.MetadataPermission, model.MetadataHistoricalBranch, model.MetadataOrigin:
			if value.State == model.MetadataKnown && nativeTime != nil {
				fact.NativeTime = nativeTime.Format(time.RFC3339Nano)
			}
		}
		if previous, exists := prior[field]; exists && previous == fact {
			continue
		}
		if changes == nil {
			changes = model.Metadata{}
		}
		changes[field], next[field] = value, fact
	}
	return next, changes
}

func CatalogKnown(fields model.Metadata, field model.MetadataField, value string) {
	if value = strings.TrimSpace(value); value != "" {
		fields[field] = model.MetadataValue{State: model.MetadataKnown, Value: value}
	}
}

func CatalogPreview(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	runes := []rune(value)
	if len(runes) > 240 {
		return string(runes[:239]) + "…"
	}
	return value
}

// CatalogTime preserves the legacy native clock fallback, including numeric
// epoch seconds/milliseconds. The reader supplies the ordered native fields;
// this helper neither chooses provider fields nor reads a wall clock.
func CatalogTime(fields map[string]json.RawMessage, keys ...string) (string, json.RawMessage, *time.Time) {
	for _, key := range keys {
		raw := fields[key]
		var value string
		if json.Unmarshal(raw, &value) == nil {
			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				return key, raw, &parsed
			}
		}
		var number json.Number
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&number) != nil {
			continue
		}
		seconds, err := strconv.ParseFloat(number.String(), 64)
		if err != nil {
			continue
		}
		if seconds > 1e12 {
			seconds /= 1000
		}
		whole := int64(seconds)
		parsed := time.Unix(whole, int64((seconds-float64(whole))*float64(time.Second))).UTC()
		if parsed.Year() >= 0 && parsed.Year() <= 9999 {
			return key, raw, &parsed
		}
	}
	return "", nil, nil
}

func CatalogObservation(base model.Observation, fields model.Metadata, field string, raw json.RawMessage, stamp *time.Time) model.Observation {
	obs := model.Observation{ID: base.ID + "/catalog", Kind: model.ObservationMetadata, Scope: base.Scope, Source: base.Source, Capture: base.Capture, Metadata: fields}
	if field != "" {
		if stamp != nil {
			normalized := stamp.UTC()
			stamp = &normalized
		}
		obs.Times = append(obs.Times, model.TimeObservation{Scope: obs.Scope, Class: model.TimeNative, Meaning: model.TimeWrite, SourceField: field, Raw: raw, Value: stamp, Precision: "native", ObservationRef: obs.ID})
	}
	for _, clock := range base.Times {
		if clock.Class != model.TimeNative {
			clock.Scope, clock.ObservationRef = obs.Scope, obs.ID
			obs.Times = append(obs.Times, clock)
		}
	}
	return obs
}

// CatalogActivity classifies a native write clock without changing its value,
// source field or precision. No capture/receipt clock can supply this fact.
func CatalogActivity(obs *model.Observation, meaning model.TimeMeaning) {
	for _, clock := range obs.Times {
		if clock.Class == model.TimeNative && clock.Meaning == model.TimeWrite && clock.Value != nil {
			clock.Meaning = meaning
			obs.Times = append(obs.Times, clock)
			return
		}
	}
}

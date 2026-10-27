package model

type UsageGrain string

const (
	UsageResponse UsageGrain = "response"
	UsageTurn     UsageGrain = "turn"
	UsageThread   UsageGrain = "thread"
)

// UsageObservation retains native samples even when a different grain supplies
// the turn summary. Revisions replace a sample, never create another call.
type UsageObservation struct {
	ID              string                `json:"id"`
	Scope           Scope                 `json:"scope"`
	Grain           UsageGrain            `json:"grain"`
	SampleID        string                `json:"sampleId"`
	Revision        string                `json:"revision,omitempty"`
	Semantics       string                `json:"semantics"`
	Values          TokenUsage            `json:"values"`
	Incomplete      *bool                 `json:"incomplete,omitempty"`
	ModelUsage      map[string]TokenUsage `json:"modelUsage,omitempty"`
	ResetEpoch      string                `json:"resetEpoch,omitempty"`
	ObservationRefs []string              `json:"observationRefs"`
}

// CounterCoverage counts reporting observations among the selected samples.
// It makes no claim about provider calls absent from the source capture.
type CounterCoverage struct {
	Reported int `json:"reported"`
	Samples  int `json:"samples"`
}

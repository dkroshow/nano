package history

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// These private records retain update facts and source references. Transcript
// text/detail live only in the existing view/body; observation payloads stay
// in the source. No compact record is a substitute for a guarded payload read.
type storedParsedAlias struct {
	Namespace       string      `json:"n"`
	Value           int64       `json:"v"`
	Authority       model.Basis `json:"a"`
	ObservationRefs []int64     `json:"r"`
}

func storeParsedAliasTx(ctx context.Context, tx *sql.Tx, projectionID int64, input model.Alias) (storedParsedAlias, error) {
	var output storedParsedAlias
	var err error
	output.Namespace = input.Namespace
	output.Value, err = internTranscriptValueTx(ctx, tx, projectionID, input.Value)
	if err != nil {
		return output, err
	}
	output.Authority = input.Authority
	output.ObservationRefs, err = storeTranscriptRefsTx(ctx, tx, projectionID, input.ObservationRefs)
	if err != nil {
		return output, err
	}
	return output, nil
}

func loadParsedAliasTx(ctx context.Context, tx *sql.Tx, projectionID int64, input storedParsedAlias) (model.Alias, error) {
	var output model.Alias
	var err error
	output.Namespace = input.Namespace
	err = loadTranscriptValueTx(ctx, tx, projectionID, input.Value, &output.Value)
	if err != nil {
		return output, err
	}
	output.Authority = input.Authority
	output.ObservationRefs, err = loadTranscriptRefsTx(ctx, tx, projectionID, input.ObservationRefs)
	if err != nil {
		return output, err
	}
	return output, nil
}

type storedParsedFinality struct {
	State           model.FinalityState `json:"s"`
	Basis           model.Basis         `json:"b,omitempty"`
	ObservationRefs []int64             `json:"r,omitempty"`
}

func storeParsedFinalityTx(ctx context.Context, tx *sql.Tx, projectionID int64, input model.Finality) (storedParsedFinality, error) {
	var output storedParsedFinality
	var err error
	output.State = input.State
	output.Basis = input.Basis
	output.ObservationRefs, err = storeTranscriptRefsTx(ctx, tx, projectionID, input.ObservationRefs)
	if err != nil {
		return output, err
	}
	return output, nil
}

func loadParsedFinalityTx(ctx context.Context, tx *sql.Tx, projectionID int64, input storedParsedFinality) (model.Finality, error) {
	var output model.Finality
	var err error
	output.State = input.State
	output.Basis = input.Basis
	output.ObservationRefs, err = loadTranscriptRefsTx(ctx, tx, projectionID, input.ObservationRefs)
	if err != nil {
		return output, err
	}
	return output, nil
}

type storedParsedPart struct {
	Type           string              `json:"t"`
	Index          *int                `json:"i,omitempty"`
	Availability   model.Availability  `json:"a"`
	Source         storedSourceAddress `json:"s"`
	OriginalBytes  *int64              `json:"b,omitempty"`
	OmissionReason string              `json:"o,omitempty"`
}

func storeParsedPartTx(ctx context.Context, tx *sql.Tx, projectionID int64, input model.Part) (storedParsedPart, error) {
	var output storedParsedPart
	var err error
	output.Type = input.Type
	output.Index = input.Index
	output.Availability = input.Availability
	output.Source, err = storeSourceAddressTx(ctx, tx, projectionID, input.Source)
	if err != nil {
		return output, err
	}
	output.OriginalBytes = input.OriginalBytes
	output.OmissionReason = input.OmissionReason
	return output, nil
}

func loadParsedPartTx(ctx context.Context, tx *sql.Tx, projectionID int64, input storedParsedPart) (model.Part, error) {
	var output model.Part
	var err error
	output.Type = input.Type
	output.Index = input.Index
	output.Availability = input.Availability
	output.Source, err = loadSourceAddressTx(ctx, tx, projectionID, input.Source)
	if err != nil {
		return output, err
	}
	output.OriginalBytes = input.OriginalBytes
	output.OmissionReason = input.OmissionReason
	return output, nil
}

type storedParsedCompletionFact struct {
	Scope           int64         `json:"s"`
	Outcome         model.Outcome `json:"o"`
	Basis           model.Basis   `json:"b"`
	ObservationRefs []int64       `json:"r"`
	Reason          string        `json:"x,omitempty"`
}

func storeParsedCompletionFactTx(ctx context.Context, tx *sql.Tx, projectionID int64, input model.CompletionFact) (storedParsedCompletionFact, error) {
	var output storedParsedCompletionFact
	var err error
	output.Scope, err = internTranscriptValueTx(ctx, tx, projectionID, input.Scope)
	if err != nil {
		return output, err
	}
	output.Outcome = input.Outcome
	output.Basis = input.Basis
	output.ObservationRefs, err = storeTranscriptRefsTx(ctx, tx, projectionID, input.ObservationRefs)
	if err != nil {
		return output, err
	}
	output.Reason = input.Reason
	return output, nil
}

func loadParsedCompletionFactTx(ctx context.Context, tx *sql.Tx, projectionID int64, input storedParsedCompletionFact) (model.CompletionFact, error) {
	var output model.CompletionFact
	var err error
	err = loadTranscriptValueTx(ctx, tx, projectionID, input.Scope, &output.Scope)
	if err != nil {
		return output, err
	}
	output.Outcome = input.Outcome
	output.Basis = input.Basis
	output.ObservationRefs, err = loadTranscriptRefsTx(ctx, tx, projectionID, input.ObservationRefs)
	if err != nil {
		return output, err
	}
	output.Reason = input.Reason
	return output, nil
}

type storedParsedDuration struct {
	Scope          int64  `json:"s"`
	Value          int64  `json:"v"`
	Unit           string `json:"u"`
	Basis          string `json:"b"`
	SourceField    string `json:"f"`
	ObservationRef int64  `json:"r"`
}

func storeParsedDurationTx(ctx context.Context, tx *sql.Tx, projectionID int64, input model.Duration) (storedParsedDuration, error) {
	var output storedParsedDuration
	var err error
	output.Scope, err = internTranscriptValueTx(ctx, tx, projectionID, input.Scope)
	if err != nil {
		return output, err
	}
	output.Value = input.Value
	output.Unit = input.Unit
	output.Basis = input.Basis
	output.SourceField = input.SourceField
	if input.ObservationRef != "" {
		output.ObservationRef, err = internTranscriptValueTx(ctx, tx, projectionID, input.ObservationRef)
		if err != nil {
			return output, err
		}
	}
	return output, nil
}

func loadParsedDurationTx(ctx context.Context, tx *sql.Tx, projectionID int64, input storedParsedDuration) (model.Duration, error) {
	var output model.Duration
	var err error
	err = loadTranscriptValueTx(ctx, tx, projectionID, input.Scope, &output.Scope)
	if err != nil {
		return output, err
	}
	output.Value = input.Value
	output.Unit = input.Unit
	output.Basis = input.Basis
	output.SourceField = input.SourceField
	if input.ObservationRef != 0 {
		err = loadTranscriptValueTx(ctx, tx, projectionID, input.ObservationRef, &output.ObservationRef)
		if err != nil {
			return output, err
		}
	}
	return output, nil
}

type storedParsedUsageObservation struct {
	ID              int64                       `json:"i"`
	Scope           int64                       `json:"s"`
	Grain           model.UsageGrain            `json:"g"`
	SampleID        int64                       `json:"k"`
	Revision        string                      `json:"r,omitempty"`
	Semantics       string                      `json:"m"`
	Values          model.TokenUsage            `json:"v"`
	Incomplete      *bool                       `json:"c,omitempty"`
	ModelUsage      map[string]model.TokenUsage `json:"u,omitempty"`
	ResetEpoch      string                      `json:"e,omitempty"`
	ObservationRefs []int64                     `json:"o"`
}

func storeParsedUsageObservationTx(ctx context.Context, tx *sql.Tx, projectionID int64, input model.UsageObservation) (storedParsedUsageObservation, error) {
	var output storedParsedUsageObservation
	var err error
	if input.ID != "" {
		output.ID, err = internTranscriptValueTx(ctx, tx, projectionID, input.ID)
		if err != nil {
			return output, err
		}
	}
	output.Scope, err = internTranscriptValueTx(ctx, tx, projectionID, input.Scope)
	if err != nil {
		return output, err
	}
	output.Grain = input.Grain
	if input.SampleID != "" {
		output.SampleID, err = internTranscriptValueTx(ctx, tx, projectionID, input.SampleID)
		if err != nil {
			return output, err
		}
	}
	output.Revision = input.Revision
	output.Semantics = input.Semantics
	output.Values = input.Values
	output.Incomplete = input.Incomplete
	output.ModelUsage = input.ModelUsage
	output.ResetEpoch = input.ResetEpoch
	output.ObservationRefs, err = storeTranscriptRefsTx(ctx, tx, projectionID, input.ObservationRefs)
	if err != nil {
		return output, err
	}
	return output, nil
}

func loadParsedUsageObservationTx(ctx context.Context, tx *sql.Tx, projectionID int64, input storedParsedUsageObservation) (model.UsageObservation, error) {
	var output model.UsageObservation
	var err error
	if input.ID != 0 {
		err = loadTranscriptValueTx(ctx, tx, projectionID, input.ID, &output.ID)
		if err != nil {
			return output, err
		}
	}
	err = loadTranscriptValueTx(ctx, tx, projectionID, input.Scope, &output.Scope)
	if err != nil {
		return output, err
	}
	output.Grain = input.Grain
	if input.SampleID != 0 {
		err = loadTranscriptValueTx(ctx, tx, projectionID, input.SampleID, &output.SampleID)
		if err != nil {
			return output, err
		}
	}
	output.Revision = input.Revision
	output.Semantics = input.Semantics
	output.Values = input.Values
	output.Incomplete = input.Incomplete
	output.ModelUsage = input.ModelUsage
	output.ResetEpoch = input.ResetEpoch
	output.ObservationRefs, err = loadTranscriptRefsTx(ctx, tx, projectionID, input.ObservationRefs)
	if err != nil {
		return output, err
	}
	return output, nil
}

type storedParsedItem struct {
	Scope             int64                `json:"s"`
	Aliases           []storedParsedAlias  `json:"a,omitempty"`
	Parts             []storedParsedPart   `json:"p,omitempty"`
	Times             []storedTimeFact     `json:"t,omitempty"`
	Capture           model.CaptureState   `json:"c"`
	Outcome           model.Outcome        `json:"o"`
	Complete          bool                 `json:"d"`
	Finality          storedParsedFinality `json:"f"`
	ID                int64                `json:"i"`
	SourceSeq         int64                `json:"q"`
	SourceKey         int64                `json:"sk,omitempty"`
	NativeItemID      int64                `json:"ni,omitempty"`
	TurnKey           int64                `json:"tk"`
	TurnOrdinal       int                  `json:"tn"`
	Type              model.ItemType       `json:"ty"`
	Category          model.Category       `json:"ca"`
	Status            model.ItemStatus     `json:"st"`
	Role              model.Role           `json:"ro,omitempty"`
	ToolName          string               `json:"tl,omitempty"`
	CallID            int64                `json:"ci,omitempty"`
	CreatedAt         *time.Time           `json:"ct,omitempty"`
	Phase             string               `json:"ph,omitempty"`
	FilterTags        []string             `json:"ft,omitempty"`
	Synthetic         bool                 `json:"sy,omitempty"`
	Sidechain         bool                 `json:"sc,omitempty"`
	IsFinalAnswer     bool                 `json:"fa,omitempty"`
	ContentRef        int64                `json:"cr,omitempty"`
	ContentTruncated  bool                 `json:"tr,omitempty"`
	OriginalBytes     int64                `json:"ob,omitempty"`
	SourceLine        int                  `json:"ln,omitempty"`
	SourceRowType     string               `json:"rt,omitempty"`
	SourcePayloadType string               `json:"pt,omitempty"`
	SourceLineSHA256  string               `json:"sh,omitempty"`
	EmissionIndex     int                  `json:"ei,omitempty"`
}

func storeParsedItemTx(ctx context.Context, tx *sql.Tx, projectionID int64, input model.Item) (storedParsedItem, error) {
	var output storedParsedItem
	var err error
	output.Scope, err = internTranscriptValueTx(ctx, tx, projectionID, input.Scope)
	if err != nil {
		return output, err
	}
	if input.Aliases != nil {
		output.Aliases = make([]storedParsedAlias, len(input.Aliases))
		for i, value := range input.Aliases {
			output.Aliases[i], err = storeParsedAliasTx(ctx, tx, projectionID, value)
			if err != nil {
				return output, err
			}
		}
	}
	if input.Parts != nil {
		output.Parts = make([]storedParsedPart, len(input.Parts))
		for i, value := range input.Parts {
			output.Parts[i], err = storeParsedPartTx(ctx, tx, projectionID, value)
			if err != nil {
				return output, err
			}
		}
	}
	output.Times, err = storeTranscriptTimesTx(ctx, tx, projectionID, input.Times)
	if err != nil {
		return output, err
	}
	output.Capture = input.Capture
	output.Outcome = input.Outcome
	output.Complete = input.Complete
	output.Finality, err = storeParsedFinalityTx(ctx, tx, projectionID, input.Finality)
	if err != nil {
		return output, err
	}
	if input.ID != "" {
		output.ID, err = internTranscriptValueTx(ctx, tx, projectionID, input.ID)
		if err != nil {
			return output, err
		}
	}
	output.SourceSeq = input.SourceSeq
	if input.SourceKey != "" {
		output.SourceKey, err = internTranscriptValueTx(ctx, tx, projectionID, input.SourceKey)
		if err != nil {
			return output, err
		}
	}
	if input.NativeItemID != "" {
		output.NativeItemID, err = internTranscriptValueTx(ctx, tx, projectionID, input.NativeItemID)
		if err != nil {
			return output, err
		}
	}
	if input.TurnKey != "" {
		output.TurnKey, err = internTranscriptValueTx(ctx, tx, projectionID, input.TurnKey)
		if err != nil {
			return output, err
		}
	}
	output.TurnOrdinal = input.TurnOrdinal
	output.Type = input.Type
	output.Category = input.Category
	output.Status = input.Status
	output.Role = input.Role
	output.ToolName = input.ToolName
	if input.CallID != "" {
		output.CallID, err = internTranscriptValueTx(ctx, tx, projectionID, input.CallID)
		if err != nil {
			return output, err
		}
	}
	output.CreatedAt = input.CreatedAt
	output.Phase = input.Phase
	output.FilterTags = input.FilterTags
	output.Synthetic = input.Synthetic
	output.Sidechain = input.Sidechain
	output.IsFinalAnswer = input.IsFinalAnswer
	if input.ContentRef != "" {
		output.ContentRef, err = internTranscriptValueTx(ctx, tx, projectionID, input.ContentRef)
		if err != nil {
			return output, err
		}
	}
	output.ContentTruncated = input.ContentTruncated
	output.OriginalBytes = input.OriginalBytes
	output.SourceLine = input.SourceLine
	output.SourceRowType = input.SourceRowType
	output.SourcePayloadType = input.SourcePayloadType
	output.SourceLineSHA256 = input.SourceLineSHA256
	output.EmissionIndex = input.EmissionIndex
	return output, nil
}

func loadParsedItemTx(ctx context.Context, tx *sql.Tx, projectionID int64, input storedParsedItem) (model.Item, error) {
	var output model.Item
	var err error
	err = loadTranscriptValueTx(ctx, tx, projectionID, input.Scope, &output.Scope)
	if err != nil {
		return output, err
	}
	if input.Aliases != nil {
		output.Aliases = make([]model.Alias, len(input.Aliases))
		for i, value := range input.Aliases {
			output.Aliases[i], err = loadParsedAliasTx(ctx, tx, projectionID, value)
			if err != nil {
				return output, err
			}
		}
	}
	if input.Parts != nil {
		output.Parts = make([]model.Part, len(input.Parts))
		for i, value := range input.Parts {
			output.Parts[i], err = loadParsedPartTx(ctx, tx, projectionID, value)
			if err != nil {
				return output, err
			}
		}
	}
	output.Times, err = loadTranscriptTimesTx(ctx, tx, projectionID, input.Times)
	if err != nil {
		return output, err
	}
	output.Capture = input.Capture
	output.Outcome = input.Outcome
	output.Complete = input.Complete
	output.Finality, err = loadParsedFinalityTx(ctx, tx, projectionID, input.Finality)
	if err != nil {
		return output, err
	}
	if input.ID != 0 {
		err = loadTranscriptValueTx(ctx, tx, projectionID, input.ID, &output.ID)
		if err != nil {
			return output, err
		}
	}
	output.SourceSeq = input.SourceSeq
	if input.SourceKey != 0 {
		err = loadTranscriptValueTx(ctx, tx, projectionID, input.SourceKey, &output.SourceKey)
		if err != nil {
			return output, err
		}
	}
	if input.NativeItemID != 0 {
		err = loadTranscriptValueTx(ctx, tx, projectionID, input.NativeItemID, &output.NativeItemID)
		if err != nil {
			return output, err
		}
	}
	if input.TurnKey != 0 {
		err = loadTranscriptValueTx(ctx, tx, projectionID, input.TurnKey, &output.TurnKey)
		if err != nil {
			return output, err
		}
	}
	output.TurnOrdinal = input.TurnOrdinal
	output.Type = input.Type
	output.Category = input.Category
	output.Status = input.Status
	output.Role = input.Role
	output.ToolName = input.ToolName
	if input.CallID != 0 {
		err = loadTranscriptValueTx(ctx, tx, projectionID, input.CallID, &output.CallID)
		if err != nil {
			return output, err
		}
	}
	output.CreatedAt = input.CreatedAt
	output.Phase = input.Phase
	output.FilterTags = input.FilterTags
	output.Synthetic = input.Synthetic
	output.Sidechain = input.Sidechain
	output.IsFinalAnswer = input.IsFinalAnswer
	if input.ContentRef != 0 {
		err = loadTranscriptValueTx(ctx, tx, projectionID, input.ContentRef, &output.ContentRef)
		if err != nil {
			return output, err
		}
	}
	output.ContentTruncated = input.ContentTruncated
	output.OriginalBytes = input.OriginalBytes
	output.SourceLine = input.SourceLine
	output.SourceRowType = input.SourceRowType
	output.SourcePayloadType = input.SourcePayloadType
	output.SourceLineSHA256 = input.SourceLineSHA256
	output.EmissionIndex = input.EmissionIndex
	return output, nil
}

type storedParsedTurn struct {
	Capture            model.CaptureState               `json:"c"`
	Outcome            model.Outcome                    `json:"o"`
	Scope              int64                            `json:"s"`
	Completion         []storedParsedCompletionFact     `json:"x,omitempty"`
	FinalItemRefs      []int64                          `json:"f,omitempty"`
	Times              []storedTimeFact                 `json:"t,omitempty"`
	Durations          []storedParsedDuration           `json:"d,omitempty"`
	UsageObservations  []storedParsedUsageObservation   `json:"u,omitempty"`
	UsageBasis         string                           `json:"b,omitempty"`
	UsageRefs          []int64                          `json:"r,omitempty"`
	UsageCoverage      map[string]model.CounterCoverage `json:"cv,omitempty"`
	UsageIncomplete    bool                             `json:"ui,omitempty"`
	Key                int64                            `json:"k"`
	NativeTurnID       int64                            `json:"n,omitempty"`
	Ordinal            int                              `json:"q"`
	StartSourceSeq     int64                            `json:"ss"`
	EndSourceSeq       int64                            `json:"es"`
	Status             model.TurnStatus                 `json:"st"`
	CompletionEvidence model.CompletionEvidence         `json:"ce"`
	FinalItemID        int64                            `json:"fi,omitempty"`
	StartedAt          *time.Time                       `json:"sa,omitempty"`
	CompletedAt        *time.Time                       `json:"ca,omitempty"`
	DurationMS         int64                            `json:"ms,omitempty"`
	Config             json.RawMessage                  `json:"cf,omitempty"`
	Usage              *model.TokenUsage                `json:"us,omitempty"`
	Model              string                           `json:"mo,omitempty"`
	Effort             string                           `json:"ef,omitempty"`
	ServiceTier        string                           `json:"sr,omitempty"`
}

func storeParsedTurnTx(ctx context.Context, tx *sql.Tx, projectionID int64, input model.Turn) (storedParsedTurn, error) {
	var output storedParsedTurn
	var err error
	output.Capture = input.Capture
	output.Outcome = input.Outcome
	output.Scope, err = internTranscriptValueTx(ctx, tx, projectionID, input.Scope)
	if err != nil {
		return output, err
	}
	if input.Completion != nil {
		output.Completion = make([]storedParsedCompletionFact, len(input.Completion))
		for i, value := range input.Completion {
			output.Completion[i], err = storeParsedCompletionFactTx(ctx, tx, projectionID, value)
			if err != nil {
				return output, err
			}
		}
	}
	output.FinalItemRefs, err = storeTranscriptRefsTx(ctx, tx, projectionID, input.FinalItemRefs)
	if err != nil {
		return output, err
	}
	output.Times, err = storeTranscriptTimesTx(ctx, tx, projectionID, input.Times)
	if err != nil {
		return output, err
	}
	if input.Durations != nil {
		output.Durations = make([]storedParsedDuration, len(input.Durations))
		for i, value := range input.Durations {
			output.Durations[i], err = storeParsedDurationTx(ctx, tx, projectionID, value)
			if err != nil {
				return output, err
			}
		}
	}
	if input.UsageObservations != nil {
		output.UsageObservations = make([]storedParsedUsageObservation, len(input.UsageObservations))
		for i, value := range input.UsageObservations {
			output.UsageObservations[i], err = storeParsedUsageObservationTx(ctx, tx, projectionID, value)
			if err != nil {
				return output, err
			}
		}
	}
	output.UsageBasis = input.UsageBasis
	output.UsageRefs, err = storeTranscriptRefsTx(ctx, tx, projectionID, input.UsageRefs)
	if err != nil {
		return output, err
	}
	output.UsageCoverage = input.UsageCoverage
	output.UsageIncomplete = input.UsageIncomplete
	if input.Key != "" {
		output.Key, err = internTranscriptValueTx(ctx, tx, projectionID, input.Key)
		if err != nil {
			return output, err
		}
	}
	if input.NativeTurnID != "" {
		output.NativeTurnID, err = internTranscriptValueTx(ctx, tx, projectionID, input.NativeTurnID)
		if err != nil {
			return output, err
		}
	}
	output.Ordinal = input.Ordinal
	output.StartSourceSeq = input.StartSourceSeq
	output.EndSourceSeq = input.EndSourceSeq
	output.Status = input.Status
	output.CompletionEvidence = input.CompletionEvidence
	if input.FinalItemID != "" {
		output.FinalItemID, err = internTranscriptValueTx(ctx, tx, projectionID, input.FinalItemID)
		if err != nil {
			return output, err
		}
	}
	output.StartedAt = input.StartedAt
	output.CompletedAt = input.CompletedAt
	output.DurationMS = input.DurationMS
	output.Config = input.Config
	output.Usage = input.Usage
	output.Model = input.Model
	output.Effort = input.Effort
	output.ServiceTier = input.ServiceTier
	return output, nil
}

func loadParsedTurnTx(ctx context.Context, tx *sql.Tx, projectionID int64, input storedParsedTurn) (model.Turn, error) {
	var output model.Turn
	var err error
	output.Capture = input.Capture
	output.Outcome = input.Outcome
	err = loadTranscriptValueTx(ctx, tx, projectionID, input.Scope, &output.Scope)
	if err != nil {
		return output, err
	}
	if input.Completion != nil {
		output.Completion = make([]model.CompletionFact, len(input.Completion))
		for i, value := range input.Completion {
			output.Completion[i], err = loadParsedCompletionFactTx(ctx, tx, projectionID, value)
			if err != nil {
				return output, err
			}
		}
	}
	output.FinalItemRefs, err = loadTranscriptRefsTx(ctx, tx, projectionID, input.FinalItemRefs)
	if err != nil {
		return output, err
	}
	output.Times, err = loadTranscriptTimesTx(ctx, tx, projectionID, input.Times)
	if err != nil {
		return output, err
	}
	if input.Durations != nil {
		output.Durations = make([]model.Duration, len(input.Durations))
		for i, value := range input.Durations {
			output.Durations[i], err = loadParsedDurationTx(ctx, tx, projectionID, value)
			if err != nil {
				return output, err
			}
		}
	}
	if input.UsageObservations != nil {
		output.UsageObservations = make([]model.UsageObservation, len(input.UsageObservations))
		for i, value := range input.UsageObservations {
			output.UsageObservations[i], err = loadParsedUsageObservationTx(ctx, tx, projectionID, value)
			if err != nil {
				return output, err
			}
		}
	}
	output.UsageBasis = input.UsageBasis
	output.UsageRefs, err = loadTranscriptRefsTx(ctx, tx, projectionID, input.UsageRefs)
	if err != nil {
		return output, err
	}
	output.UsageCoverage = input.UsageCoverage
	output.UsageIncomplete = input.UsageIncomplete
	if input.Key != 0 {
		err = loadTranscriptValueTx(ctx, tx, projectionID, input.Key, &output.Key)
		if err != nil {
			return output, err
		}
	}
	if input.NativeTurnID != 0 {
		err = loadTranscriptValueTx(ctx, tx, projectionID, input.NativeTurnID, &output.NativeTurnID)
		if err != nil {
			return output, err
		}
	}
	output.Ordinal = input.Ordinal
	output.StartSourceSeq = input.StartSourceSeq
	output.EndSourceSeq = input.EndSourceSeq
	output.Status = input.Status
	output.CompletionEvidence = input.CompletionEvidence
	if input.FinalItemID != 0 {
		err = loadTranscriptValueTx(ctx, tx, projectionID, input.FinalItemID, &output.FinalItemID)
		if err != nil {
			return output, err
		}
	}
	output.StartedAt = input.StartedAt
	output.CompletedAt = input.CompletedAt
	output.DurationMS = input.DurationMS
	output.Config = input.Config
	output.Usage = input.Usage
	output.Model = input.Model
	output.Effort = input.Effort
	output.ServiceTier = input.ServiceTier
	return output, nil
}

// Source positions stay row-local; long source definitions are shared.
type storedSourceAddress struct {
	Source int64  `json:"s"`
	Path   int64  `json:"p,omitempty"`
	Line   int64  `json:"l,omitempty"`
	Frame  int64  `json:"f,omitempty"`
	Start  int64  `json:"b"`
	Length int64  `json:"n"`
	SHA256 string `json:"h,omitempty"`
}

func storeSourceAddressTx(ctx context.Context, tx *sql.Tx, projectionID int64, source model.SourceRef) (storedSourceAddress, error) {
	saved := storedSourceAddress{Line: source.Line, Frame: source.Frame, Start: source.Start, Length: source.End - source.Start, SHA256: source.SHA256}
	base := source
	base.Line, base.Frame, base.Start, base.End, base.BlockPath, base.SHA256 = 0, 0, 0, 0, "", ""
	var err error
	saved.Source, err = internTranscriptValueTx(ctx, tx, projectionID, base)
	if err != nil {
		return saved, err
	}
	if source.BlockPath != "" {
		saved.Path, err = internTranscriptValueTx(ctx, tx, projectionID, source.BlockPath)
	}
	return saved, err
}

func loadSourceAddressTx(ctx context.Context, tx *sql.Tx, projectionID int64, saved storedSourceAddress) (model.SourceRef, error) {
	var source model.SourceRef
	if err := loadTranscriptValueTx(ctx, tx, projectionID, saved.Source, &source); err != nil {
		return source, err
	}
	if saved.Path != 0 {
		if err := loadTranscriptValueTx(ctx, tx, projectionID, saved.Path, &source.BlockPath); err != nil {
			return source, err
		}
	}
	source.Line, source.Frame, source.Start, source.End, source.SHA256 = saved.Line, saved.Frame, saved.Start, saved.Start+saved.Length, saved.SHA256
	return source, nil
}

func encodeParsedItemTx(ctx context.Context, tx *sql.Tx, projectionID int64, item model.Item) ([]byte, error) {
	saved, err := storeParsedItemTx(ctx, tx, projectionID, item)
	if err != nil {
		return nil, err
	}
	return json.Marshal(saved)
}

func decodeParsedItemTx(ctx context.Context, tx *sql.Tx, projectionID int64, raw []byte) (model.Item, error) {
	var saved storedParsedItem
	if err := json.Unmarshal(raw, &saved); err != nil {
		return model.Item{}, err
	}
	// Schema-16 rows have no parser state. Other missing scopes are corrupt.
	if saved.Scope == 0 {
		if !bytes.Equal(bytes.TrimSpace(raw), []byte(`{}`)) {
			return model.Item{}, fmt.Errorf("parsed item has no stored scope")
		}
		return model.Item{}, nil
	}
	return loadParsedItemTx(ctx, tx, projectionID, saved)
}

func encodeParsedTurnTx(ctx context.Context, tx *sql.Tx, projectionID int64, turn model.Turn) ([]byte, error) {
	saved, err := storeParsedTurnTx(ctx, tx, projectionID, turn)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(saved)
	if err != nil {
		return nil, err
	}
	// SQLite views read a few turn fields with json_extract, so this private
	// model remains JSON rather than using the shared facts/continuation codec.
	return raw, nil
}

func decodeParsedTurnTx(ctx context.Context, tx *sql.Tx, projectionID int64, raw []byte) (model.Turn, error) {
	raw, err := decodeTranscriptContinuation(raw)
	if err != nil {
		return model.Turn{}, err
	}
	var saved storedParsedTurn
	if err := json.Unmarshal(raw, &saved); err != nil {
		return model.Turn{}, err
	}
	if saved.Scope == 0 {
		if !bytes.Equal(bytes.TrimSpace(raw), []byte(`{}`)) {
			return model.Turn{}, fmt.Errorf("parsed turn has no stored scope")
		}
		return model.Turn{}, nil
	}
	return loadParsedTurnTx(ctx, tx, projectionID, saved)
}

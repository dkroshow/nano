package claude

import "github.com/dkroshow/nano/internal/transcriptparse/model"

// A consumer persists emitted rows separately from the continuation checkpoint.
// Conformance assertions inspect these rows, never the reader's retained state.
// Tests that serialize parsedView restore both stores as an actual consumer
// would, while only Continuation goes back into the reader.
type parsedView struct {
	Continuation State                 `json:"continuation"`
	Items        map[string]model.Item `json:"emittedItems"`
	Turns        map[string]model.Turn `json:"emittedTurns"`
}

func parseFileView(prior parsedView, input model.Input) (parsedView, model.Batch, error) {
	state, batch, err := ParseFile(prior.Continuation, input)
	if err != nil {
		return prior, batch, err
	}
	return prior.collect(state, batch), batch, nil
}

func parseWireView(prior parsedView, input model.Input) (parsedView, model.Batch, error) {
	state, batch, err := ParseWire(prior.Continuation, input)
	if err != nil {
		return prior, batch, err
	}
	return prior.collect(state, batch), batch, nil
}

func (prior parsedView) collect(state State, batch model.Batch) parsedView {
	// Continuation size accounting is process-local and never part of a
	// checkpoint, so a view — which these tests save and restore as a consumer
	// would — drops it. A restored reader starts that accounting over.
	state.measured, state.pending, state.measurements = 0, 0, 0
	next := parsedView{Continuation: state, Items: copyMap(prior.Items), Turns: copyMap(prior.Turns)}
	for _, change := range batch.Changes {
		if change.Item != nil {
			next.Items[change.Item.ID] = *change.Item
		}
		if change.Turn != nil {
			next.Turns[change.Turn.Key] = *change.Turn
		}
	}
	return next
}

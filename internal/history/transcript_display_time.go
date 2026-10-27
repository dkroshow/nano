package history

import (
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// These are the existing history/usage display clocks. They never fill a
// missing native start/end field in the source model. Source arrival order,
// rather than clock sorting, selects a turn's first native activity.
func parsedTurnDisplayStarts(observations []model.Observation) map[string]*time.Time {
	starts := make(map[string]*time.Time)
	for _, observation := range observations {
		key := observation.Scope.TurnKey
		if key == "" || starts[key] != nil {
			continue
		}
		starts[key] = parsedDisplayTime(observation.Times, model.TimeStart)
	}
	return starts
}

func parsedTurnDisplayCompletion(turn model.Turn) *time.Time {
	if turn.Outcome == model.OutcomeUnknown || turn.Outcome == model.OutcomeRunning {
		return nil
	}
	var selected *model.CompletionFact
	rank := 0
	for i := range turn.Completion {
		fact := &turn.Completion[i]
		if fact.Outcome != turn.Outcome {
			continue
		}
		priority := 1
		if fact.Basis == model.BasisNative || fact.Basis == model.BasisDerivedExact {
			priority = 2
		}
		if priority >= rank {
			selected, rank = fact, priority
		}
	}
	if selected == nil {
		return nil
	}
	for _, ref := range selected.ObservationRefs {
		for _, observation := range turn.Observations {
			if observation.ID == ref {
				if at := parsedDisplayTime(observation.Times, model.TimeEnd); at != nil {
					return at
				}
			}
		}
	}
	return nil
}

func parsedDisplayTime(times []model.TimeObservation, meaning model.TimeMeaning) *time.Time {
	meanings := []model.TimeMeaning{meaning}
	if meaning == model.TimeEnd {
		meanings = append(meanings, model.TimeTerminal)
	}
	for _, want := range append(meanings, model.TimeEvent, model.TimeWrite) {
		for _, stamp := range times {
			if stamp.Class == model.TimeNative && stamp.Meaning == want && stamp.Value != nil {
				return cloneTime(stamp.Value)
			}
		}
	}
	return nil
}

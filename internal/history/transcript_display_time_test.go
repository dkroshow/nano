package history

import (
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestParsedTurnDisplayClocksKeepSourceOrderAndNativeAuthority(t *testing.T) {
	first, earlier, observed := time.Unix(10, 0), time.Unix(5, 0), time.Unix(99, 0)
	makeObservation := func(id, turn string, class model.TimeClass, at *time.Time) model.Observation {
		return model.Observation{ID: id, Scope: model.Scope{TurnKey: turn}, Times: []model.TimeObservation{{Class: class, Meaning: model.TimeWrite, Value: at}}}
	}
	observations := []model.Observation{
		makeObservation("receipt", "parent", model.TimeObserved, &observed),
		makeObservation("first", "parent", model.TimeNative, &first),
		makeObservation("child", "child", model.TimeNative, &earlier),
		makeObservation("regressed", "parent", model.TimeNative, &earlier),
	}
	starts := parsedTurnDisplayStarts(observations)
	if starts["parent"] == nil || !starts["parent"].Equal(first) || starts["child"] == nil || !starts["child"].Equal(earlier) {
		t.Fatal("receipt, child traffic or regressing clock changed the parent's first native activity")
	}
	turn := model.Turn{Outcome: model.OutcomeCompleted, Observations: observations, Completion: []model.CompletionFact{
		{Outcome: model.OutcomeCompleted, Basis: model.BasisNative, ObservationRefs: []string{"first"}},
		{Outcome: model.OutcomeCompleted, Basis: model.BasisDerived, ObservationRefs: []string{"regressed"}},
	}}
	if at := parsedTurnDisplayCompletion(turn); at == nil || !at.Equal(first) {
		t.Fatal("derived stop replaced the native completion clock")
	}
	turn.Completion = append(turn.Completion, model.CompletionFact{Outcome: model.OutcomeCompleted, Basis: model.BasisNative, ObservationRefs: []string{"receipt"}})
	if parsedTurnDisplayCompletion(turn) != nil {
		t.Fatal("native terminal without native time borrowed a prior stop or receipt clock")
	}
	turn.Outcome = model.OutcomeRunning
	if parsedTurnDisplayCompletion(turn) != nil {
		t.Fatal("reopened turn retained a completion display clock")
	}
}

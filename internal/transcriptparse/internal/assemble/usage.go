package assemble

import (
	"math"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// SummarizeUsage selects one grain. Exact-turn snapshots take precedence;
// otherwise distinct response snapshots contribute, with revisions replacing
// their earlier sample. Thread totals remain evidence, never contributions.
// Cumulative deltas require provider-specific reset/baseline evidence first.
func SummarizeUsage(turn model.Turn) (model.Turn, []model.Diagnostic) {
	turn.Usage, turn.UsageRefs, turn.UsageCoverage = nil, nil, nil
	turn.UsageBasis, turn.UsageIncomplete = "", false
	var selected []model.UsageObservation
	var diagnostics []model.Diagnostic
	for _, observation := range turn.UsageObservations {
		if observation.Grain == model.UsageTurn && observation.Semantics == "snapshot" && sameTurn(turn.Scope, observation.Scope) {
			selected = []model.UsageObservation{observation}
			turn.UsageBasis = "native_turn_snapshot"
		}
	}
	if len(selected) == 0 {
		type identity struct{ sample, epoch string }
		index := map[identity]int{}
		for _, observation := range turn.UsageObservations {
			if observation.Grain != model.UsageResponse || !sameTurn(turn.Scope, observation.Scope) {
				continue
			}
			if observation.Semantics != "snapshot" || observation.SampleID == "" {
				diagnostics = append(diagnostics, model.Diagnostic{Code: "usage_contribution_unresolved", Severity: "warning", Scope: observation.Scope, Message: "response usage lacks snapshot semantics or sample identity"})
				continue
			}
			key := identity{observation.SampleID, observation.ResetEpoch}
			if i, ok := index[key]; ok {
				selected[i] = observation
			} else {
				index[key] = len(selected)
				selected = append(selected, observation)
			}
		}
		if len(selected) > 0 {
			turn.UsageBasis = "distinct_response_snapshots"
		}
	}
	if len(selected) == 0 {
		return turn, diagnostics
	}
	usage := model.TokenUsage{Contract: model.UsageContractPerTurnV1}
	turn.UsageCoverage = map[string]model.CounterCoverage{}
	for _, observation := range selected {
		turn.UsageRefs = append(turn.UsageRefs, observation.ID)
		turn.UsageIncomplete = turn.UsageIncomplete || (observation.Incomplete != nil && *observation.Incomplete)
	}
	// Same nullable arithmetic as history/usage.go, with explicit coverage and
	// overflow checks. A missing counter does not become a reported zero.
	for _, field := range []struct {
		name string
		get  func(*model.TokenUsage) **int64
	}{
		{"input", func(u *model.TokenUsage) **int64 { return &u.Input }},
		{"output", func(u *model.TokenUsage) **int64 { return &u.Output }},
		{"reasoning", func(u *model.TokenUsage) **int64 { return &u.Reasoning }},
		{"cacheRead", func(u *model.TokenUsage) **int64 { return &u.CacheRead }},
		{"cacheWrite", func(u *model.TokenUsage) **int64 { return &u.CacheWrite }},
		{"cached", func(u *model.TokenUsage) **int64 { return &u.Cached }},
		{"total", func(u *model.TokenUsage) **int64 { return &u.Total }},
	} {
		coverage := model.CounterCoverage{Samples: len(selected)}
		var total int64
		valid := true
		for _, observation := range selected {
			value := *field.get(&observation.Values)
			if value == nil {
				continue
			}
			coverage.Reported++
			if *value < 0 || *value > math.MaxInt64-total {
				valid = false
				continue
			}
			total += *value
		}
		turn.UsageCoverage[field.name] = coverage
		if !valid {
			diagnostics = append(diagnostics, model.Diagnostic{Code: "invalid_usage_counter", Severity: "warning", Scope: turn.Scope, Message: "selected " + field.name + " counter is negative or overflows"})
		} else if coverage.Reported > 0 {
			*field.get(&usage) = &total
		}
	}
	last := selected[len(selected)-1].Values
	if last.ContextWindow != nil {
		value := *last.ContextWindow
		usage.ContextWindow = &value
	}
	if turn.UsageBasis == "native_turn_snapshot" {
		usage.Native = append([]byte(nil), last.Native...)
	}
	turn.Usage = &usage
	return turn, diagnostics
}

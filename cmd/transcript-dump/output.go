package main

import (
	"encoding/json"
	"io"
	"sort"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type snapshot struct {
	Provider     string              `json:"provider"`
	Versions     model.Versions      `json:"versions"`
	Items        []model.Item        `json:"items"`
	Turns        []model.Turn        `json:"turns"`
	Observations []model.Observation `json:"observations"`
	Relations    []model.Relation    `json:"relations,omitempty"`
	Diagnostics  []model.Diagnostic  `json:"diagnostics"`
	Records      []model.Record      `json:"records"`
}
type collector struct {
	options        options
	stdout, stderr *json.Encoder
	versions       model.Versions
	items          map[string]model.Item
	turns          map[string]model.Turn
	observations   []model.Observation
	relations      []model.Relation
	diagnostics    []model.Diagnostic
	records        []model.Record
}

func newCollector(o options, stdout, stderr io.Writer) *collector {
	return &collector{options: o, stdout: json.NewEncoder(stdout), stderr: json.NewEncoder(stderr), items: map[string]model.Item{}, turns: map[string]model.Turn{}}
}
func (sink *collector) add(batch model.Batch) error {
	stream := sink.options.format == "jsonl"
	if batch.Versions.Schema != "" && batch.Versions != sink.versions {
		sink.versions = batch.Versions
		if stream {
			if err := sink.stdout.Encode(map[string]any{"type": "versions", "provider": sink.options.provider, "versions": batch.Versions}); err != nil {
				return err
			}
		}
	}
	for _, change := range batch.Changes {
		if change.Item != nil {
			item := displayItem(*change.Item, sink.options.raw)
			sink.items[item.ID] = item
			change.Item = &item
		}
		if change.Turn != nil {
			turn := displayTurn(*change.Turn, sink.options.raw)
			sink.turns[turn.Key] = turn
			change.Turn = &turn
		}
		if change.Relation != nil {
			sink.relations = append(sink.relations, *change.Relation)
		}
		if stream {
			if err := sink.stdout.Encode(map[string]any{"type": "change", "change": change}); err != nil {
				return err
			}
		}
	}
	for _, obs := range batch.Observations {
		obs = displayObservation(obs, sink.options.raw)
		sink.observations = append(sink.observations, obs)
		if stream {
			if err := sink.stdout.Encode(map[string]any{"type": "observation", "observation": obs}); err != nil {
				return err
			}
		}
	}
	for _, d := range batch.Diagnostics {
		sink.diagnostics = append(sink.diagnostics, d)
		if err := sink.stderr.Encode(d); err != nil {
			return err
		}
		if stream {
			if err := sink.stdout.Encode(map[string]any{"type": "diagnostic", "diagnostic": d}); err != nil {
				return err
			}
		}
	}
	for _, record := range batch.Records {
		sink.records = append(sink.records, record)
		if stream {
			if err := sink.stdout.Encode(map[string]any{"type": "record", "record": record}); err != nil {
				return err
			}
		}
	}
	return nil
}
func (sink *collector) finish() error {
	if sink.options.format == "jsonl" {
		return nil
	}
	result := snapshot{Provider: sink.options.provider, Versions: sink.versions, Items: []model.Item{}, Turns: []model.Turn{}, Observations: []model.Observation{}, Relations: sink.relations, Diagnostics: sink.diagnostics, Records: sink.records}
	owned := map[string]bool{}
	for _, item := range sink.items {
		result.Items = append(result.Items, item)
		for _, obs := range item.Observations {
			owned[obs.ID] = true
		}
	}
	for _, turn := range sink.turns {
		result.Turns = append(result.Turns, turn)
		for _, obs := range turn.Observations {
			owned[obs.ID] = true
		}
	}
	for _, obs := range sink.observations {
		if !owned[obs.ID] {
			result.Observations = append(result.Observations, obs)
		}
	}
	sort.Slice(result.Items, func(i, j int) bool { return result.Items[i].SourceSeq < result.Items[j].SourceSeq })
	sort.Slice(result.Turns, func(i, j int) bool { return result.Turns[i].Ordinal < result.Turns[j].Ordinal })
	sink.stdout.SetIndent("", "  ")
	return sink.stdout.Encode(result)
}
func displayObservation(obs model.Observation, raw bool) model.Observation {
	if !raw && obs.Kind != model.ObservationMetadata {
		obs.Detail = nil
	}
	return obs
}
func displayItem(item model.Item, raw bool) model.Item {
	if raw {
		return item
	}
	item.Detail = nil
	item.Parts = append([]model.Part(nil), item.Parts...)
	for i := range item.Parts {
		item.Parts[i].Detail = nil
	}
	item.Observations = append([]model.Observation(nil), item.Observations...)
	for i := range item.Observations {
		item.Observations[i] = displayObservation(item.Observations[i], false)
	}
	return item
}
func displayTurn(turn model.Turn, raw bool) model.Turn {
	if raw {
		return turn
	}
	turn.Observations = append([]model.Observation(nil), turn.Observations...)
	for i := range turn.Observations {
		turn.Observations[i] = displayObservation(turn.Observations[i], false)
	}
	turn.UsageObservations = append([]model.UsageObservation(nil), turn.UsageObservations...)
	for i := range turn.UsageObservations {
		turn.UsageObservations[i].Values.Native = nil
		if buckets := turn.UsageObservations[i].ModelUsage; buckets != nil {
			filtered := make(map[string]model.TokenUsage, len(buckets))
			for name, value := range buckets {
				value.Native = nil
				filtered[name] = value
			}
			turn.UsageObservations[i].ModelUsage = filtered
		}
	}
	if turn.Usage != nil {
		usage := *turn.Usage
		usage.Native = nil
		turn.Usage = &usage
	}
	return turn
}

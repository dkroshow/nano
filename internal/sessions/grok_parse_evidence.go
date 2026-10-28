package sessions

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Operational consumers inspect this frame's native evidence only. Their
// submission ownership and lifecycle policies stay outside parser state.
//
// One frame reaches about ten consumers on its reader and session goroutines,
// and each parsed it again: at boot, Grok replays made that the largest CPU
// cost. Parsing from an empty state depends only on the frame's bytes, and
// consumers only read the batch, so recent frames' batches are kept and
// shared. The returned batch is shared: never append to or modify it.
func grokWireEvidence(line []byte) model.Batch {
	if batch, ok := grokEvidenceCache.get(line); ok {
		return batch
	}
	_, batch, err := grok.ParseWire(grok.State{}, model.Input{Bytes: line,
		Source: model.SourceRef{Label: "operational-wire", Incarnation: "frame", Direction: "recv"}})
	if err != nil {
		batch = model.Batch{}
	}
	grokEvidenceCache.put(line, batch)
	return batch
}

// grokEvidenceCache bounds its memory by frame bytes: 4 MiB of frames of at
// most 256 KiB, oldest out first. A parsed batch is larger than its frame
// (about 20 times for a small chunk), so the cache holds roughly 7 to 20 MiB
// for the process's life. At boot about thirty Grok replays interleave, and a
// frame's reader and session goroutines can be hundreds of frames apart, so
// the bound is in bytes, not entries.
var grokEvidenceCache = newGrokEvidenceStore(4 << 20)

const grokEvidenceMaxFrame = 256 << 10

type grokEvidenceStore struct {
	mu      sync.Mutex
	budget  int
	bytes   int
	batches map[string]model.Batch
	order   []string
}

func newGrokEvidenceStore(budget int) *grokEvidenceStore {
	return &grokEvidenceStore{budget: budget, batches: map[string]model.Batch{}}
}

func (c *grokEvidenceStore) get(line []byte) (model.Batch, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	batch, ok := c.batches[string(line)]
	return batch, ok
}

func (c *grokEvidenceStore) put(line []byte, batch model.Batch) {
	if len(line) > grokEvidenceMaxFrame {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := string(line)
	if _, ok := c.batches[key]; ok {
		return
	}
	c.batches[key] = batch
	c.order = append(c.order, key)
	c.bytes += len(key)
	for c.bytes > c.budget {
		oldest := c.order[0]
		c.order[0] = "" // release the evicted key now, not when order regrows
		c.order = c.order[1:]
		delete(c.batches, oldest)
		c.bytes -= len(oldest)
	}
}

func grokUpdateSource(source model.SourceRef) bool {
	return strings.HasSuffix(source.BlockPath, ".params.update") || strings.HasSuffix(source.BlockPath, ".params._meta")
}

func grokEvidencePrompt(batch model.Batch) (prompt string, atMS int64) {
	for _, observation := range batch.Observations {
		if !grokUpdateSource(observation.Source) {
			continue
		}
		if native := observation.Scope.NativeTurnID; prompt == "" && native != nil && !grok.IsSyntheticPromptID(*native) {
			prompt = *native
		}
		if atMS == 0 {
			for _, stamp := range observation.Times {
				if stamp.Class == model.TimeNative && stamp.Value != nil {
					atMS = stamp.Value.UnixMilli()
					break
				}
			}
		}
	}
	return prompt, atMS
}

func grokEvidenceTerminal(batch model.Batch) (model.Observation, string, bool) {
	for _, observation := range batch.Observations {
		if observation.Kind != model.ObservationStop || !grokUpdateSource(observation.Source) {
			continue
		}
		for _, change := range batch.Changes {
			if turn := change.Turn; turn != nil {
				for _, fact := range turn.Completion {
					for _, ref := range fact.ObservationRefs {
						if ref == observation.ID {
							return observation, fact.Reason, true
						}
					}
				}
			}
		}
		// Unscoped or evicted replay evidence does not create a new turn.
		// Its bounded native detail still supplies the operational error code.
		var detail struct {
			StopReason string `json:"stop_reason"`
		}
		_ = json.Unmarshal(observation.Detail, &detail)
		return observation, detail.StopReason, true
	}
	return model.Observation{}, "", false
}

package liveview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/threads"
)

// TruncateJSON clones a bounded JSON value or records its original byte count.
func TruncateJSON(raw json.RawMessage, cap int) json.RawMessage {
	if len(raw) <= cap {
		return append(json.RawMessage(nil), raw...)
	}
	return json.RawMessage(fmt.Sprintf(`{"truncated":true,"bytes":%d}`, len(raw)))
}

// CloneRuntime copies and bounds the wire-visible runtime fields.
func CloneRuntime(runtime Runtime) Runtime {
	out := runtime
	out.SessionStatus = threads.TruncateText(runtime.SessionStatus, 64)
	out.ThreadStatus = threads.TruncateText(runtime.ThreadStatus, 128)
	out.TurnID = threads.TruncateText(runtime.TurnID, 256)
	out.LastError = threads.TruncateText(runtime.LastError, 4<<10)
	out.TokenUsage = TruncateJSON(runtime.TokenUsage, 16<<10)
	return out
}

// RuntimeEqual compares serialized runtime facts.
func RuntimeEqual(a, b Runtime) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

// CloneInteraction copies and bounds a pending provider request.
func CloneInteraction(interaction interact.PendingInteraction) interact.PendingInteraction {
	interaction.ID = threads.TruncateText(interaction.ID, 256)
	interaction.Method = threads.TruncateText(interaction.Method, 256)
	interaction.Kind = threads.TruncateText(interaction.Kind, 128)
	interaction.ThreadID = threads.TruncateText(interaction.ThreadID, 256)
	interaction.TurnID = threads.TruncateText(interaction.TurnID, 256)
	interaction.ItemID = threads.TruncateText(interaction.ItemID, 256)
	interaction.CreatedAt = threads.TruncateText(interaction.CreatedAt, 128)
	interaction.ExpiresAt = threads.TruncateText(interaction.ExpiresAt, 128)
	interaction.Params = TruncateJSON(interaction.Params, 8<<10)
	return interaction
}

// StatusAfter applies one ordered projection update to its status view.
// StatusAfter is the status one projection update leaves, at that update's
// revision. Each queued update is applied in turn, so a turn's start and end
// that land while the stream is busy are both sent.
func StatusAfter(status Status, update Update) (Status, bool) {
	next := status
	next.Revision = update.Revision
	// Row snapshots reach subscribers as compact row records without status;
	// runtime and question changes each have their own update.
	switch {
	case update.Op == TurnUpsertOp && update.Runtime != nil:
		next.Runtime = CloneRuntime(*update.Runtime)
	case update.Op == InteractionOp && update.Interaction != nil:
		next.Interactions = slices.DeleteFunc(slices.Clone(status.Interactions), func(interaction interact.PendingInteraction) bool { return interaction.ID == update.Interaction.ID })
		next.Interactions = append(next.Interactions, CloneInteraction(*update.Interaction))
		slices.SortStableFunc(next.Interactions, func(a, b interact.PendingInteraction) int { return strings.Compare(a.CreatedAt, b.CreatedAt) })
	case update.Op == InteractionRemoveOp:
		next.Interactions = slices.DeleteFunc(slices.Clone(status.Interactions), func(interaction interact.PendingInteraction) bool { return interaction.ID == update.RequestID })
	default:
		return next, false
	}
	return next, !RuntimeEqual(status.Runtime, next.Runtime) || !reflect.DeepEqual(status.Interactions, next.Interactions)
}

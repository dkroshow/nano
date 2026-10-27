package codex

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Live command output is presentation state. Keep a UTF-8-safe 64 KiB
// head/tail preview without claiming that another source retains a complete
// recoverable copy.
const (
	liveCommandOutputBytes     = 64 << 10
	liveCommandObservationEdge = 4
	liveCommandOutputMarker    = "\n… [live command output truncated] …\n"
)

func liveWireSource(source model.SourceRef) bool { return source.Kind == "wire" }

func appendLiveCommandOutput(part model.Part, delta string) model.Part {
	if part.Availability == model.AvailableTruncated && part.OriginalBytes != nil {
		if head, tail, ok := strings.Cut(part.Text, liveCommandOutputMarker); ok {
			original := *part.OriginalBytes + int64(len(delta))
			part.Text = head + liveCommandOutputMarker + utf8Suffix(tail+delta, liveCommandTailBytes())
			part.OriginalBytes = &original
			return part
		}
	}
	part.Text += delta
	return boundLiveCommandOutput(part)
}

func boundLiveCommandOutput(part model.Part) model.Part {
	if part.Availability == model.AvailableTruncated && part.OriginalBytes != nil &&
		strings.Contains(part.Text, liveCommandOutputMarker) {
		return part
	}
	if len(part.Text) <= liveCommandOutputBytes {
		return part
	}
	original := int64(len(part.Text))
	part.Text = utf8Prefix(part.Text, liveCommandHeadBytes()) + liveCommandOutputMarker + utf8Suffix(part.Text, liveCommandTailBytes())
	part.Availability = model.AvailableTruncated
	part.OriginalBytes = &original
	return part
}

func liveCommandHeadBytes() int {
	return (liveCommandOutputBytes - len(liveCommandOutputMarker)) / 2
}

func liveCommandTailBytes() int {
	return liveCommandOutputBytes - len(liveCommandOutputMarker) - liveCommandHeadBytes()
}

func utf8Prefix(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}

func utf8Suffix(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	start := len(value) - limit
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:]
}

func boundLiveCommandItem(item model.Item) model.Item {
	if item.Type != model.ItemCommand {
		return item
	}
	for index := range item.Parts {
		if item.Parts[index].Type != "tool_output" {
			continue
		}
		item.Parts[index] = boundLiveCommandOutput(item.Parts[index])
		part := item.Parts[index]
		if part.Availability == model.AvailableTruncated && part.OriginalBytes != nil {
			item.ContentTruncated = true
			item.OriginalBytes = *part.OriginalBytes
			item.Detail = boundLiveCommandDetail(item.Detail, part.Text, *part.OriginalBytes)
		}
		break
	}
	if len(item.Observations) > 2*liveCommandObservationEdge {
		bounded := make([]model.Observation, 0, 2*liveCommandObservationEdge)
		bounded = append(bounded, item.Observations[:liveCommandObservationEdge]...)
		bounded = append(bounded, item.Observations[len(item.Observations)-liveCommandObservationEdge:]...)
		item.Observations = bounded
	}
	return item
}

func boundLiveCommandDetail(detail json.RawMessage, output string, original int64) json.RawMessage {
	var payload map[string]any
	if json.Unmarshal(detail, &payload) != nil || payload == nil {
		return detail
	}
	if _, ok := payload["aggregatedOutput"]; ok {
		payload["aggregatedOutput"] = output
	}
	if _, ok := payload["aggregated_output"]; ok {
		payload["aggregated_output"] = output
	}
	payload["nanoLiveOutputTruncated"] = true
	payload["nanoLiveOutputOriginalBytes"] = original
	bounded, err := json.Marshal(payload)
	if err != nil {
		return detail
	}
	return bounded
}

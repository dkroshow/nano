package claude

import (
	"encoding/json"
	"time"

	"github.com/dkroshow/nano/internal/wire"
)

// TailSeq requests no historical lines from the deployed shim protocol.
const TailSeq int64 = 1<<63 - 1

// EventKind classifies a shim turn event.
type EventKind string

const (
	EventAccepted EventKind = "accepted"
	EventItem     EventKind = "item"
	EventResult   EventKind = "result"
)

// Event is the typed transport-to-adapter seam for provider-owned turn identity.
type Event struct {
	Kind           EventKind
	ProviderTurnID string
	Seq            int64
	At             time.Time
	Direction      wire.Direction
	Line           []byte
}

// Line is one typed session/line notification from the shim spool or live stream.
type Line struct {
	Seq            int64
	At             time.Time
	Direction      wire.Direction
	Bytes          []byte
	ProviderTurnID string
}

// DecodeLine decodes one shim session/line notification.
func DecodeLine(params json.RawMessage) (Line, bool) {
	var payload struct {
		Seq    int64  `json:"seq"`
		TS     string `json:"ts"`
		Dir    string `json:"dir"`
		Line   string `json:"line"`
		TurnID string `json:"turnId"`
	}
	if json.Unmarshal(params, &payload) != nil {
		return Line{}, false
	}
	direction := wire.Recv
	if payload.Dir == string(wire.Send) {
		direction = wire.Send
	}
	var at time.Time
	if payload.TS != "" {
		at, _ = time.Parse(time.RFC3339Nano, payload.TS)
	}
	return Line{
		Seq: payload.Seq, At: at, Direction: direction,
		Bytes: []byte(payload.Line), ProviderTurnID: payload.TurnID,
	}, true
}

// EventKindFor classifies one shim line without projecting session state.
func EventKindFor(direction wire.Direction, line []byte) (EventKind, bool) {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &probe) != nil {
		return "", false
	}
	if direction == wire.Send {
		return EventAccepted, probe.Type == "user"
	}
	if probe.Type == "result" {
		return EventResult, true
	}
	return EventItem, true
}

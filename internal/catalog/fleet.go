package catalog

import (
	"context"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/lifecycle"
)

const (
	FleetWorking         = "working"
	FleetNeedsYou        = "needs_you"
	FleetIdle            = "idle"
	CourtUserOwes        = "user_owes"
	CourtAgentOwes       = "agent_owes"
	FleetUserMessageType = string(history.ItemUserMessage)
)

// WorkingSilenceBound is how long a thread Nano does not hold may show an
// unanswered prompt as working. Its saved file is the only evidence, and the
// file moves with every tool call, so a quiet file means no turn is running.
const WorkingSilenceBound = 30 * time.Minute

type FleetState struct {
	ThreadID    string `json:"threadId"`
	Label       string `json:"label"`
	Court       string `json:"court"`
	AgeSeconds  int64  `json:"ageSeconds"`
	BallInCourt string `json:"ballInCourt"`
	Watched     bool   `json:"watched,omitempty"`
}

var askPhrases = []string{
	"let me know", "want me to", "should i ", "shall i ", "do you want",
	"would you like", "your call", "say the word", "if you want me",
	"ready for your instructions", "awaiting your", "need your decision",
	"which option", "let me know if you want",
}

func EndsWithQuestion(tail string) bool {
	tail = strings.TrimRight(tail, " \t\r\n`\"'*).]}>")
	return strings.HasSuffix(tail, "?")
}

func AwaitsUser(head, tail string) bool {
	if EndsWithQuestion(tail) {
		return true
	}
	text := strings.ToLower(head + "\n" + tail)
	for _, phrase := range askPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func DeriveFleetState(threadID, lastItemType, head, tail string, ageSeconds int64) *FleetState {
	state := &FleetState{ThreadID: threadID, AgeSeconds: ageSeconds, Watched: true}
	switch lastItemType {
	case "":
		state.Label = FleetIdle
		state.BallInCourt = "No messages yet — nothing has been said in this thread."
	case FleetUserMessageType:
		state.Court = CourtAgentOwes
		if time.Duration(ageSeconds)*time.Second >= WorkingSilenceBound {
			state.Label = FleetIdle
			state.BallInCourt = "Idle — the last prompt has no reply and no activity for 30 minutes."
		} else {
			state.Label = FleetWorking
			state.BallInCourt = "Agent is working — turn in flight."
		}
	default:
		state.Court = CourtUserOwes
		if AwaitsUser(head, tail) {
			state.Label = FleetNeedsYou
			state.BallInCourt = "Your move — agent is waiting on you."
		} else {
			state.Label = FleetIdle
			state.BallInCourt = "Idle — agent finished; still the current thread on this topic."
		}
	}
	return state
}

// FleetLabelFromLifecycle is the turn state of a thread Nano holds. The
// session publishes it to the pane, the session list and the runtime overlay;
// catalog rows copy it through LiveFleetState.
func FleetLabelFromLifecycle(state lifecycle.State) string {
	if state.Connection != lifecycle.Ready {
		return FleetIdle
	}
	switch state.Work {
	case lifecycle.Accepted, lifecycle.Working:
		return FleetWorking
	case lifecycle.WaitingOnYou:
		return FleetNeedsYou
	default:
		return FleetIdle
	}
}

// LiveFleetState explains a lifecycle label on a catalog row.
func LiveFleetState(threadID, label string) *FleetState {
	out := &FleetState{ThreadID: threadID, Label: FleetIdle, BallInCourt: "Idle — no hosted turn is active."}
	switch label {
	case FleetNeedsYou:
		out.Label, out.Court = FleetNeedsYou, CourtUserOwes
		out.BallInCourt = "Your move — agent is waiting on you."
	case FleetWorking:
		out.Label, out.Court = FleetWorking, CourtAgentOwes
		out.BallInCourt = "Agent is working — turn in flight."
	}
	return out
}

type FleetCourtMessage struct {
	ItemType     string
	Head         string
	Tail         string
	LastActivity time.Time
	Interrupted  bool
}

type FleetCourtSource interface {
	FleetCourt(context.Context, []string) (map[string]FleetCourtMessage, error)
}

func (message FleetCourtMessage) State(threadID string, now time.Time) *FleetState {
	if message.Interrupted {
		return &FleetState{
			ThreadID: threadID, Label: FleetIdle, Court: CourtUserOwes,
			AgeSeconds: message.AgeSeconds(now), BallInCourt: "Idle — turn was interrupted.", Watched: true,
		}
	}
	return DeriveFleetState(threadID, message.ItemType, message.Head, message.Tail, message.AgeSeconds(now))
}

func (message FleetCourtMessage) AgeSeconds(now time.Time) int64 {
	if message.LastActivity.IsZero() {
		return 0
	}
	age := int64(now.Sub(message.LastActivity) / time.Second)
	if age < 0 {
		return 0
	}
	return age
}

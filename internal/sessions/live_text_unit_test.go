package sessions

import (
	"testing"

	"github.com/dkroshow/nano/internal/core/prompts"
)

// One drain applies producer events in arrival order: a reply that began
// before the next prompt was delivered shows first, and its newer full text
// replaces its pending update without moving it.
func TestLiveTextKeepsArrivalOrderInOneDrain(t *testing.T) {
	var in liveTextInbox
	in.addReply(liveReplyUpdate{key: "msg_1:0", text: "Ra", turn: "t1", started: true})
	next := prompts.OutboxEntry{ActionID: "next", Sequence: 2, PresentationText: "next", State: prompts.Delivering}
	in.events = append(in.events, liveTextEvent{outbox: &next})
	in.addReply(liveReplyUpdate{key: "msg_1:0", text: "Rain falls.", turn: "t1", done: true, started: true})
	l := NewLiveTextLoop(LiveTextSources{}, nil)
	l.reset()
	l.process(in)
	live := l.visible()
	if len(live) != 2 || live[0].ID != "reply:msg_1:0" || live[0].Text != "Rain falls." || live[0].State != "done" || live[1].ID != "prompt:next" {
		t.Fatalf("live = %+v, want the reply's latest text, then the prompt", live)
	}
}

// An expired prompt (an old delivered prompt exact identity never settled) is
// no longer shown. A long-lived thread's outbox held 23, and file-only showed
// every one as live text below the saved rows. The loop drops an expired
// prompt as it drops a refused one, whether a fresh loop seeds it or a shown
// prompt expires later.
func TestLiveTextShowsNoExpiredPrompt(t *testing.T) {
	old := prompts.OutboxEntry{ActionID: "agent-old", Sequence: 1, PresentationText: "an old delivered prompt", State: prompts.Expired}
	pending := prompts.OutboxEntry{ActionID: "human-now", Sequence: 2, PresentationText: "a prompt being delivered", State: prompts.Accepted}
	var in liveTextInbox
	in.events = append(in.events, liveTextEvent{outbox: &old}, liveTextEvent{outbox: &pending})
	l := NewLiveTextLoop(LiveTextSources{}, nil)
	l.reset()
	l.process(in)
	if live := l.visible(); len(live) != 1 || live[0].ID != "prompt:human-now" {
		t.Fatalf("live = %+v, want only the prompt being delivered", live)
	}
	expired := pending
	expired.State = prompts.Expired
	l.process(liveTextInbox{events: []liveTextEvent{{outbox: &expired}}})
	if live := l.visible(); len(live) != 0 {
		t.Fatalf("live = %+v after the prompt expired, want none", live)
	}
}

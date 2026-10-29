package sidebar

import (
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/catalog"
)

func TestDeriveWatchedFleetState(t *testing.T) {
	fresh := int64(5 * 60)
	old := int64((365 * 24 * time.Hour) / time.Second)
	for _, tc := range []struct {
		name     string
		itemType string
		head     string
		tail     string
		ageSec   int64
		label    string
		court    string
	}{
		{"user spoke last and recently: the agent is working",
			"user_message", "please investigate", "please investigate", fresh, fleetWorking, fleetCourtAgentOwes},
		{"user spoke last long ago: the agent still owes, but nothing is running",
			"user_message", "please investigate", "please investigate", old, fleetIdle, fleetCourtAgentOwes},
		{"a recent answer ending in a question needs you",
			"agent_message", "Deployed.", "Want me to roll it back?", fresh, fleetNeedsYou, fleetCourtUserOwes},
		{"a recent answer with an ask phrase needs you",
			"agent_message", "Let me know if you want the second pass.", "All three checks pass.", fresh, fleetNeedsYou, fleetCourtUserOwes},
		{"a recent statement-style answer stays idle",
			"agent_message", "Deployed and verified.", "Deployed and verified.", fresh, fleetIdle, fleetCourtUserOwes},
		{"an old open question still needs you",
			"agent_message", "Deployed.", "Want me to roll it back?", old, fleetNeedsYou, fleetCourtUserOwes},
		{"nothing said at all is idle", "", "", "", fresh, fleetIdle, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := catalog.DeriveFleetState("nth_one", tc.itemType, tc.head, tc.tail, tc.ageSec)
			if state.Label != tc.label || state.Court != tc.court {
				t.Fatalf("state = %#v", state)
			}
			if state.ThreadID != "nth_one" || state.AgeSeconds != tc.ageSec || state.BallInCourt == "" || !state.Watched {
				t.Fatalf("state carried no usable detail: %#v", state)
			}
		})
	}
}

// A thread Nano does not hold has only its saved file as evidence. An
// unanswered prompt is working while the file keeps moving; one silent for the
// bound is an abandoned turn, and calling it working is the ghost rows the
// sidebar counted after their agents had stopped (weeks-old Claude threads).
func TestDeriveFleetStateWorkingIsBoundedBySilence(t *testing.T) {
	for _, tc := range []struct {
		silent time.Duration
		label  string
	}{
		{time.Minute, fleetWorking},
		{catalog.WorkingSilenceBound - time.Second, fleetWorking},
		{catalog.WorkingSilenceBound, fleetIdle},
		{19 * 24 * time.Hour, fleetIdle},
	} {
		state := catalog.DeriveFleetState("nth_one", "user_message", "run the migration", "run the migration", int64(tc.silent/time.Second))
		if state.Label != tc.label || state.Court != fleetCourtAgentOwes {
			t.Fatalf("silent %s => %#v, want %s owed by the agent", tc.silent, state, tc.label)
		}
	}
}

func TestAwaitsUserIsPrecisionBiased(t *testing.T) {
	for _, tail := range []string{
		"Should I proceed?", "which option do you prefer?", "Deployed — want me to verify?",
		"ready for your instructions", "Your call.", "Is that right?**", "done?`",
	} {
		if !catalog.AwaitsUser("", tail) {
			t.Fatalf("open loop missed: %q", tail)
		}
	}
	for _, tail := range []string{
		"Deployed and verified.", "The migration ran clean.", "I asked myself whether it mattered.",
		"", "Closed out: three fixes landed.",
	} {
		if catalog.AwaitsUser("", tail) {
			t.Fatalf("false open loop: %q", tail)
		}
	}
	// The ask can be anywhere in the bounded head/tail windows, not just at the end.
	if !catalog.AwaitsUser("Let me know which branch to target.", "Everything else is done.") {
		t.Fatal("ask phrase in the head was missed")
	}
}

func TestEndsWithQuestionStripsTrailingMarkup(t *testing.T) {
	for _, tail := range []string{"ok?", "ok?  \n", "ok?`", `ok?"`, "ok?*)", "ok?]}>"} {
		if !catalog.EndsWithQuestion(tail) {
			t.Fatalf("question missed: %q", tail)
		}
	}
	if catalog.EndsWithQuestion("ok? Then I fixed it.") {
		t.Fatal("a mid-text question is not a trailing one")
	}
}

func TestFleetCourtMessageAge(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	working := fleetCourtMessage{ItemType: "user_message", LastActivity: now.Add(-10 * time.Minute)}
	if got := working.AgeSeconds(now); got != 600 {
		t.Fatalf("age = %d", got)
	}
	// A clock skewed into the future reads as fresh, never as a negative age.
	if got := (fleetCourtMessage{LastActivity: now.Add(time.Hour)}).AgeSeconds(now); got != 0 {
		t.Fatalf("future clock age = %d", got)
	}
}

func TestInterruptedFleetTurnIsImmediatelyIdle(t *testing.T) {
	now := time.Date(2026, 8, 4, 22, 20, 0, 0, time.UTC)
	state := (fleetCourtMessage{
		ItemType: "user_message", LastActivity: now.Add(-time.Second), Interrupted: true,
	}).State("nth_worker", now)
	if state.Label != fleetIdle || state.Court != fleetCourtUserOwes || state.BallInCourt != "Idle — turn was interrupted." {
		t.Fatalf("interrupted state = %#v", state)
	}
}

package sessions

import (
	"testing"
	"time"
)

// The app sibling checks persistence across a store reload. This boundary
// catches the same timestamp ordering failure in the live attachment.
func TestHumanPromptSessionAdvancesAcrossTrimmedFractions(t *testing.T) {
	sess := New(Config{})
	earlier := time.Date(2026, 10, 15, 12, 0, 4, 237400000, time.UTC)
	later := earlier.Add(time.Microsecond)
	if earlier.Format(time.RFC3339Nano) <= later.Format(time.RFC3339Nano) {
		t.Fatal("fixture must reverse chronological order as text")
	}
	for _, at := range []time.Time{earlier, later, earlier} {
		sess.noteHumanPrompt(at)
	}
	if got := sess.AvailabilityState().LastHumanPromptAt; !got.Equal(later) {
		t.Fatalf("last human prompt = %s, want %s", got, later)
	}
}

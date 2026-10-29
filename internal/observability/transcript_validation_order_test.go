package observability

import "testing"

func TestTranscriptValidationHistoryOrdersUnequalTimestampPrecision(t *testing.T) {
	store := NewValidationStore(10)
	// RFC3339Nano omits trailing zeroes. These are real server receipt times
	// from a failing suite run; their text order is the reverse of time order.
	store.history["pane"] = []storedTranscriptValidation{
		storedValidation(transcriptValidationSnapshot{SnapshotID: "earlier", ReceivedAt: "2026-09-09T11:57:26.3395Z"}),
		storedValidation(transcriptValidationSnapshot{SnapshotID: "later", ReceivedAt: "2026-09-09T11:57:26.339564Z"}),
	}
	_, history := store.list(1, "", "", "", "", "")
	if len(history) != 1 || history[0].SnapshotID != "later" {
		t.Fatalf("newest diagnostic receipt = %+v", history)
	}
}

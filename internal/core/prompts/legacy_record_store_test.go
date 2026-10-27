package prompts

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTurnRecordsPersistAtomicallyWithoutChangingOtherFamilies(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "threads-copy.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(queuedTurnSchema + ";" + sendAttemptSchema); err != nil {
		t.Fatal(err)
	}
	queue := []QueuedTurn{{ClientActionID: "second-by-time-first-in-file", Provider: "codex", HostedThreadID: "hosted", Text: "kept", CreatedAt: "z", State: "uncertain", ProviderTurnID: "exact-turn"}, {ClientActionID: "first-by-time-last-in-file", Provider: "claude", NativeThreadID: "provisional", Text: "kept", CreatedAt: "a", State: "parked"}}
	attempts := []SendAttempt{{ClientActionID: "action", Provider: "codex", NativeThreadID: "native", Confirmed: true, ConfirmedAt: "at", ProviderTurnID: "exact-turn", IdentityConflict: "conflict"}}
	if err := SaveQueuedTurns(ctx, db, queue); err != nil {
		t.Fatal(err)
	}
	if err := SaveSendAttempts(ctx, db, attempts); err != nil {
		t.Fatal(err)
	}
	// A late insertion error must roll back the earlier deletion and insertion.
	failedQueue := append([]QueuedTurn(nil), queue...)
	failedQueue[0].Text = "must not commit"
	if err := SaveQueuedTurns(ctx, db, append(failedQueue, failedQueue[0])); err == nil {
		t.Fatal("duplicate queue write succeeded")
	}
	failedAttempts := append([]SendAttempt(nil), attempts...)
	failedAttempts[0].IdentityConflict = "must not replace prior evidence"
	if err := SaveSendAttempts(ctx, db, append(failedAttempts, failedAttempts[0])); err == nil {
		t.Fatal("duplicate attempt write succeeded")
	}
	if got, err := LoadQueuedTurns(ctx, db); err != nil || !reflect.DeepEqual(got, queue) {
		t.Fatalf("queue lost text/order/binding: %+v %v", got, err)
	}
	if got, err := LoadSendAttempts(ctx, db); err != nil || !reflect.DeepEqual(got, attempts) {
		t.Fatalf("journal lost exact evidence: %+v %v", got, err)
	}
	if err := SaveQueuedTurns(ctx, db, queue[:1]); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadSendAttempts(ctx, db); err != nil || !reflect.DeepEqual(got, attempts) {
		t.Fatalf("queue write changed journal: %+v %v", got, err)
	}
	if err := SaveSendAttempts(ctx, db, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadQueuedTurns(ctx, db); err != nil || !reflect.DeepEqual(got, queue[:1]) {
		t.Fatalf("journal write changed queue: %+v %v", got, err)
	}
}

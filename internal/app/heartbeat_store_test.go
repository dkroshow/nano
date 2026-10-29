package app

import (
	"path/filepath"
	"testing"
	"time"

	featureheartbeat "github.com/dkroshow/nano/internal/features/heartbeat"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/threads"
)

func openTestHeartbeatStore(t *testing.T) *featureheartbeat.Store {
	t.Helper()
	store, err := featureheartbeat.OpenStore(filepath.Join(t.TempDir(), "heartbeat.sqlite"), threads.Backup)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testHeartbeatInput(target string) featureheartbeat.AttachmentInput {
	return featureheartbeat.AttachmentInput{
		TargetHostedThreadID: target,
		CadenceMinutes:       1,
		Prompt:               "Continue the assigned work.",
		Authority:            "Operator approved",
		Actor:                featureheartbeat.Actor{Kind: "operator", Name: "Operator"},
	}
}

func TestHeartbeatPauseResumeRequireReasonsAndAudit(t *testing.T) {
	store := openTestHeartbeatStore(t)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	rec, err := store.CreateAttachment(testHeartbeatInput("nht_target"), now)
	if err != nil {
		t.Fatal(err)
	}
	actor := featureheartbeat.Actor{Kind: "hosted-thread", HostedThreadID: "nht_manager"}
	if _, err := store.PauseAttachment(rec.ID, "", actor, now); err == nil {
		t.Fatal("pause without reason succeeded")
	}
	paused, err := store.PauseAttachment(rec.ID, "waiting for a review", actor, now.Add(time.Minute))
	if err != nil || paused.State != featureheartbeat.StatePaused || paused.PauseReason != "waiting for a review" {
		t.Fatalf("pause = %#v, %v", paused, err)
	}
	if _, err := store.ResumeAttachment(rec.ID, "", actor, now); err == nil {
		t.Fatal("resume without reason succeeded")
	}
	if _, err := store.DB.Exec(`UPDATE heartbeat_attachments SET consecutive_wakes=14 WHERE id=?`, rec.ID); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.ResumeAttachment(rec.ID, "review arrived", actor, now.Add(2*time.Minute))
	if err != nil || resumed.State != featureheartbeat.StateActive || resumed.ConsecutiveWakes != 0 || resumed.LastResumeAt == "" {
		t.Fatalf("resume = %#v, %v", resumed, err)
	}
	var details string
	if err := store.DB.QueryRow(`SELECT group_concat(detail,'|') FROM heartbeat_attachment_events WHERE attachment_id=?`, rec.ID).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if details != "|waiting for a review|review arrived" {
		t.Fatalf("audit details = %q", details)
	}
}

func TestHeartbeatBackoffAndHumanReset(t *testing.T) {
	store := openTestHeartbeatStore(t)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	rec, err := store.CreateAttachment(testHeartbeatInput("nht_target"), now)
	if err != nil {
		t.Fatal(err)
	}
	for wakes, want := range map[int]int{0: 1, 9: 1, 10: 2, 19: 2, 20: 5, 30: 5} {
		if _, err := store.DB.Exec(`UPDATE heartbeat_attachments SET consecutive_wakes=? WHERE id=?`, wakes, rec.ID); err != nil {
			t.Fatal(err)
		}
		loaded, err := store.Attachment(rec.ID)
		if err != nil || loaded.EffectiveCadenceMinutes != want {
			t.Fatalf("wakes %d: attachment=%#v err=%v", wakes, loaded, err)
		}
	}
	if err := store.ObserveHumanMessage(rec.ID, now.Add(time.Minute).Format(time.RFC3339Nano), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Attachment(rec.ID)
	if err != nil || loaded.ConsecutiveWakes != 0 || loaded.LastHumanMessageAt == "" || loaded.EffectiveCadenceMinutes != 1 {
		t.Fatalf("human reset = %#v, %v", loaded, err)
	}
}

func TestHeartbeatDueUsesCurrentWallClockMinute(t *testing.T) {
	store := openTestHeartbeatStore(t)
	input := testHeartbeatInput("nht_target")
	input.CadenceMinutes, input.OffsetMinutes = 5, 2
	rec, err := store.CreateAttachment(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for minute, want := range map[int64]bool{101: false, 102: true, 103: false, 107: true} {
		due, err := store.DueAttachments(minute)
		if err != nil {
			t.Fatal(err)
		}
		got := len(due) == 1 && due[0].ID == rec.ID
		if got != want {
			t.Fatalf("minute %d due=%v, want %v", minute, due, want)
		}
	}
}

func TestHeartbeatUsageCapAndDecisionRetention(t *testing.T) {
	store := openTestHeartbeatStore(t)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	rec, err := store.CreateAttachment(testHeartbeatInput("nht_target"), now)
	if err != nil {
		t.Fatal(err)
	}
	old := now.AddDate(0, 0, -31)
	for minute, outcome := range map[int64]string{1: featureheartbeat.OutcomeSkipped, 2: featureheartbeat.OutcomeFailed, 3: featureheartbeat.OutcomeDelivered} {
		id, claimed, err := store.ClaimWakeDecision(rec.ID, minute, old)
		if err != nil || !claimed {
			t.Fatalf("claim %d: %v", minute, err)
		}
		op := ""
		if outcome == featureheartbeat.OutcomeDelivered {
			op = "aop_wake"
		}
		if err := store.SettleWakeDecision(id, featureheartbeat.GateDelivery, "available", nil, outcome, op, "", old); err != nil {
			t.Fatal(err)
		}
	}
	input, cached, output := int64(13), int64(5), int64(8)
	if err := store.RecordWakeUsage("aop_wake", &interact.TokenUsage{InputTokens: &input, CachedInputTokens: &cached, OutputTokens: &output}); err != nil {
		t.Fatal(err)
	}
	id, claimed, err := store.ClaimWakeDecision(rec.ID, 4, old)
	if err != nil || !claimed {
		t.Fatalf("claim unavailable usage wake: %v", err)
	}
	if err := store.SettleWakeDecision(id, featureheartbeat.GateDelivery, "available", nil, featureheartbeat.OutcomeDelivered, "aop_no_usage", "", old); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordWakeUsage("aop_no_usage", nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Attachment(rec.ID)
	if err != nil || loaded.Usage.WakeCount != 2 || loaded.Usage.UnavailableWakes != 1 || loaded.Usage.InputTokens != 13 || loaded.Usage.CachedInputTokens != 5 || loaded.Usage.OutputTokens != 8 {
		t.Fatalf("usage = %#v, %v", loaded.Usage, err)
	}
	if ok, err := store.HasWakeCapacity(old.Add(-time.Minute), 2); err != nil || ok {
		t.Fatalf("cap = %v, %v", ok, err)
	}
	deleted, err := store.PruneDecisionLog(now.AddDate(0, 0, -30))
	if err != nil || deleted != 1 {
		t.Fatalf("prune = %d, %v", deleted, err)
	}
	rows, err := store.ListWakeLog(rec.ID, 10)
	if err != nil || len(rows) != 3 {
		t.Fatalf("retained = %#v, %v", rows, err)
	}
}

// Catches: a newer human message failing to reset the backoff because the
// stored older time's fraction ends in zeros, which RFC3339Nano trims, so as
// text ".2374Z" sorts after the later ".237401Z". Boundary: the heartbeat store.
func TestHeartbeatHumanMessageMovesForwardAcrossTrimmedFractions(t *testing.T) {
	store := openTestHeartbeatStore(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	rec, err := store.CreateAttachment(testHeartbeatInput("nht_target"), now)
	if err != nil {
		t.Fatal(err)
	}
	earlier := time.Date(2026, 10, 15, 12, 0, 4, 237400000, time.UTC)
	later := earlier.Add(time.Microsecond)
	for _, step := range []struct {
		observed      time.Time
		wantLast      time.Time
		wantWakesKept bool
	}{{earlier, earlier, false}, {later, later, false}, {earlier, later, true}} {
		if _, err := store.DB.Exec(`UPDATE heartbeat_attachments SET consecutive_wakes=3 WHERE id=?`, rec.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.ObserveHumanMessage(rec.ID, step.observed.Format(time.RFC3339Nano), now); err != nil {
			t.Fatal(err)
		}
		loaded, err := store.Attachment(rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.LastHumanMessageAt != step.wantLast.Format(time.RFC3339Nano) || (loaded.ConsecutiveWakes == 3) != step.wantWakesKept {
			t.Fatalf("after observing %s: last=%q wakes=%d", step.observed.Format(time.RFC3339Nano), loaded.LastHumanMessageAt, loaded.ConsecutiveWakes)
		}
	}
}

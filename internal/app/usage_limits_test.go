package app

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageOffSwitchOpensNoStore(t *testing.T) {
	dir := t.TempDir()
	store, path, err := openUsageLimits(false, filepath.Join(dir, "history.sqlite"))
	if err != nil || store != nil || path != "" {
		t.Fatalf("disabled usage = store=%v path=%q err=%v", store, path, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "usage-limits.sqlite")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled usage created a store: %v", err)
	}
}

func TestNextAlignedTickLandsOnWallClockBoundaries(t *testing.T) {
	// A fixed whole-hour zone (UTC-7, matching PDT) for the common case.
	pdt := time.FixedZone("PDT", -7*3600)
	at := func(h, m, s int) time.Time { return time.Date(2026, 8, 6, h, m, s, 0, pdt) }
	atMs := func(h, m, s, ms int) time.Time {
		return time.Date(2026, 8, 6, h, m, s, ms*int(time.Millisecond), pdt)
	}

	cases := []struct {
		name     string
		now      time.Time
		interval time.Duration
		want     time.Time
	}{
		{"mid-interval rounds up to the next multiple", at(16, 27, 13), 5 * time.Minute, at(16, 30, 0)},
		{"just past a boundary waits a full interval", at(16, 30, 2), 5 * time.Minute, at(16, 35, 0)},
		{"last slot of the hour rolls over", at(16, 57, 30), 5 * time.Minute, at(17, 0, 0)},
		{"one-minute interval aligns to the minute", at(16, 27, 13), time.Minute, at(16, 28, 0)},
		{"fifteen-minute interval uses quarter hours", at(16, 27, 13), 15 * time.Minute, at(16, 30, 0)},
		// Landing within a second of a boundary would poll twice in a row
		// right after the startup read, so the following slot is taken.
		{"a boundary under a second away is skipped", atMs(16, 29, 59, 500), 5 * time.Minute, at(16, 35, 0)},
		{"a boundary a full second away is kept", at(16, 29, 59), 5 * time.Minute, at(16, 30, 0)},
	}
	for _, tc := range cases {
		got := nextAlignedTick(tc.now, tc.interval)
		if !got.Equal(tc.want) {
			t.Errorf("%s: nextAlignedTick(%s, %s) = %s, want %s",
				tc.name, tc.now.Format("15:04:05"), tc.interval,
				got.Format("15:04:05"), tc.want.Format("15:04:05"))
		}
	}
}

func TestNextAlignedTickFollowsTheLocalClockInHalfHourZones(t *testing.T) {
	// India is UTC+05:30. Truncating in absolute time would put the boundary
	// on :02:30, :07:30 … instead of on the clock's own five-minute marks.
	ist := time.FixedZone("IST", 5*3600+30*60)
	now := time.Date(2026, 8, 6, 16, 27, 13, 0, ist)
	got := nextAlignedTick(now, 5*time.Minute)
	want := time.Date(2026, 8, 6, 16, 30, 0, 0, ist)
	if !got.Equal(want) {
		t.Errorf("got %s, want %s", got.Format("15:04:05"), want.Format("15:04:05"))
	}
	if got.Second() != 0 || got.Minute()%5 != 0 {
		t.Errorf("tick %s is not on a wall-clock five-minute mark", got.Format("15:04:05"))
	}
}

func TestNextAlignedTickIsAlwaysInTheFuture(t *testing.T) {
	// Whatever the offset or interval, the poller must never be handed a
	// negative delay — that would spin.
	for _, offsetMin := range []int{0, -420, 330, 345, -180} {
		zone := time.FixedZone("Z", offsetMin*60)
		for _, iv := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour} {
			for sec := 0; sec < 3600; sec += 37 {
				now := time.Date(2026, 8, 6, 0, 0, 0, 0, zone).Add(time.Duration(sec) * time.Second)
				next := nextAlignedTick(now, iv)
				if !next.After(now) {
					t.Fatalf("offset %d, interval %s, now %s: next %s is not in the future",
						offsetMin, iv, now, next)
				}
				if d := next.Sub(now); d > 2*iv {
					t.Fatalf("offset %d, interval %s, now %s: waited %s, more than two intervals",
						offsetMin, iv, now, d)
				}
			}
		}
	}
}

func TestObserveCarriesReadingsThroughANotice(t *testing.T) {
	store, err := newUsageLimitsStore(filepath.Join(t.TempDir(), "usage-limits.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	store.observe(&usageProviderSnapshot{
		Provider:   "claude",
		ObservedAt: time.Now().UTC(),
		Buckets:    []usageBucket{{Key: "weekly", Label: "week", UsedPercent: 78}},
	})
	store.observe((&usageProviderSnapshot{Provider: "claude"}).
		Wait("waiting for a Claude session to refresh the token"))

	got := store.snapshot()[0]
	if got.Error != "" {
		t.Errorf("a notice must not populate Error: %q", got.Error)
	}
	if got.Notice == "" {
		t.Error("the notice was dropped")
	}
	// The bars stay: a routine rollover is not a reason to blank or grey them.
	if len(got.Buckets) != 1 || got.Buckets[0].UsedPercent != 78 {
		t.Errorf("last good reading was lost: %+v", got.Buckets)
	}

	// A later success clears the notice.
	store.observe(&usageProviderSnapshot{
		Provider:   "claude",
		ObservedAt: time.Now().UTC(),
		Buckets:    []usageBucket{{Key: "weekly", Label: "week", UsedPercent: 79}},
	})
	if n := store.snapshot()[0].Notice; n != "" {
		t.Errorf("recovery left a notice behind: %q", n)
	}
}

func TestSnapshotSurvivesRestart(t *testing.T) {
	// A restart during an outage used to blank the strip: the carried-forward
	// reading lived only in memory even though the readings were on disk.
	path := filepath.Join(t.TempDir(), "usage-limits.sqlite")
	first, err := newUsageLimitsStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	observedAt := time.Now().UTC().Add(-20 * time.Minute)
	first.observe(&usageProviderSnapshot{
		Provider:   "claude",
		PlanType:   "max",
		ObservedAt: observedAt,
		Buckets: []usageBucket{{
			Key: "weekly", Label: "week", Group: "weekly", UsedPercent: 78,
			WindowMinutes: 10080, ResetsAt: time.Now().Add(48 * time.Hour).Unix(),
			Severity: "warning",
		}},
	})
	first.Close()

	second, err := newUsageLimitsStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer second.Close()

	got := second.snapshot()
	if len(got) != 1 || len(got[0].Buckets) != 1 {
		t.Fatalf("snapshot was not seeded from history: %+v", got)
	}
	b := got[0].Buckets[0]
	if b.Key != "weekly" || b.UsedPercent != 78 || b.Severity != "warning" || b.WindowMinutes != 10080 {
		t.Errorf("seeded bucket lost fields: %+v", b)
	}
	if got[0].PlanType != "max" {
		t.Errorf("plan type = %q", got[0].PlanType)
	}
	// Dated by the stored reading, not by the restart — otherwise the UI would
	// present a restart as a fresh read.
	if delta := got[0].ObservedAt.Sub(observedAt); delta > time.Second || delta < -time.Second {
		t.Errorf("observedAt = %s, want the stored %s", got[0].ObservedAt, observedAt)
	}

	// Seeding must not make the next unchanged reading append a duplicate.
	second.observe(&usageProviderSnapshot{
		Provider: "claude", ObservedAt: time.Now().UTC(),
		Buckets: []usageBucket{{Key: "weekly", Label: "week", UsedPercent: 78, Severity: "warning"}},
	})
	var n int
	if err := second.db.QueryRow(`SELECT COUNT(*) FROM usage_limit_observations`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("unchanged reading after restart appended: %d rows, want 1", n)
	}
}

func TestObserveAppendsOnlyOnChange(t *testing.T) {
	store, err := newUsageLimitsStore(filepath.Join(t.TempDir(), "usage-limits.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	rows := func() int {
		t.Helper()
		var n int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM usage_limit_observations`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	snap := func(pct float64) *usageProviderSnapshot {
		return &usageProviderSnapshot{
			Provider:   "codex",
			ObservedAt: time.Now().UTC(),
			Buckets:    []usageBucket{{Key: "codex:primary", Label: "week", UsedPercent: pct}},
		}
	}

	store.observe(snap(92))
	if rows() != 1 {
		t.Fatalf("first observation wrote %d rows", rows())
	}
	// An unchanged reading is the common case — the poller fires every few
	// minutes and the percentage moves far more slowly than that.
	store.observe(snap(92))
	if rows() != 1 {
		t.Errorf("unchanged reading appended a row: %d total", rows())
	}
	store.observe(snap(93))
	if rows() != 2 {
		t.Errorf("changed reading did not append: %d total", rows())
	}
}

func TestObserveRetriesUnchangedReadingAfterWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-limits.sqlite")
	store, err := newUsageLimitsStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Keep the store on one connection so this short timeout applies to the
	// connection appendRows will use. A second connection then holds SQLite's
	// write lock long enough for the first append to fail deterministically.
	store.db.SetMaxOpenConns(1)
	if _, err := store.db.Exec(`PRAGMA busy_timeout=10`); err != nil {
		t.Fatalf("shorten store busy timeout: %v", err)
	}
	locker, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open lock holder: %v", err)
	}
	defer locker.Close()
	lockTx, err := locker.Begin()
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	defer lockTx.Rollback()
	if _, err := lockTx.Exec(`INSERT INTO usage_limit_observations
		(observed_at, provider, bucket_key, used_percent) VALUES ('held', 'test', 'held', 0)`); err != nil {
		t.Fatalf("acquire write lock: %v", err)
	}

	reading := &usageProviderSnapshot{
		Provider:   "codex",
		ObservedAt: time.Now().UTC(),
		Buckets:    []usageBucket{{Key: "weekly", Label: "week", UsedPercent: 42}},
	}
	store.observe(reading)
	if err := lockTx.Rollback(); err != nil {
		t.Fatalf("release write lock: %v", err)
	}

	// The value has not changed, but the previous attempt did not commit. It
	// therefore still needs to be written rather than suppressed as a duplicate.
	store.observe(reading)
	var rows int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM usage_limit_observations WHERE provider = 'codex'`).Scan(&rows); err != nil {
		t.Fatalf("count retried rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("unchanged reading after failed append wrote %d rows, want 1", rows)
	}
}

func TestObserveIgnoresDriftingResetTime(t *testing.T) {
	store, err := newUsageLimitsStore(filepath.Join(t.TempDir(), "usage-limits.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Codex reports an untouched weekly window's reset as "now + 7 days", so
	// the timestamp advances by one poll interval on every poll while the
	// percentage never moves. That must not append a row each time.
	base := time.Now().Unix() + 7*24*3600
	for i := 0; i < 4; i++ {
		store.observe(&usageProviderSnapshot{
			Provider:   "codex",
			ObservedAt: time.Now().UTC(),
			Buckets: []usageBucket{{
				Key: "codex_bengalfox:primary", Label: "Spark",
				UsedPercent: 0, ResetsAt: base + int64(i)*300,
			}},
		})
	}
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM usage_limit_observations`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("drifting reset time appended %d rows, want 1", n)
	}
}

func TestChangeDetectionSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-limits.sqlite")
	reading := func() *usageProviderSnapshot {
		return &usageProviderSnapshot{
			Provider:   "claude",
			ObservedAt: time.Now().UTC(),
			Buckets:    []usageBucket{{Key: "weekly", Label: "week", UsedPercent: 54, Severity: "normal"}},
		}
	}

	first, err := newUsageLimitsStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	first.observe(reading())
	first.Close()

	// A restart must not re-append an unchanged reading: the dedup state is
	// rebuilt from the newest stored row rather than starting empty.
	second, err := newUsageLimitsStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer second.Close()
	second.observe(reading())

	var n int
	if err := second.db.QueryRow(`SELECT COUNT(*) FROM usage_limit_observations`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("restart re-appended an unchanged reading: %d rows, want 1", n)
	}

	// A real change after the restart must still land.
	changed := reading()
	changed.Buckets[0].UsedPercent = 55
	second.observe(changed)
	if err := second.db.QueryRow(`SELECT COUNT(*) FROM usage_limit_observations`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("changed reading after restart did not append: %d rows, want 2", n)
	}
}

func TestObserveCarriesLastGoodReadingThroughAnError(t *testing.T) {
	store, err := newUsageLimitsStore(filepath.Join(t.TempDir(), "usage-limits.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	store.observe(&usageProviderSnapshot{
		Provider:   "claude",
		PlanType:   "max",
		ObservedAt: time.Now().UTC(),
		Buckets:    []usageBucket{{Key: "weekly", Label: "week", UsedPercent: 53}},
	})
	// A failed poll must not blank the bars: an empty strip reads as "no usage
	// at all", which is the opposite of what a transport failure means.
	store.observe(&usageProviderSnapshot{Provider: "claude", Error: "token expired"})

	got := store.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 provider, got %d", len(got))
	}
	if len(got[0].Buckets) != 1 || got[0].Buckets[0].UsedPercent != 53 {
		t.Errorf("last good reading was lost: %+v", got[0])
	}
	if got[0].Error != "token expired" {
		t.Errorf("error not surfaced: %q", got[0].Error)
	}
	if got[0].ObservedAt.IsZero() {
		t.Error("observedAt must stay at the last successful read so the UI can age it")
	}
}

func TestObserveKeepsErrorDetailAndClearsOnRecovery(t *testing.T) {
	store, err := newUsageLimitsStore(filepath.Join(t.TempDir(), "usage-limits.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	good := func() *usageProviderSnapshot {
		return &usageProviderSnapshot{
			Provider:   "claude",
			ObservedAt: time.Now().UTC(),
			Buckets:    []usageBucket{{Key: "weekly", Label: "week", UsedPercent: 54}},
		}
	}
	store.observe(good())

	failed := (&usageProviderSnapshot{Provider: "claude"}).
		Fail("Claude rejected the login token — run any Claude Code session",
			errGetReturned401)
	store.observe(failed)

	got := store.snapshot()[0]
	// The visible message is written for a human; the technical cause rides
	// along for the tooltip rather than replacing it.
	if got.Error != "Claude rejected the login token — run any Claude Code session" {
		t.Errorf("visible message = %q", got.Error)
	}
	if got.ErrorDetail != errGetReturned401.Error() {
		t.Errorf("error detail = %q", got.ErrorDetail)
	}

	// A later success must clear both, or the rail keeps shouting after the
	// problem is gone.
	store.observe(good())
	got = store.snapshot()[0]
	if got.Error != "" || got.ErrorDetail != "" {
		t.Errorf("recovery left error state behind: %q / %q", got.Error, got.ErrorDetail)
	}
}

var errGetReturned401 = errors.New("GET https://api.anthropic.com/api/oauth/usage returned 401 Unauthorized")

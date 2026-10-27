package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The two provider payloads below are verbatim-shaped captures of live
// responses, trimmed to the fields nano reads. They exist so a provider
// changing its shape fails here rather than silently drawing a 0% bar.

const codexRateLimitsFixture = `{
  "rateLimits": {
    "limitId": "codex", "limitName": null, "planType": "pro",
    "primary": {"usedPercent": 92, "windowDurationMins": 10080, "resetsAt": 1786160206},
    "secondary": null
  },
  "rateLimitsByLimitId": {
    "codex_bengalfox": {
      "limitId": "codex_bengalfox", "limitName": "GPT-5.3-Codex-Spark", "planType": "pro",
      "primary": {"usedPercent": 0, "windowDurationMins": 10080, "resetsAt": 1786655860},
      "secondary": null
    },
    "codex": {
      "limitId": "codex", "limitName": null, "planType": "pro",
      "primary": {"usedPercent": 92, "windowDurationMins": 10080, "resetsAt": 1786160206},
      "secondary": null
    }
  },
  "rateLimitResetCredits": {
    "availableCount": 1,
    "credits": [{
      "id": "RateLimitResetCredit_example", "resetType": "codexRateLimits",
      "status": "available", "grantedAt": 1783964577, "expiresAt": 1786556577,
      "title": "Full reset", "description": "One free rate limit reset."
    }]
  }
}`

const claudeUsageFixture = `{
  "limits": [
    {"kind": "session", "group": "session", "percent": 61, "severity": "normal",
     "resets_at": "2026-08-06T23:40:00.435867+00:00", "scope": null, "is_active": false},
    {"kind": "weekly_all", "group": "weekly", "percent": 53, "severity": "normal",
     "resets_at": "2026-08-10T00:59:59.435885+00:00", "scope": null, "is_active": false},
    {"kind": "weekly_scoped", "group": "weekly", "percent": 82, "severity": "warning",
     "resets_at": "2026-08-10T01:00:00.436135+00:00",
     "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}, "is_active": true}
  ]
}`

func bucketByKey(t *testing.T, buckets []usageBucket, key string) usageBucket {
	t.Helper()
	for _, b := range buckets {
		if b.Key == key {
			return b
		}
	}
	t.Fatalf("no bucket %q in %+v", key, buckets)
	return usageBucket{}
}

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

func TestClaudeUsageBucketsFromLimitsArray(t *testing.T) {
	var payload claudeUsageResponse
	if err := json.Unmarshal([]byte(claudeUsageFixture), &payload); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	var buckets []usageBucket
	for _, lim := range payload.Limits {
		if b, ok := claudeUsageBucket(lim); ok {
			buckets = append(buckets, b)
		}
	}
	if len(buckets) != 3 {
		t.Fatalf("want 3 buckets, got %d", len(buckets))
	}

	session := bucketByKey(t, buckets, "session")
	if session.Label != "5h" || session.UsedPercent != 61 || !session.Primary {
		t.Errorf("session bucket = %+v", session)
	}
	weekly := bucketByKey(t, buckets, "weekly")
	if weekly.Label != "week" || weekly.UsedPercent != 53 || !weekly.Primary {
		t.Errorf("weekly bucket = %+v", weekly)
	}

	// A per-model cap keys on its scope so a second model does not overwrite
	// the first one's series, and it is NOT primary — it earns a bar only by
	// being active or elevated.
	scoped := bucketByKey(t, buckets, "weekly_scoped:fable")
	if scoped.Label != "Fable" || scoped.UsedPercent != 82 {
		t.Errorf("scoped bucket = %+v", scoped)
	}
	if scoped.Primary {
		t.Error("per-model cap must not be primary")
	}
	if !scoped.IsActive || scoped.Severity != "warning" {
		t.Errorf("scoped bucket lost its provider severity/active flags: %+v", scoped)
	}
	if got := time.Unix(scoped.ResetsAt, 0).UTC().Format(time.RFC3339); got != "2026-08-10T01:00:00Z" {
		t.Errorf("resets_at = %s", got)
	}
}

func TestClaudeUsageBucketWithoutScopeKeepsKindKey(t *testing.T) {
	// An unknown or unscoped kind must still produce a stable key rather than
	// colliding with another bucket at the empty string.
	b, ok := claudeUsageBucket(claudeUsageLimit{Kind: "monthly_all", Group: "monthly", Percent: 12})
	if !ok {
		t.Fatal("unscoped limit was dropped")
	}
	if b.Key != "monthly_all" || b.Label != "monthly_all" {
		t.Errorf("bucket = %+v", b)
	}
}

func TestCodexUsageBucketsPreferKeyedView(t *testing.T) {
	var resp codexRateLimitsResponse
	if err := json.Unmarshal([]byte(codexRateLimitsFixture), &resp); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	var buckets []usageBucket
	// Mirror fetchCodexUsage's mapping without needing a live app-server.
	for id, snap := range resp.RateLimitsByLimitID {
		name := ""
		if snap.LimitName != nil {
			name = *snap.LimitName
		}
		if b, ok := codexUsageBucket(id, "primary", name, id == "codex", snap.Primary); ok {
			buckets = append(buckets, b)
		}
		if b, ok := codexUsageBucket(id, "secondary", name, false, snap.Secondary); ok {
			buckets = append(buckets, b)
		}
	}
	if len(buckets) != 2 {
		t.Fatalf("want 2 buckets (null secondaries dropped), got %d: %+v", len(buckets), buckets)
	}

	account := bucketByKey(t, buckets, "codex:primary")
	if account.UsedPercent != 92 || !account.Primary {
		t.Errorf("account bucket = %+v", account)
	}
	// The 7-day window reads "week" on both providers so the stacked bars share
	// one word.
	if account.Label != "week" || account.Group != "weekly" {
		t.Errorf("account label/group = %q/%q", account.Label, account.Group)
	}
	scoped := bucketByKey(t, buckets, "codex_bengalfox:primary")
	if scoped.Label != "GPT-5.3-Codex-Spark" || scoped.Primary {
		t.Errorf("per-model bucket = %+v", scoped)
	}
	if resp.RateLimitResetCredits == nil || resp.RateLimitResetCredits.AvailableCount != 1 {
		t.Errorf("reset credits = %+v", resp.RateLimitResetCredits)
	}
	if got := resp.RateLimitResetCredits.Credits; len(got) != 1 || got[0].ExpiresAt == nil || *got[0].ExpiresAt != 1786556577 {
		t.Errorf("reset credit expiry = %+v", got)
	}
	count, credits := codexUsageResetCredits(resp.RateLimitResetCredits)
	if count != 1 || len(credits) != 1 || credits[0].ExpiresAt != 1786556577 || credits[0].Title != "Full reset" {
		t.Errorf("mapped reset credits = count=%d credits=%+v", count, credits)
	}
}

func TestUsageLabelAndGroupForWindow(t *testing.T) {
	cases := []struct {
		minutes int
		label   string
		group   string
	}{
		{300, "5h", "session"},
		{10080, "week", "weekly"},
		{60, "1h", "session"},
		{30, "30m", "session"},
		{0, "usage", "weekly"},
	}
	for _, tc := range cases {
		if got := usageLabelForWindow(tc.minutes); got != tc.label {
			t.Errorf("label(%d) = %q, want %q", tc.minutes, got, tc.label)
		}
		if got := usageGroupForWindow(tc.minutes); got != tc.group {
			t.Errorf("group(%d) = %q, want %q", tc.minutes, got, tc.group)
		}
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

func TestFreshestCredentialsPrefersTheLaterToken(t *testing.T) {
	// The regression this guards: on macOS the live credential is in the
	// Keychain and ~/.claude/.credentials.json is a leftover Claude Code no
	// longer maintains. Reading only the file eventually reads a permanently
	// expired token — observed 2026-08-10, file expired 08:21 while the
	// Keychain held one good until 10:11.
	at := func(ms int64) *claudeCredentials {
		c := &claudeCredentials{}
		c.ClaudeAiOauth.AccessToken = "sk-ant-oat01-test"
		c.ClaudeAiOauth.ExpiresAt = ms
		return c
	}
	older, newer := at(1000), at(2000)

	if got := freshestCredentials(older, newer); got != newer {
		t.Error("the later-expiring source must win even when it is the fallback")
	}
	if got := freshestCredentials(newer, older); got != newer {
		t.Error("the later-expiring source must win when it is preferred")
	}
	if got := freshestCredentials(nil, older); got != older {
		t.Error("a missing preferred source must fall back")
	}
	if got := freshestCredentials(newer, nil); got != newer {
		t.Error("a missing fallback must not discard the preferred source")
	}
	if got := freshestCredentials(nil, nil); got != nil {
		t.Error("two missing sources must yield nil, not a zero credential")
	}
	// Equal expiries go to the platform's authoritative store.
	a, b := at(1000), at(1000)
	if got := freshestCredentials(a, b); got != a {
		t.Error("a tie must go to the preferred source")
	}
}

func TestParseClaudeCredentialsRejectsATokenlessPayload(t *testing.T) {
	// An empty or shape-changed store must not read as a usable credential,
	// or the caller would send an empty bearer token and get a confusing 401.
	if _, err := parseClaudeCredentials([]byte(`{"claudeAiOauth":{}}`)); err == nil {
		t.Error("a payload with no access token should not parse as usable")
	}
	if _, err := parseClaudeCredentials([]byte(`not json`)); err == nil {
		t.Error("malformed JSON should error")
	}
	got, err := parseClaudeCredentials([]byte(
		`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-x","expiresAt":123,"refreshTokenExpiresAt":456}}`))
	if err != nil {
		t.Fatalf("valid payload: %v", err)
	}
	if got.ClaudeAiOauth.ExpiresAt != 123 || got.ClaudeAiOauth.RefreshTokenExpiresAt != 456 {
		t.Errorf("fields lost: %+v", got.ClaudeAiOauth)
	}
}

func TestStaleAccessTokenIsNotAnExpiredLogin(t *testing.T) {
	// The regression this guards: an access token lapses roughly every eight
	// hours and Claude Code renews it silently. Reporting that as "login
	// expired" sent the operator hunting for an account problem that did not
	// exist. Only a dead REFRESH token is a real login expiry.
	now := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	creds := func(access, refresh time.Time) *claudeCredentials {
		c := &claudeCredentials{}
		c.ClaudeAiOauth.AccessToken = "sk-ant-oat01-test"
		c.ClaudeAiOauth.ExpiresAt = access.UnixMilli()
		c.ClaudeAiOauth.RefreshTokenExpiresAt = refresh.UnixMilli()
		return c
	}

	routine := creds(now.Add(-30*time.Minute), now.Add(26*24*time.Hour))
	if !routine.accessTokenStale(now) {
		t.Error("a lapsed access token should read as stale")
	}
	if routine.expiredLogin(now) {
		t.Error("a healthy refresh token must not be reported as an expired login")
	}

	dead := creds(now.Add(-30*time.Minute), now.Add(-time.Hour))
	if !dead.expiredLogin(now) {
		t.Error("a lapsed refresh token is a real login expiry")
	}

	healthy := creds(now.Add(4*time.Hour), now.Add(26*24*time.Hour))
	if healthy.accessTokenStale(now) || healthy.expiredLogin(now) {
		t.Error("a valid pair should be neither stale nor expired")
	}

	// An absent refresh expiry must not be treated as expired — a genuinely
	// dead login still surfaces through the endpoint's 401.
	noRefresh := &claudeCredentials{}
	noRefresh.ClaudeAiOauth.ExpiresAt = now.Add(-time.Hour).UnixMilli()
	if noRefresh.expiredLogin(now) {
		t.Error("a missing refresh expiry must not be reported as an expired login")
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

func TestFetchClaudeUsageReportsRejectedTokenInPlainEnglish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	got := claudeUsageFromEndpoint(context.Background(), srv.URL, "token-that-was-revoked")
	if got.Error == "" {
		t.Fatal("a 401 must surface an error, not an empty strip")
	}
	if !strings.Contains(got.Error, "rejected the login token") {
		t.Errorf("message is not the operator-facing one: %q", got.Error)
	}
	if !strings.Contains(got.ErrorDetail, "401") {
		t.Errorf("detail lost the status code: %q", got.ErrorDetail)
	}
	if len(got.Buckets) != 0 {
		t.Error("a failed read must not invent buckets")
	}
}

func TestFetchClaudeUsageReportsMovedEndpoint(t *testing.T) {
	// The endpoint is undocumented; a 404 is what a Claude Code update moving
	// it would look like, and the operator needs to be told it is a code
	// problem rather than something wrong with their account.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	got := claudeUsageFromEndpoint(context.Background(), srv.URL, "any-token")
	if !strings.Contains(got.Error, "may have moved") {
		t.Errorf("404 message = %q", got.Error)
	}
}

func TestFetchClaudeUsageRejectsAnEmptyLimitsList(t *testing.T) {
	// A 200 carrying nothing usable is the quiet way an upstream change
	// breaks this: it must not render as healthy zero usage.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"limits":[]}`))
	}))
	defer srv.Close()

	got := claudeUsageFromEndpoint(context.Background(), srv.URL, "any-token")
	if got.Error == "" {
		t.Fatal("an empty limits list must be an error, not a silent success")
	}
	if len(got.Buckets) != 0 {
		t.Error("no buckets should be produced")
	}
}

var errGetReturned401 = errors.New("GET https://api.anthropic.com/api/oauth/usage returned 401 Unauthorized")

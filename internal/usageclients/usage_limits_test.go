package usageclients

import (
	"context"
	"encoding/json"
	"github.com/dkroshow/nano/internal/usage"
	"net/http"
	"net/http/httptest"
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

func bucketByKey(t *testing.T, buckets []usage.Bucket, key string) usage.Bucket {
	t.Helper()
	for _, b := range buckets {
		if b.Key == key {
			return b
		}
	}
	t.Fatalf("no bucket %q in %+v", key, buckets)
	return usage.Bucket{}
}

func TestClaudeUsageBucketsFromLimitsArray(t *testing.T) {
	var payload claudeUsageResponse
	if err := json.Unmarshal([]byte(claudeUsageFixture), &payload); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	var buckets []usage.Bucket
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
	var buckets []usage.Bucket
	// Mirror usageclients.FetchCodexUsage's mapping without needing a live app-server.
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

package usageclients

// usage_limits_claude.go — the Claude half of account usage.
//
// Claude Code stores no usage figures on disk and exposes none through its
// CLI: its `/usage` screen is drawn from a live HTTP call. Nano makes the same
// call, with the OAuth access token Claude Code maintains in
// ~/.claude/.credentials.json.
//
// Two constraints shape this file:
//
//  0. THE CREDENTIAL STORE IS PLATFORM-SPECIFIC. macOS keeps it in the
//     encrypted Keychain; Linux and Windows keep it in
//     ~/.claude/.credentials.json. On a Mac the file still exists but is a
//     leftover Claude Code no longer maintains, so reading only the file
//     eventually reads an expired token forever. readClaudeCredentials reads
//     both and takes the fresher.
//
//  1. READ-ONLY toward ~/.claude. Nano never writes that file. In particular
//     it does NOT refresh the OAuth token itself: an OAuth refresh normally
//     invalidates the refresh token it consumed, and nano cannot write the
//     replacement back without racing Claude Code and corrupting the operator's
//     login. So the contract is: read whatever token is on disk, and if it has
//     expired, report that and let the store keep serving the last good
//     reading with its age. Claude Code rewrites the token roughly every eight
//     hours, so any machine that runs Claude at all stays fresh on its own.
//
//  2. UNDOCUMENTED endpoint. This is not a published API and the shape can
//     change under a Claude Code upgrade. Every field is read defensively and
//     a parse miss degrades to an error string on the snapshot, never a panic
//     or a zeroed bar. If it ever breaks, the call site is rediscoverable with
//     `strings <claude-binary> | grep oauth/usage`.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/dkroshow/nano/internal/usage"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	claudeUsageEndpoint  = "https://api.anthropic.com/api/oauth/usage"
	claudeOAuthBetaValue = "oauth-2025-04-20"
)

// claudeCredentials is the subset of ~/.claude/.credentials.json nano reads.
// The refresh TOKEN itself is deliberately left unread — nano has no use for
// it and no business holding it — but its EXPIRY is read, because without it
// nano cannot tell a routine condition from a real one. See expiredLogin.
type claudeCredentials struct {
	ClaudeAiOauth struct {
		AccessToken string `json:"accessToken"`
		// ExpiresAt is unix MILLISECONDS.
		ExpiresAt int64 `json:"expiresAt"`
		// RefreshTokenExpiresAt is unix MILLISECONDS. Zero when absent.
		RefreshTokenExpiresAt int64  `json:"refreshTokenExpiresAt"`
		SubscriptionType      string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

// accessTokenStale reports that the short-lived access token has lapsed. This
// happens roughly every eight hours and is NOT a problem: Claude Code renews
// it from the refresh token the next time any session takes a turn.
func (c *claudeCredentials) accessTokenStale(now time.Time) bool {
	exp := c.ClaudeAiOauth.ExpiresAt
	return exp > 0 && now.After(time.UnixMilli(exp))
}

// expiredLogin reports that the login itself is dead — the refresh token is
// gone or lapsed, so nothing can renew the access token and the operator has
// to sign in again.
//
// Keeping these apart matters because nano previously called any stale access
// token a "login expired" and painted it red, which is the phrase Claude Code
// reserves for exactly this state. The operator went looking for a login
// problem that did not exist while a routine eight-hour rollover healed
// itself. A refresh window is around a month; an access token is hours.
func (c *claudeCredentials) expiredLogin(now time.Time) bool {
	exp := c.ClaudeAiOauth.RefreshTokenExpiresAt
	// Absent expiry: assume the login is fine rather than cry wolf. A truly
	// dead login still surfaces — the endpoint answers 401 and that path
	// reports it.
	return exp > 0 && now.After(time.UnixMilli(exp))
}

// claudeUsageLimit is one entry of the endpoint's flattened `limits` array —
// the provider's own UI-ready projection of its typed per-window fields.
// Reading this rather than the typed fields means new limit kinds appear
// automatically instead of needing a matching struct field here.
type claudeUsageLimit struct {
	Kind     string  `json:"kind"`
	Group    string  `json:"group"`
	Percent  float64 `json:"percent"`
	Severity string  `json:"severity"`
	ResetsAt *string `json:"resets_at"`
	IsActive bool    `json:"is_active"`
	Scope    *struct {
		Model *struct {
			DisplayName *string `json:"display_name"`
		} `json:"model"`
		Surface *struct {
			DisplayName *string `json:"display_name"`
		} `json:"surface"`
	} `json:"scope"`
}

type claudeUsageResponse struct {
	Limits []claudeUsageLimit `json:"limits"`
}

var claudeUsageHTTPClient = &http.Client{Timeout: 20 * time.Second}

// FetchClaudeUsage reads the live subscription usage. Like the Codex fetcher it
// always returns a snapshot; failures carry Error and no buckets.
func FetchClaudeUsage(ctx context.Context) *usage.ProviderSnapshot {
	out := &usage.ProviderSnapshot{Provider: "claude", Buckets: []usage.Bucket{}}

	creds, err := readClaudeCredentials()
	if err != nil {
		return out.Fail("Can't read the Claude login file", err)
	}
	out.PlanType = creds.ClaudeAiOauth.SubscriptionType
	if creds.ClaudeAiOauth.AccessToken == "" {
		return out.Fail("No Claude login token — sign in to Claude Code", nil)
	}
	now := time.Now()
	if creds.expiredLogin(now) {
		// The refresh token is gone: nothing can renew the access token and
		// only the operator can fix it.
		return out.Fail("Claude login expired — run /login in Claude Code",
			fmt.Errorf("refresh token expired at %s",
				time.UnixMilli(creds.ClaudeAiOauth.RefreshTokenExpiresAt).Format(time.RFC3339)))
	}
	if creds.accessTokenStale(now) {
		// Routine eight-hour rollover with a healthy login behind it. Skip the
		// call — it would only 401 and spend a request against an endpoint
		// that rate-limits — and say so WITHOUT calling it a failure. Nano
		// deliberately does not refresh the token itself; see the file header.
		return out.Wait("waiting for a Claude session to refresh the token")
	}

	snap := claudeUsageFromEndpoint(ctx, claudeUsageEndpoint, creds.ClaudeAiOauth.AccessToken)
	// The plan name comes from the credentials file, not the response, so it
	// survives a failed call and keeps labelling the stale bars.
	snap.PlanType = out.PlanType
	return snap
}

// claudeUsageFromEndpoint is the HTTP half, split out so the failure paths can
// be driven against a local test server instead of the real account.
func claudeUsageFromEndpoint(ctx context.Context, endpoint, token string) *usage.ProviderSnapshot {
	out := &usage.ProviderSnapshot{Provider: "claude", Buckets: []usage.Bucket{}}

	reqCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return out.Fail("Can't build the Claude usage request", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", claudeOAuthBetaValue)
	req.Header.Set("Content-Type", "application/json")

	resp, err := claudeUsageHTTPClient.Do(req)
	if err != nil {
		return out.Fail("Can't reach api.anthropic.com", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// The on-disk token went stale between the expiry check and the call,
		// or was revoked. Same remedy either way, and the same deliberate
		// refusal to self-refresh.
		return out.Fail("Claude rejected the login token — run any Claude Code session",
			fmt.Errorf("GET %s returned %s", endpoint, resp.Status))
	case resp.StatusCode == http.StatusNotFound:
		// The endpoint is undocumented; a 404 is what a Claude Code update
		// moving it would look like. Say so, because the fix is code, not
		// anything the operator can do to their account.
		return out.Fail("Claude usage endpoint is gone — it may have moved in a Claude Code update",
			fmt.Errorf("GET %s returned %s", endpoint, resp.Status))
	default:
		return out.Fail("Claude usage request failed ("+resp.Status+")",
			fmt.Errorf("GET %s returned %s", endpoint, resp.Status))
	}

	var payload claudeUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return out.Fail("Claude sent usage data nano couldn't read", err)
	}

	for _, lim := range payload.Limits {
		if b, ok := claudeUsageBucket(lim); ok {
			out.Buckets = append(out.Buckets, b)
		}
	}
	if len(out.Buckets) == 0 {
		// A 200 with nothing usable in it is the other shape an upstream
		// change takes, and it is the one that would otherwise pass silently.
		return out.Fail("Claude reported no usage limits", fmt.Errorf("limits[] was empty or unrecognized"))
	}
	out.ObservedAt = time.Now().UTC()
	return out
}

// claudeUsageBucket normalizes one limits[] entry. The two plan-wide windows
// keep stable keys; a per-model or per-surface cap is keyed by its scope so a
// new model appearing mid-week starts its own series rather than overwriting
// another model's.
func claudeUsageBucket(lim claudeUsageLimit) (usage.Bucket, bool) {
	if lim.Kind == "" {
		return usage.Bucket{}, false
	}
	b := usage.Bucket{
		UsedPercent: lim.Percent,
		Severity:    lim.Severity,
		IsActive:    lim.IsActive,
		Group:       lim.Group,
	}
	scopeName := claudeScopeName(lim)
	switch lim.Kind {
	case "session":
		b.Key, b.Label, b.Primary = "session", "5h", true
		if b.Group == "" {
			b.Group = "session"
		}
		b.WindowMinutes = 300
	case "weekly_all":
		b.Key, b.Label, b.Primary = "weekly", "week", true
		b.Group = "weekly"
		b.WindowMinutes = 7 * 24 * 60
	default:
		// weekly_scoped and any kind added later. Without a scope name there
		// is nothing to distinguish two entries of the same kind, so fold the
		// kind into the key and let the label say what it is.
		b.Key = lim.Kind
		b.Label = lim.Kind
		if scopeName != "" {
			b.Key = lim.Kind + ":" + strings.ToLower(scopeName)
			b.Label = scopeName
		}
		if b.Group == "" {
			b.Group = "weekly"
		}
		if b.Group == "weekly" {
			b.WindowMinutes = 7 * 24 * 60
		}
	}
	if lim.ResetsAt != nil {
		if t, err := time.Parse(time.RFC3339, *lim.ResetsAt); err == nil {
			b.ResetsAt = t.Unix()
		}
	}
	return b, true
}

func claudeScopeName(lim claudeUsageLimit) string {
	if lim.Scope == nil {
		return ""
	}
	if lim.Scope.Model != nil && lim.Scope.Model.DisplayName != nil {
		return *lim.Scope.Model.DisplayName
	}
	if lim.Scope.Surface != nil && lim.Scope.Surface.DisplayName != nil {
		return *lim.Scope.Surface.DisplayName
	}
	return ""
}

// readClaudeCredentials returns the freshest credential Claude Code holds.
//
// WHICH SOURCE IS AUTHORITATIVE DEPENDS ON THE PLATFORM. Claude Code stores
// credentials in the encrypted macOS Keychain on darwin, and in
// ~/.claude/.credentials.json on Linux and Windows. Reading only the file on a
// Mac reads a leftover that Claude Code no longer maintains: on 2026-08-10 the
// file's token had expired at 08:21 while the Keychain held one valid until
// 10:11, so nano reported "waiting for a Claude session to refresh the token"
// indefinitely while Claude itself worked perfectly.
//
// Both sources are read and the one with the later expiry wins, rather than
// branching on runtime.GOOS. That is one code path instead of two, and it is
// self-correcting if either store goes stale for a reason we have not seen.
func readClaudeCredentials() (*claudeCredentials, error) {
	fromFile, fileErr := claudeCredentialsFromFile()
	fromKeychain := claudeCredentialsFromKeychain()
	if best := freshestCredentials(fromKeychain, fromFile); best != nil {
		return best, nil
	}
	if fileErr != nil {
		return nil, fileErr
	}
	return nil, fmt.Errorf("no Claude credentials found in the keychain or on disk")
}

// freshestCredentials picks whichever source holds the later-expiring access
// token, tolerating a nil from either. Ties go to the first argument, which
// callers pass as the platform's authoritative store.
func freshestCredentials(preferred, fallback *claudeCredentials) *claudeCredentials {
	switch {
	case preferred == nil:
		return fallback
	case fallback == nil:
		return preferred
	case preferred.ClaudeAiOauth.ExpiresAt >= fallback.ClaudeAiOauth.ExpiresAt:
		return preferred
	default:
		return fallback
	}
}

func claudeCredentialsFromFile() (*claudeCredentials, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	path := filepath.Join(home, ".claude", ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no Claude credentials at %s", path)
		}
		return nil, fmt.Errorf("read Claude credentials: %w", err)
	}
	return parseClaudeCredentials(data)
}

// claudeCredentialsFromKeychain reads the darwin credential store. It returns
// nil for every failure — wrong platform, no such item, a prompt we must not
// wait on — because the file is a legitimate fallback and this must never be
// the reason usage reporting fails.
func claudeCredentialsFromKeychain() *claudeCredentials {
	if runtime.GOOS != "darwin" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// -w prints only the secret. The item is created by this same user, so the
	// read is silent; the timeout is a guard in case an ACL change ever makes
	// it prompt, which would otherwise hang the poll.
	out, err := exec.CommandContext(ctx, "/usr/bin/security",
		"find-generic-password", "-s", claudeKeychainService, "-w").Output()
	if err != nil {
		return nil
	}
	creds, err := parseClaudeCredentials(out)
	if err != nil {
		return nil
	}
	return creds
}

const claudeKeychainService = "Claude Code-credentials"

func parseClaudeCredentials(data []byte) (*claudeCredentials, error) {
	var creds claudeCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("parse Claude credentials: %w", err)
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return nil, fmt.Errorf("Claude credentials contained no access token")
	}
	return &creds, nil
}

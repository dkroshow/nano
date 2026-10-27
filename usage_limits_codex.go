package main

// usage_limits_codex.go — the Codex half of account usage.
//
// Codex publishes this properly: `account/rateLimits/read` is a documented
// request in the app-server protocol schema, answered over the same websocket
// nano already holds for every session, so there is no second connection, no
// credential handling, and no log scraping. (An earlier tool in this family
// read the same numbers out of ~/.codex/logs_2.sqlite by substring-matching
// the app-server's own event log. That is unnecessary here.)
//
// The response carries two views of the same data: `rateLimits` is the
// legacy single-bucket view, and `rateLimitsByLimitId` breaks it out per
// metered limit — the account-wide "codex" bucket plus any per-model buckets
// the plan meters separately. We read the keyed view and fall back to the
// single bucket only when the server omits it.

import (
	"context"
	"fmt"
	"sort"
	"time"
)

const codexRateLimitsReadMethod = "account/rateLimits/read"

// codexRateLimitWindow is one metered window. UsedPercent is percent consumed;
// WindowDurationMins is 10080 for a week, 300 for a 5-hour window.
type codexRateLimitWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins *int    `json:"windowDurationMins"`
	ResetsAt           *int64  `json:"resetsAt"`
}

type codexRateLimitSnapshot struct {
	LimitID   string                `json:"limitId"`
	LimitName *string               `json:"limitName"`
	PlanType  *string               `json:"planType"`
	Primary   *codexRateLimitWindow `json:"primary"`
	Secondary *codexRateLimitWindow `json:"secondary"`
}

type codexRateLimitResetCredit struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	ExpiresAt *int64 `json:"expiresAt"`
}

type codexRateLimitResetCredits struct {
	AvailableCount int                         `json:"availableCount"`
	Credits        []codexRateLimitResetCredit `json:"credits"`
}

type codexRateLimitsResponse struct {
	RateLimits            codexRateLimitSnapshot            `json:"rateLimits"`
	RateLimitsByLimitID   map[string]codexRateLimitSnapshot `json:"rateLimitsByLimitId"`
	RateLimitResetCredits *codexRateLimitResetCredits       `json:"rateLimitResetCredits"`
}

// fetchCodexUsage reads the account-wide limits from the app-server. It always
// returns a snapshot: on failure, one carrying only Error, which the store
// folds into the last good reading rather than discarding it.
func fetchCodexUsage(ctx context.Context, daemon *codexDaemon) *usageProviderSnapshot {
	out := &usageProviderSnapshot{Provider: "codex", Buckets: []usageBucket{}}
	if daemon == nil {
		return out.Fail("Codex connection is not configured", nil)
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var resp codexRateLimitsResponse
	if err := daemon.Call(callCtx, codexRateLimitsReadMethod, map[string]any{}, &resp); err != nil {
		return out.Fail("Codex didn't answer the usage request",
			fmt.Errorf("%s: %w", codexRateLimitsReadMethod, err))
	}

	snapshots := resp.RateLimitsByLimitID
	if len(snapshots) == 0 {
		snapshots = map[string]codexRateLimitSnapshot{}
		if resp.RateLimits.LimitID != "" || resp.RateLimits.Primary != nil {
			key := resp.RateLimits.LimitID
			if key == "" {
				key = "codex"
			}
			snapshots[key] = resp.RateLimits
		}
	}

	// Map iteration order is random and this list is served to a UI that draws
	// bars in order, so sort: the account-wide "codex" bucket first, then the
	// per-model buckets alphabetically.
	ids := make([]string, 0, len(snapshots))
	for id := range snapshots {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if (ids[i] == "codex") != (ids[j] == "codex") {
			return ids[i] == "codex"
		}
		return ids[i] < ids[j]
	})

	for _, id := range ids {
		snap := snapshots[id]
		if snap.PlanType != nil && out.PlanType == "" {
			out.PlanType = *snap.PlanType
		}
		name := ""
		if snap.LimitName != nil {
			name = *snap.LimitName
		}
		// The account-wide bucket is the one the strip always shows; the
		// per-model buckets earn their bar only when they light up.
		primary := id == "codex"
		if b, ok := codexUsageBucket(id, "primary", name, primary, snap.Primary); ok {
			out.Buckets = append(out.Buckets, b)
		}
		if b, ok := codexUsageBucket(id, "secondary", name, false, snap.Secondary); ok {
			out.Buckets = append(out.Buckets, b)
		}
	}

	if len(out.Buckets) == 0 {
		// An answer nano can't find any window in means the shape changed.
		// Without this it would render as "no limits", which looks like
		// healthy zero usage.
		return out.Fail("Codex reported no usage limits",
			fmt.Errorf("%s returned no rate-limit windows", codexRateLimitsReadMethod))
	}
	if resp.RateLimitResetCredits != nil {
		out.ResetCredits, out.ResetCreditDetails = codexUsageResetCredits(resp.RateLimitResetCredits)
	}
	out.ObservedAt = time.Now().UTC()
	return out
}

// codexUsageResetCredits keeps one-time reset expiry separate from metered
// usage windows. The API may include used or expired credits in the array;
// only available credits belong in the operator's current headroom view.
func codexUsageResetCredits(summary *codexRateLimitResetCredits) (int, []usageResetCredit) {
	if summary == nil {
		return 0, nil
	}
	details := make([]usageResetCredit, 0, len(summary.Credits))
	for _, credit := range summary.Credits {
		if credit.Status != "available" {
			continue
		}
		detail := usageResetCredit{ID: credit.ID, Title: credit.Title, Status: credit.Status}
		if credit.ExpiresAt != nil {
			detail.ExpiresAt = *credit.ExpiresAt
		}
		details = append(details, detail)
	}
	sort.Slice(details, func(i, j int) bool {
		// Reported expiry times sort before an unavailable timestamp, which keeps
		// the next action at the top without manufacturing a value for the latter.
		if details[i].ExpiresAt == 0 || details[j].ExpiresAt == 0 {
			return details[j].ExpiresAt == 0 && details[i].ExpiresAt != 0
		}
		return details[i].ExpiresAt < details[j].ExpiresAt
	})
	return summary.AvailableCount, details
}

// codexUsageBucket normalizes one window. Returns ok=false when the provider
// reported no window at that slot, which is the common case for `secondary`.
func codexUsageBucket(limitID, slot, limitName string, primary bool, w *codexRateLimitWindow) (usageBucket, bool) {
	if w == nil {
		return usageBucket{}, false
	}
	b := usageBucket{
		Key:         limitID + ":" + slot,
		UsedPercent: w.UsedPercent,
		Primary:     primary,
	}
	if w.WindowDurationMins != nil {
		b.WindowMinutes = *w.WindowDurationMins
	}
	if w.ResetsAt != nil {
		b.ResetsAt = *w.ResetsAt
	}
	b.Group = usageGroupForWindow(b.WindowMinutes)
	// Prefer the provider's own name for a metered per-model bucket; fall back
	// to the window length so a bar is never labelled with a raw limit id.
	switch {
	case limitName != "":
		b.Label = limitName
	default:
		b.Label = usageLabelForWindow(b.WindowMinutes)
	}
	return b, true
}

// usageGroupForWindow buckets a window length into the two bands the strip
// draws. Anything under a day is the rolling session window.
func usageGroupForWindow(minutes int) string {
	if minutes > 0 && minutes < 24*60 {
		return "session"
	}
	return "weekly"
}

func usageLabelForWindow(minutes int) string {
	switch {
	case minutes <= 0:
		return "usage"
	// Both providers meter a 7-day cap and the strip stacks them, so the
	// common case gets one shared word rather than "week" over "7d".
	case minutes == 7*24*60:
		return "week"
	case minutes >= 24*60:
		return fmt.Sprintf("%dd", minutes/(24*60))
	case minutes >= 60:
		return fmt.Sprintf("%dh", minutes/60)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// The app-server also pushes `account/rateLimits/updated` as turns consume
// quota. NANO DELIBERATELY IGNORES IT. Do not wire it back up without reading
// this first.
//
// It was wired up originally, refetching the full snapshot on each push. With
// a fleet of live sessions those pushes arrive continuously: on 2026-08-06
// that produced 20 Codex reads in 7.4 minutes — gaps as short as 1.7 seconds —
// where the timer alone would have made 2.
//
// Consuming the pushed payload instead of refetching would fix the volume, but
// the notification is documented as a SPARSE update carrying one bucket at a
// time, so it would need merge logic, it never carries the reset-credit count,
// and it has no equivalent on the Claude side. The timer already produces a
// complete snapshot for all providers on one code path, which is worth more
// than sub-minute Codex freshness.

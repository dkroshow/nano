package main

import (
	"context"
	"github.com/dkroshow/nano/internal/httpapi"
	"net/http"
	"strconv"
	"time"

	"github.com/dkroshow/nano/internal/usage"
)

type usageAttempt = usage.Attempt
type usageSeriesPoint = usage.SeriesPoint
type usageSeries = usage.Series

func (store *usageLimitsStore) history(ctx context.Context, window time.Duration) ([]usageSeries, error) {
	return store.History(ctx, window)
}

func (s *server) handleUsageLimitsDetail(w http.ResponseWriter, r *http.Request) {
	if s.usageLimits == nil {
		httpapi.WriteJSON(w, map[string]any{
			"providers": []usageProviderSnapshot{}, "series": []usageSeries{}, "attempts": []usageAttempt{},
		})

		return
	}
	hours := 24
	if raw := r.URL.Query().Get("hours"); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 && value <= 24*30 {
			hours = value
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	series, err := s.usageLimits.History(ctx, time.Duration(hours)*time.Hour)
	if err != nil {
		series = []usageSeries{}
	}
	httpapi.WriteJSON(w, map[string]any{
		"hours": hours, "providers": s.usageLimits.Snapshot(), "series": series,
		"attempts": s.usageLimits.Attempts(120), "pollSeconds": int(s.usageLimitsPollInterval().Seconds()),
	})
}

func (s *server) usageLimitsPollInterval() time.Duration {
	if s.usageLimitsPoller == nil {
		return defaultUsageLimitPollInterval
	}
	return s.usageLimitsPoller.interval
}

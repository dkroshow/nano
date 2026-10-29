package usage

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
)

// Handlers serves retained account usage and its bounded history.
type Handlers struct {
	Store  *Store
	Poller *Poller
}

// HandleSummary serves current provider observations.
func (s *Handlers) HandleSummary(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		httpapi.WriteJSON(w, map[string]any{"providers": []ProviderSnapshot{}})
		return
	}
	httpapi.WriteJSON(w, map[string]any{"providers": s.Store.Snapshot()})
}

// HandleDetail serves current observations and bounded historical readings.
func (s *Handlers) HandleDetail(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		httpapi.WriteJSON(w, map[string]any{
			"providers": []ProviderSnapshot{}, "series": []Series{}, "attempts": []Attempt{},
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
	series, err := s.Store.History(ctx, time.Duration(hours)*time.Hour)
	if err != nil {
		series = []Series{}
	}
	httpapi.WriteJSON(w, map[string]any{
		"hours": hours, "providers": s.Store.Snapshot(), "series": series,
		"attempts": s.Store.Attempts(120), "pollSeconds": int(s.Poller.Interval().Seconds()),
	})
}

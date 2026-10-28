package hosted

import (
	"errors"
	"net/http"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
)

type threadPinMonitorThread struct {
	HostedThreadID string `json:"hostedThreadId"`
	Pins           int    `json:"pins"`
	NewestAt       string `json:"newestAt"`
	ListCalls      int    `json:"listCalls"`
	ContinuedCalls int    `json:"continuedListCalls"`
	MaxListDepth   int    `json:"maxListDepth"`
}

type threadPinMonitorBroken struct {
	PinID          string `json:"pinId"`
	HostedThreadID string `json:"hostedThreadId"`
	NativeThreadID string `json:"nativeThreadId"`
	ItemID         string `json:"itemId"`
	Reason         string `json:"reason"`
}

type threadPinMonitorResponse struct {
	GeneratedAt  string `json:"generatedAt"`
	Note         string `json:"note"`
	Accumulation struct {
		TotalPins      int                      `json:"totalPins"`
		Threads        int                      `json:"threads"`
		CreatedLast24h int                      `json:"createdLast24h"`
		CreatedLast7d  int                      `json:"createdLast7d"`
		ByThread       []threadPinMonitorThread `json:"byThread"`
	} `json:"accumulation"`
	Retrieval struct {
		ListCalls          int     `json:"listCalls"`
		ContinuedListCalls int     `json:"continuedListCalls"`
		MaxListDepth       int     `json:"maxListDepth"`
		ContextReads       int     `json:"contextReads"`
		BrokenContextReads int     `json:"brokenContextReads"`
		PinsEverRead       int     `json:"pinsEverRead"`
		PinsNeverRead      int     `json:"pinsNeverRead"`
		MeanReadRank       float64 `json:"meanReadRank"`
		MaxReadRank        int     `json:"maxReadRank"`
	} `json:"retrieval"`
	Anchors struct {
		Checked   int                      `json:"checked"`
		Unchecked int                      `json:"unchecked"`
		Broken    []threadPinMonitorBroken `json:"broken"`
	} `json:"anchors"`
}

// HandleMonitor reads pin counters and probes anchors without changing pins.
func (s *PinService) HandleMonitor(w http.ResponseWriter, r *http.Request) {
	catalog, ok := s.pinPreconditions(w)
	if !ok {
		return
	}
	var response threadPinMonitorResponse
	now := time.Now().UTC()
	response.GeneratedAt = now.Format(time.RFC3339Nano)
	response.Note = "Observational only: this report never modifies, prunes, or validates pins. Read rank 1 is the newest pin in its thread."
	response.Anchors.Broken = []threadPinMonitorBroken{}
	response.Accumulation.ByThread = []threadPinMonitorThread{}
	db := s.Store.db
	if err := func() error {
		dayAgo, weekAgo := now.Add(-24*time.Hour).Format(time.RFC3339Nano), now.Add(-7*24*time.Hour).Format(time.RFC3339Nano)
		if err := db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT hosted_thread_id),
  COALESCE(SUM(created_at >= ?), 0), COALESCE(SUM(created_at >= ?), 0) FROM thread_pins`, dayAgo, weekAgo).Scan(
			&response.Accumulation.TotalPins, &response.Accumulation.Threads,
			&response.Accumulation.CreatedLast24h, &response.Accumulation.CreatedLast7d); err != nil {
			return err
		}
		rows, err := db.Query(`SELECT p.hosted_thread_id, COUNT(*), MAX(p.created_at),
  COALESCE(l.calls, 0), COALESCE(l.continued_calls, 0), COALESCE(l.max_depth, 0)
FROM thread_pins p LEFT JOIN thread_pin_list_reads l ON l.hosted_thread_id = p.hosted_thread_id
GROUP BY p.hosted_thread_id ORDER BY COUNT(*) DESC, MAX(p.seq) DESC LIMIT 50`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var thread threadPinMonitorThread
			if err := rows.Scan(&thread.HostedThreadID, &thread.Pins, &thread.NewestAt, &thread.ListCalls,
				&thread.ContinuedCalls, &thread.MaxListDepth); err != nil {
				rows.Close()
				return err
			}
			response.Accumulation.ByThread = append(response.Accumulation.ByThread, thread)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := db.QueryRow(`SELECT COALESCE(SUM(calls), 0), COALESCE(SUM(continued_calls), 0), COALESCE(MAX(max_depth), 0)
FROM thread_pin_list_reads`).Scan(&response.Retrieval.ListCalls, &response.Retrieval.ContinuedListCalls,
			&response.Retrieval.MaxListDepth); err != nil {
			return err
		}
		var rankTotal int
		if err := db.QueryRow(`SELECT COALESCE(SUM(reads), 0), COALESCE(SUM(broken_reads), 0), COALESCE(SUM(rank_total), 0),
  COALESCE(MAX(max_rank), 0), (SELECT COUNT(*) FROM thread_pins WHERE pin_id IN (SELECT pin_id FROM thread_pin_context_reads))
FROM thread_pin_context_reads`).Scan(&response.Retrieval.ContextReads, &response.Retrieval.BrokenContextReads,
			&rankTotal, &response.Retrieval.MaxReadRank, &response.Retrieval.PinsEverRead); err != nil {
			return err
		}
		response.Retrieval.PinsNeverRead = response.Accumulation.TotalPins - response.Retrieval.PinsEverRead
		if response.Retrieval.ContextReads > 0 {
			response.Retrieval.MeanReadRank = float64(rankTotal) / float64(response.Retrieval.ContextReads)
		}
		return nil
	}(); err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "pin_monitor_error", "Pin monitor read failed")
		return
	}

	rows, err := db.Query(`SELECT `+threadPinColumns+` FROM thread_pins ORDER BY seq DESC LIMIT ?`, maxPinMonitorAnchorCheck)
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "pin_monitor_error", "Pin monitor read failed")
		return
	}
	var pins []threadPinRecord
	for rows.Next() {
		pin, err := scanThreadPin(rows)
		if err != nil {
			rows.Close()
			httpapi.WriteError(w, http.StatusInternalServerError, "pin_monitor_error", "Pin monitor read failed")
			return
		}
		pins = append(pins, pin)
	}
	rows.Close()
	for _, pin := range pins {
		reason := ""
		threadID, err := s.ResolveThread(r.Context(), pin.NativeThreadID)
		if err == nil {
			_, err = catalog.LocatePinAnchor(r.Context(), threadID, pin.AnchorItemID)
		}
		switch {
		case err == nil:
		case errors.Is(err, history.ErrPinAnchorNotFound):
			reason = "anchor_missing"
		case errors.Is(err, s.CatalogErrors.NotFound):
			reason = "thread_not_indexed"
		default:
			reason = "check_failed"
		}
		response.Anchors.Checked++
		if reason != "" {
			response.Anchors.Broken = append(response.Anchors.Broken, threadPinMonitorBroken{
				PinID: pin.PinID, HostedThreadID: pin.HostedThreadID, NativeThreadID: pin.NativeThreadID,
				ItemID: pin.AnchorItemID, Reason: reason,
			})
		}
	}
	response.Anchors.Unchecked = response.Accumulation.TotalPins - response.Anchors.Checked
	writeCatalogJSON(w, http.StatusOK, response)
}

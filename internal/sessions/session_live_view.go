package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/liveview"
)

// HandleLiveStream is a file-only pane's one stream.
func (sess *Session) HandleLiveStream(w http.ResponseWriter, r *http.Request) {
	pane, err := sess.liveText.RequestPane(r)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpapi.WriteHTTPError(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// Status has one writer, this handler. It subscribes to the projection
	// before it reads the status it sends first, then sends the status again
	// whenever a later update changes it; the loop's messages carry rows and
	// live text only. The projection closes a subscription that falls behind,
	// and the stream ends so the browser reconnects to a fresh status.
	updates, cancel := sess.projection.subscribe()
	defer cancel()
	pane.Start()
	defer pane.Stop()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	write := func(raw []byte) bool {
		_, err := fmt.Fprintf(w, "data: %s\n\n", raw)
		return err == nil
	}
	status := sess.projection.Status()
	writeStatus := func() bool {
		raw, err := json.Marshal(struct {
			Type   string          `json:"type"`
			Status *liveTextStatus `json:"status,omitempty"`
		}{Type: "status", Status: &status})
		return err == nil && write(raw)
	}
	if !writeStatus() {
		return
	}
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-pane.DoneChannel():
			// A failed read leaves its error message to deliver.
			for _, raw := range pane.Take() {
				write(raw)
			}
			flusher.Flush()
			return
		case <-pane.ReadyChannel():
			for _, raw := range pane.Take() {
				if !write(raw) {
					return
				}
			}
			flusher.Flush()
		case update, ok := <-updates:
			if !ok {
				return
			}
			if update.Revision <= status.Revision {
				continue
			}
			next, changed := liveview.StatusAfter(status, update)
			status = next
			if changed {
				if !writeStatus() {
					return
				}
				flusher.Flush()
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// fileConversationItems reads a file-only thread's saved rows for agents, in
// the conversation item shape. Live text never appears here.
func (sess *Session) fileConversationItems(ctx context.Context, view conversationView, before string, limit int) ([]liveProjectionItem, *liveProjectionPage, string, error) {
	return sess.liveText.ConversationItems(ctx, view, before, limit)
}

// fileConversationSnapshot is the agent snapshot of a file-only thread: saved
// rows plus the live status and pending questions. Status keeps the live
// Projection's epoch and revision, the one cursor every status reader uses,
// so an older answer never replaces a newer status.
func (sess *Session) fileConversationSnapshot(ctx context.Context, view conversationView, before string, limit int) (liveProjectionSnapshot, error) {
	items, page, _, err := sess.fileConversationItems(ctx, view, before, limit)
	if err != nil {
		return liveProjectionSnapshot{}, err
	}
	status := sess.projection.Status()
	return liveProjectionSnapshot{Version: liveProjectionVersion, Provider: sess.provider, NativeThreadID: sess.ThreadID(),
		Epoch: status.Epoch, Revision: status.Revision, Items: items, Turns: []liveProjectionTurn{},
		Runtime: status.Runtime, Interactions: status.Interactions, Page: page}, nil
}

// handleFileConversationEvents is the agent event stream of a file-only
// thread: a saved-rows snapshot, then status and pending questions, and a
// rows.changed hint whenever the thread's saved rows change. It subscribes
// before reading its snapshot, so no status change falls between them, and
// ends when the status epoch changes, so the caller reconnects to a new one.
func (sess *Session) handleFileConversationEvents(w http.ResponseWriter, r *http.Request, view conversationView) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpapi.WriteHTTPError(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	updates, cancel := sess.projection.subscribe()
	defer cancel()
	snapshot, err := sess.fileConversationSnapshot(r.Context(), view, "", transcriptFirstPaintRows)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	pane := sess.liveText.RowsPane(view)
	pane.Start()
	defer pane.Stop()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	write := func(value any) bool {
		raw, err := json.Marshal(value)
		if err != nil {
			return false
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
		return err == nil
	}
	if !write(liveProjectionUpdate{Version: liveProjectionVersion, Op: projectionSnapshot, Epoch: snapshot.Epoch, Snapshot: &snapshot}) {
		return
	}
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-pane.DoneChannel():
			return
		case <-pane.ReadyChannel():
			changed := false
			for _, raw := range pane.Take() {
				var message struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(raw, &message) == nil && (message.Type == "tail" || message.Type == "update") {
					changed = true
				}
			}
			if changed && !write(map[string]any{"version": liveProjectionVersion, "op": "rows.changed"}) {
				return
			}
			flusher.Flush()
		case update, ok := <-updates:
			if !ok || update.Epoch != snapshot.Epoch {
				return
			}
			// Row snapshots carry no status news, and the snapshot already
			// holds every update through its revision.
			if update.Op == projectionSnapshot || update.Revision <= snapshot.Revision {
				continue
			}
			if !write(update) {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func parseConversationView(r *http.Request, defaultView conversationView) (conversationView, error) {
	view := conversationView(strings.TrimSpace(r.URL.Query().Get("view")))
	if view == "" {
		view = defaultView
	}
	switch view {
	case conversationViewFull, conversationViewMessages:
		return view, nil
	default:
		return "", fmt.Errorf("view must be %q or %q", conversationViewMessages, conversationViewFull)
	}
}

func parseConversationPage(r *http.Request) (string, int, error) {
	before := strings.TrimSpace(r.URL.Query().Get("before"))
	limit := transcriptFirstPaintRows
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return "", 0, errors.New("limit must be a positive integer")
		}
		limit = value
	}
	if limit > conversationPageMaxRows {
		limit = conversationPageMaxRows
	}
	return before, limit, nil
}

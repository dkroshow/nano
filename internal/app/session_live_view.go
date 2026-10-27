package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
)

// handleLiveStream is a file-only pane's one stream.
func (sess *session) handleLiveStream(w http.ResponseWriter, r *http.Request) {
	query, err := parseTranscriptListQuery(r)
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
	pane := newLiveTextPane(query)
	sess.liveText.subscribe(pane)
	defer sess.liveText.unsubscribe(pane)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	write := func(raw []byte) bool {
		_, err := fmt.Fprintf(w, "data: %s\n\n", raw)
		return err == nil
	}
	status := sess.projection.status()
	writeStatus := func() bool {
		raw, err := json.Marshal(liveTextMessage{Type: "status", Status: &status})
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
		case <-pane.done:
			// A failed read leaves its error message to deliver.
			for _, raw := range pane.take() {
				write(raw)
			}
			flusher.Flush()
			return
		case <-pane.ready:
			for _, raw := range pane.take() {
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
			next, changed := statusAfter(status, update)
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
func (sess *session) fileConversationItems(ctx context.Context, view conversationView, before string, limit int) ([]liveProjectionItem, *liveProjectionPage, string, error) {
	catalog, ok := sess.liveText.srv.catalog.(threadTranscriptCatalog)
	if !ok {
		return nil, nil, "", errors.New("catalog unavailable")
	}
	threadID, err := sess.liveText.catalogThreadID()
	if errors.Is(err, errLiveTextNotIndexed) {
		return []liveProjectionItem{}, &liveProjectionPage{}, "", nil
	}
	if err != nil {
		return nil, nil, "", err
	}
	query := transcriptListQuery{View: defaultThreadTranscriptView, Before: before, Limit: min(limit, maxThreadTranscriptLimit)}
	if view == conversationViewFull {
		query.View, query.HiddenSpecified = devThreadTranscriptView, true
	}
	response, err := catalog.LoadTranscript(ctx, threadID, query)
	if errors.Is(err, errCatalogThreadNotFound) {
		return []liveProjectionItem{}, &liveProjectionPage{}, "", nil
	}
	if err != nil {
		return nil, nil, "", err
	}
	items := make([]liveProjectionItem, 0, len(response.Items))
	for _, row := range response.Items {
		items = append(items, savedConversationItem(row))
	}
	page := &liveProjectionPage{HasMoreBefore: response.Page.HasMoreBefore, NextBeforeCursor: response.Page.NextBeforeCursor, Source: "file"}
	return items, page, response.Projection.Key, nil
}

// fileConversationSnapshot is the agent snapshot of a file-only thread: saved
// rows plus the live status and pending questions. Status keeps the live
// projection's epoch and revision, the one cursor every status reader uses,
// so an older answer never replaces a newer status.
func (sess *session) fileConversationSnapshot(ctx context.Context, view conversationView, before string, limit int) (liveProjectionSnapshot, error) {
	items, page, _, err := sess.fileConversationItems(ctx, view, before, limit)
	if err != nil {
		return liveProjectionSnapshot{}, err
	}
	status := sess.projection.status()
	return liveProjectionSnapshot{Version: liveProjectionVersion, Provider: sess.provider, NativeThreadID: sess.currentThreadID(),
		Epoch: status.Epoch, Revision: status.Revision, Items: items, Turns: []liveProjectionTurn{},
		Runtime: status.Runtime, Interactions: status.Interactions, Page: page}, nil
}

// handleFileConversationEvents is the agent event stream of a file-only
// thread: a saved-rows snapshot, then status and pending questions, and a
// rows.changed hint whenever the thread's saved rows change. It subscribes
// before reading its snapshot, so no status change falls between them, and
// ends when the status epoch changes, so the caller reconnects to a new one.
func (sess *session) handleFileConversationEvents(w http.ResponseWriter, r *http.Request, view conversationView) {
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
	query := transcriptListQuery{View: defaultThreadTranscriptView, Limit: 1}
	if view == conversationViewFull {
		query.View, query.HiddenSpecified = devThreadTranscriptView, true
	}
	pane := newLiveTextPane(query)
	pane.rowsOnly = true
	sess.liveText.subscribe(pane)
	defer sess.liveText.unsubscribe(pane)
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
		case <-pane.done:
			return
		case <-pane.ready:
			changed := false
			for _, raw := range pane.take() {
				var message liveTextMessage
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

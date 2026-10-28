package app

import (
	"context"
	"errors"
	"net/http"
)

// liveTextOf recovers the application-owned loop at application composition sites.
func liveTextOf(sess *session) *liveTextLoop {
	if sess == nil || sess.liveText == nil {
		return nil
	}
	return sess.liveText.(*liveTextLoop)
}

type liveTextStream struct {
	loop *liveTextLoop
	pane *liveTextPane
}

// Start starts the pane’s ordered stream.
func (s *liveTextStream) Start() { s.loop.subscribe(s.pane) }

// Stop releases the pane’s subscription.
func (s *liveTextStream) Stop() { s.loop.unsubscribe(s.pane) }

// DoneChannel closes when the pane stream ends.
func (s *liveTextStream) DoneChannel() <-chan struct{} { return s.pane.done }

// ReadyChannel signals that pane bytes are available.
func (s *liveTextStream) ReadyChannel() <-chan struct{} { return s.pane.ready }

// Take takes the pane’s pending encoded bytes.
func (s *liveTextStream) Take() [][]byte { return s.pane.take() }

// RequestPane creates a pane for the validated request parameters.
func (l *liveTextLoop) RequestPane(r *http.Request) (sessionPane, error) {
	query, err := parseTranscriptListQuery(r)
	if err != nil {
		return nil, err
	}
	return &liveTextStream{loop: l, pane: newLiveTextPane(query)}, nil
}

// RowsPane creates a saved-row pane for a conversation view.
func (l *liveTextLoop) RowsPane(view conversationView) sessionPane {
	query := transcriptListQuery{View: defaultThreadTranscriptView, Limit: 1}
	if view == conversationViewFull {
		query.View, query.HiddenSpecified = devThreadTranscriptView, true
	}
	pane := newLiveTextPane(query)
	pane.rowsOnly = true
	return &liveTextStream{loop: l, pane: pane}
}

// WriteBackendError writes a transcript backend failure to the HTTP response.
func (l *liveTextLoop) WriteBackendError(w http.ResponseWriter, err error) {
	writeTranscriptBackendError(w, err)
}

// ConversationItems reads a bounded page of conversation items.
func (l *liveTextLoop) ConversationItems(ctx context.Context, view conversationView, before string, limit int) ([]liveProjectionItem, *liveProjectionPage, string, error) {
	catalog, ok := l.srv.catalog.(threadTranscriptCatalog)
	if !ok {
		return nil, nil, "", errors.New("catalog unavailable")
	}
	threadID, err := l.catalogThreadID()
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

func newLiveTextLoop(srv *server, sess *session) *liveTextLoop {
	loop := &liveTextLoop{srv: srv, wake: make(chan struct{}, 1)}
	if sess != nil {
		loop.sess = sess
	}
	return loop
}

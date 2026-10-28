package app

import (
	"context"
	"errors"
	"net/http"

	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
)

// liveTextOf recovers the application-owned loop at application composition sites.
func liveTextOf(sess *sessions.Session) *liveTextLoop {
	if sess == nil || sess.LiveTextService() == nil {
		return nil
	}
	return sess.LiveTextService().(*liveTextLoop)
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
func (l *liveTextLoop) RequestPane(r *http.Request) (sessions.Pane, error) {
	query, err := threadindex.ParseTranscriptListQuery(r)
	if err != nil {
		return nil, err
	}
	return &liveTextStream{loop: l, pane: newLiveTextPane(query)}, nil
}

// RowsPane creates a saved-row pane for a conversation view.
func (l *liveTextLoop) RowsPane(view conversationView) sessions.Pane {
	query := threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 1}
	if view == conversationViewFull {
		query.View, query.HiddenSpecified = threadindex.DevThreadTranscriptView, true
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
	catalog, ok := l.srv.catalog.(threadindex.TranscriptCatalog)
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
	query := threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Before: before, Limit: min(limit, threadindex.MaxThreadTranscriptLimit)}
	if view == conversationViewFull {
		query.View, query.HiddenSpecified = threadindex.DevThreadTranscriptView, true
	}
	response, err := catalog.LoadTranscript(ctx, threadID, query)
	if errors.Is(err, threadindex.ErrCatalogThreadNotFound) {
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

func newLiveTextLoop(srv *server, sess *sessions.Session) *liveTextLoop {
	loop := &liveTextLoop{srv: srv, wake: make(chan struct{}, 1)}
	if sess != nil {
		loop.sess = sess
	}
	return loop
}

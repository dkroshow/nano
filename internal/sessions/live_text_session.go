package sessions

import (
	"context"
	"errors"
	"net/http"

	"github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/threadindex"
)

// LiveTextStream owns one pane subscription to the ordered transcript loop.
type LiveTextStream struct {
	loop *LiveTextLoop
	pane *liveTextPane
}

// Start starts the pane’s ordered stream.
func (s *LiveTextStream) Start() { s.loop.subscribe(s.pane) }

// Stop releases the pane’s subscription.
func (s *LiveTextStream) Stop() { s.loop.unsubscribe(s.pane) }

// DoneChannel closes when the pane stream ends.
func (s *LiveTextStream) doneChannel() <-chan struct{} { return s.pane.done }

// ReadyChannel signals that pane bytes are available.
func (s *LiveTextStream) readyChannel() <-chan struct{} { return s.pane.ready }

// Take takes the pane’s pending encoded bytes.
func (s *LiveTextStream) take() [][]byte { return s.pane.take() }

// requestPane creates a pane for the validated request parameters.
func (l *LiveTextLoop) requestPane(r *http.Request) (*LiveTextStream, error) {
	query, err := threadindex.ParseTranscriptListQuery(r)
	if err != nil {
		return nil, err
	}
	return &LiveTextStream{loop: l, pane: newLiveTextPane(query)}, nil
}

// rowsPane creates a saved-row pane for a conversation view.
func (l *LiveTextLoop) rowsPane(view liveview.View) *LiveTextStream {
	query := threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 1}
	if view == conversationViewFull {
		query.View, query.HiddenSpecified = threadindex.DevThreadTranscriptView, true
	}
	pane := newLiveTextPane(query)
	pane.rowsOnly = true
	return &LiveTextStream{loop: l, pane: pane}
}

// WriteBackendError writes a transcript backend failure to the HTTP response.
func (l *LiveTextLoop) writeBackendError(w http.ResponseWriter, err error) {
	threadindex.WriteTranscriptBackendError(w, err)
}

// ConversationItems reads a bounded page of conversation items.
func (l *LiveTextLoop) conversationItems(ctx context.Context, view liveview.View, before string, limit int) ([]liveview.Item, *liveview.Page, string, error) {
	catalog, ok := l.sources.Catalog, l.sources.Catalog != nil
	if !ok {
		return nil, nil, "", errors.New("catalog unavailable")
	}
	threadID, err := l.catalogThreadID()
	if errors.Is(err, errLiveTextNotIndexed) {
		return []liveview.Item{}, &liveview.Page{}, "", nil
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
		return []liveview.Item{}, &liveview.Page{}, "", nil
	}
	if err != nil {
		return nil, nil, "", err
	}
	items := make([]liveview.Item, 0, len(response.Items))
	for _, row := range response.Items {
		items = append(items, savedConversationItem(row))
	}
	page := &liveview.Page{HasMoreBefore: response.Page.HasMoreBefore, NextBeforeCursor: response.Page.NextBeforeCursor, Source: "file"}
	return items, page, response.Projection.Key, nil
}

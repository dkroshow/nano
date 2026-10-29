package sessions

import (
	"context"
	"errors"
	"net/http"

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
func (s *LiveTextStream) DoneChannel() <-chan struct{} { return s.pane.done }

// ReadyChannel signals that pane bytes are available.
func (s *LiveTextStream) ReadyChannel() <-chan struct{} { return s.pane.ready }

// Take takes the pane’s pending encoded bytes.
func (s *LiveTextStream) Take() [][]byte { return s.pane.take() }

// RequestPane creates a pane for the validated request parameters.
func (l *LiveTextLoop) RequestPane(r *http.Request) (*LiveTextStream, error) {
	query, err := threadindex.ParseTranscriptListQuery(r)
	if err != nil {
		return nil, err
	}
	return &LiveTextStream{loop: l, pane: newLiveTextPane(query)}, nil
}

// RowsPane creates a saved-row pane for a conversation view.
func (l *LiveTextLoop) RowsPane(view conversationView) *LiveTextStream {
	query := threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 1}
	if view == conversationViewFull {
		query.View, query.HiddenSpecified = threadindex.DevThreadTranscriptView, true
	}
	pane := newLiveTextPane(query)
	pane.rowsOnly = true
	return &LiveTextStream{loop: l, pane: pane}
}

// WriteBackendError writes a transcript backend failure to the HTTP response.
func (l *LiveTextLoop) WriteBackendError(w http.ResponseWriter, err error) {
	threadindex.WriteTranscriptBackendError(w, err)
}

// ConversationItems reads a bounded page of conversation items.
func (l *LiveTextLoop) ConversationItems(ctx context.Context, view conversationView, before string, limit int) ([]liveProjectionItem, *liveProjectionPage, string, error) {
	catalog, ok := l.sources.Transcripts, l.sources.Transcripts != nil
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

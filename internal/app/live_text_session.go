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

func (s *liveTextStream) start()                        { s.loop.subscribe(s.pane) }
func (s *liveTextStream) stop()                         { s.loop.unsubscribe(s.pane) }
func (s *liveTextStream) doneChannel() <-chan struct{}  { return s.pane.done }
func (s *liveTextStream) readyChannel() <-chan struct{} { return s.pane.ready }
func (s *liveTextStream) take() [][]byte                { return s.pane.take() }

func (l *liveTextLoop) requestPane(r *http.Request) (sessionPane, error) {
	query, err := parseTranscriptListQuery(r)
	if err != nil {
		return nil, err
	}
	return &liveTextStream{loop: l, pane: newLiveTextPane(query)}, nil
}

func (l *liveTextLoop) rowsPane(view conversationView) sessionPane {
	query := transcriptListQuery{View: defaultThreadTranscriptView, Limit: 1}
	if view == conversationViewFull {
		query.View, query.HiddenSpecified = devThreadTranscriptView, true
	}
	pane := newLiveTextPane(query)
	pane.rowsOnly = true
	return &liveTextStream{loop: l, pane: pane}
}

func (l *liveTextLoop) writeBackendError(w http.ResponseWriter, err error) {
	writeTranscriptBackendError(w, err)
}

func (l *liveTextLoop) conversationItems(ctx context.Context, view conversationView, before string, limit int) ([]liveProjectionItem, *liveProjectionPage, string, error) {
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

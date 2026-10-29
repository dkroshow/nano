package app

import (
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
)

type liveTextStatus = liveview.Status

// Integration tests decode the stream's wire values independently.
type liveTextEntry struct {
	ID                string   `json:"id"`
	Kind              string   `json:"kind"`
	Text              string   `json:"text"`
	State             string   `json:"state,omitempty"`
	ActionID          string   `json:"actionId,omitempty"`
	SenderSubmittedAt string   `json:"senderSubmittedAt,omitempty"`
	AllowedActions    []string `json:"allowedActions,omitempty"`
	Label             string   `json:"label,omitempty"`
}
type liveTextMessage struct {
	Type       string                            `json:"type"`
	Ref        string                            `json:"ref,omitempty"`
	Refs       []string                          `json:"refs,omitempty"`
	ThreadID   string                            `json:"threadId,omitempty"`
	Key        string                            `json:"key,omitempty"`
	Rows       []threadindex.TranscriptItem      `json:"rows,omitempty"`
	Removed    []string                          `json:"removed,omitempty"`
	Turns      []threadindex.TranscriptTurn      `json:"turns,omitempty"`
	Page       *threadindex.TranscriptPageInfo   `json:"page,omitempty"`
	Projection *threadindex.TranscriptProjection `json:"projection,omitempty"`
	Live       *[]liveTextEntry                  `json:"live,omitempty"`
	Saved      []string                          `json:"saved,omitempty"`
	ID         string                            `json:"id,omitempty"`
	Append     string                            `json:"append,omitempty"`
	Error      string                            `json:"error,omitempty"`
	Status     *liveview.Status                  `json:"status,omitempty"`
}

const liveTextWarmDelay = time.Second
const projectionTurnUpsert = liveview.TurnUpsertOp

type liveTextPane struct {
	*sessions.LiveTextStream
	query    threadindex.TranscriptQuery
	rowsOnly bool
	ready    <-chan struct{}
	done     <-chan struct{}
}

func newLiveTextPane(q threadindex.TranscriptQuery) *liveTextPane { return &liveTextPane{query: q} }
func (p *liveTextPane) take() [][]byte                            { return p.Take() }
func (p *liveTextPane) isClosed() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

type testLiveTextLoop struct{ *sessions.LiveTextLoop }

func liveTextOf(s *session) *testLiveTextLoop { return &testLiveTextLoop{s.LiveTextService()} }
func (l *testLiveTextLoop) subscribe(p *liveTextPane) {
	if p.rowsOnly {
		p.LiveTextStream = l.RowsPane(liveview.Messages)
	} else {
		q := url.Values{"view": {p.query.View}}
		if p.query.Limit > 0 {
			q.Set("limit", strconv.Itoa(p.query.Limit))
		}
		if p.query.Before != "" {
			q.Set("before", p.query.Before)
		}
		if p.query.HiddenSpecified {
			q.Set("hide", strings.Join(p.query.HiddenCategories, ","))
		}
		var err error
		p.LiveTextStream, err = l.RequestPane(httptest.NewRequest("GET", "/?"+q.Encode(), nil))
		if err != nil {
			panic(err)
		}
	}
	p.ready, p.done = p.ReadyChannel(), p.DoneChannel()
	p.Start()
}
func (l *testLiveTextLoop) unsubscribe(p *liveTextPane) { p.Stop() }
func newLiveTextLoop(s *server, sess *session) *sessions.LiveTextLoop {
	if sess != nil {
		s.startLiveText(sess)
		return sess.LiveTextService()
	}
	return sessions.NewLiveTextLoop(sessions.LiveTextSources{}, nil)
}

const liveTextSaveWindow = 20 * time.Second
const liveTextPaneBacklog = 1 << 20

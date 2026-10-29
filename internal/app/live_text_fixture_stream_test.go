package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
)

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

// liveTextPane receives the real session HTTP stream. Status has its own
// tests; this fixture queues only the row/live messages the pane applies.
type liveTextPane struct {
	query   threadindex.TranscriptQuery
	ready   chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
	mu      sync.Mutex
	queued  [][]byte
	partial []byte
	header  http.Header
}

func newLiveTextPane(q threadindex.TranscriptQuery) *liveTextPane {
	return &liveTextPane{query: q, ready: make(chan struct{}, 1), done: make(chan struct{}), header: make(http.Header)}
}
func (p *liveTextPane) Header() http.Header { return p.header }
func (p *liveTextPane) WriteHeader(int)     {}
func (p *liveTextPane) Flush()              {}
func (p *liveTextPane) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.partial = append(p.partial, data...)
	for {
		end := bytes.Index(p.partial, []byte("\n\n"))
		if end < 0 {
			break
		}
		frame := string(p.partial[:end])
		p.partial = append([]byte(nil), p.partial[end+2:]...)
		if raw, ok := strings.CutPrefix(frame, "data: "); ok {
			var head struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(raw), &head); err != nil {
				return 0, err
			}
			if head.Type != "status" {
				p.queued = append(p.queued, []byte(raw))
				select {
				case p.ready <- struct{}{}:
				default:
				}
			}
		}
	}
	return len(data), nil
}
func (p *liveTextPane) take() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.queued
	p.queued = nil
	select {
	case <-p.ready:
	default:
	}
	return out
}
func (p *liveTextPane) isClosed() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}
func (p *liveTextPane) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

type testLiveTextLoop struct{ sess *sessions.Session }

func liveTextOf(s *sessions.Session) *testLiveTextLoop { return &testLiveTextLoop{sess: s} }
func (l *testLiveTextLoop) subscribe(p *liveTextPane) {
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
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go func() {
		defer close(p.done)
		l.sess.View().HandleLiveStream(p, httptest.NewRequest("GET", "/?"+q.Encode(), nil).WithContext(ctx))
	}()
}
func (l *testLiveTextLoop) unsubscribe(p *liveTextPane) { p.Stop(); <-p.done }
func newLiveTextLoop(s *server, sess *sessions.Session) *sessions.LiveTextLoop {
	if sess != nil {
		s.startLiveText(sess)
		return sess.View().LiveTextService()
	}
	return sessions.NewLiveTextLoop(sessions.LiveTextSources{}, nil)
}

const liveTextSaveWindow = 20 * time.Second
const liveTextPaneBacklog = 1 << 20

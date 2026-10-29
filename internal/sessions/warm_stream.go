package sessions

import "bytes"

// NewPane connects one warm member to the session's existing ordered loop.
func (s *WarmStream) NewPane(ref string, loop *LiveTextLoop, marker WarmMarker) *LiveTextStream {
	p := newLiveTextPane(warmPaneQuery())
	p.rowsOnly, p.warm, p.ref, p.reads, p.fill = true, true, ref, &s.reads, s.fill
	if marker.Key != "" {
		p.marker = &marker
	}
	p.liveTextQueue = s.queue
	return &LiveTextStream{loop: loop, pane: p}
}

// IsClosed reports that a member must be removed from its warm stream.
func (s *LiveTextStream) IsClosed() bool { return s.pane.isClosed() }

// ReadyChannel reports pending bytes on the shared stream queue.
func (s *WarmStream) ReadyChannel() <-chan struct{} { return s.queue.ready }

// DoneChannel closes when the shared queue exceeds its backlog.
func (s *WarmStream) DoneChannel() <-chan struct{} { return s.queue.done }

// Take removes the next ordered batch of encoded warm messages.
func (s *WarmStream) Take() [][]byte { return s.queue.take() }

// SetRefs records the number of members retained by the browser stream.
func (s *WarmStream) SetRefs(n int) { s.refs.Store(int64(n)) }

// RecordWrite records the bytes and tail count actually written to the browser.
func (s *WarmStream) RecordWrite(raw []byte, n int) {
	s.bytes.Add(int64(n))
	if bytes.HasPrefix(raw, []byte(`{"type":"tail"`)) {
		s.tails.Add(1)
		s.tailBytes.Add(int64(n))
	}
}

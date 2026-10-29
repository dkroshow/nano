package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/sessions"
)

// handleWarmTranscripts streams the set's saved rows. Messages are the
// file-only pane messages (tail, update, error) tagged with ref, and set,
// which names the threads carried: the browser drops any other. A thread
// joining with a marker at the current projection gets an update with the
// rows it lacks; any other joining thread gets a tail; one that cannot be
// warmed gets one error, and is tried again when the sidebar changes.
func (s *server) handleWarmTranscripts(w http.ResponseWriter, r *http.Request) {
	request, err := sessions.ParseWarmRequest(r)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpapi.WriteHTTPError(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	var sidebarEvents <-chan sidebarDelta
	if s.sidebar != nil {
		ch := s.sidebar.events.subscribe()
		defer s.sidebar.events.unsubscribe(ch)
		sidebarEvents = ch
	}
	stream := s.warm.Open(request.Fill)
	defer s.warm.Close(stream)
	type member struct {
		sess     *sessions.Session
		pane     *sessions.LiveTextStream
		joining  bool // its first message is not written yet
		joinedAt time.Time
	}
	members := make(map[string]member)
	leave := func(ref string, m member) {
		m.pane.Stop()
		s.warm.Release(ref)
		delete(members, ref)
	}
	defer func() {
		for ref, m := range members {
			leave(ref, m)
		}
	}()
	told := make(map[string]bool)          // threads this stream has sent an error for
	failedAt := make(map[string]time.Time) // threads whose pane failed, and when
	var sent []string                      // the set the browser last received

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	write := func(raw []byte) bool {
		n, err := w.Write(append(append([]byte("data: "), raw...), '\n', '\n'))
		stream.RecordWrite(raw, n)
		return err == nil
	}
	writeMessage := func(message sessions.LiveTextMessage) bool {
		raw, err := json.Marshal(message)
		return err == nil && write(raw)
	}
	// update moves the stream toward the set: a member whose session was
	// replaced rejoins on its new loop, a failed one leaves and is tried
	// again later, a few threads join at a time in set order, and the rest
	// stay carried until a slot frees. It reports how long until something
	// it is waiting on (a joining thread's slot, a failed thread's retry)
	// needs it again, zero for nothing, and whether the writes succeeded.
	update := func() (wake time.Duration, ok bool) {
		now := time.Now()
		soonest := func(d time.Duration) {
			if d > 0 && (wake == 0 || d < wake) {
				wake = d
			}
		}
		for ref, m := range members {
			switch {
			case m.pane.IsClosed():
				// Its own error went out when it failed; the browser dropped
				// what it held, so a retry starts from nothing.
				failedAt[ref], told[ref] = now, true
				delete(request.Markers, ref)
				leave(ref, m)
			case s.SessionForHostedThread(ref) != m.sess:
				leave(ref, m)
			}
		}
		joining := 0
		for _, m := range members {
			if m.joining && now.Sub(m.joinedAt) < sessions.WarmJoinWait {
				joining++
				soonest(sessions.WarmJoinWait - now.Sub(m.joinedAt))
			}
		}
		carried := make([]string, 0, len(request.Set))
		for _, ref := range request.Set {
			if _, ok := members[ref]; ok || joining >= sessions.WarmJoining {
				carried = append(carried, ref)
				continue
			}
			if failed, ok := failedAt[ref]; ok && now.Sub(failed) < sessions.WarmRetryWait {
				soonest(sessions.WarmRetryWait - now.Sub(failed))
				continue
			}
			sess := s.SessionForHostedThread(ref)
			reason := ""
			switch {
			case sess == nil || sess.LiveTextService() == nil:
				reason = "no live session holds this thread"
			case !s.warm.Admit(ref):
				reason = "the warm budget is spent"
			}
			if reason != "" {
				// The browser drops the thread on the error, so a later
				// retry sends a tail, not an update against its marker.
				delete(request.Markers, ref)
				if !told[ref] {
					told[ref] = true
					if !writeMessage(sessions.LiveTextMessage{Type: "error", Ref: ref, Error: reason}) {
						return 0, false
					}
				}
				continue
			}
			pane := stream.NewPane(ref, sess.LiveTextService(), request.Markers[ref])
			members[ref] = member{sess: sess, pane: pane, joining: true, joinedAt: now}
			joining++
			soonest(sessions.WarmJoinWait)
			pane.Start()
			carried = append(carried, ref)
		}
		stream.SetRefs(len(members))
		if sent != nil && slices.Equal(carried, sent) {
			return wake, true
		}
		sent = carried
		return wake, writeMessage(sessions.LiveTextMessage{Type: "set", Refs: carried})
	}
	// joined notes a thread's first message written, which frees its slot.
	joined := func(raw []byte) bool {
		var head struct {
			Ref string `json:"ref"`
		}
		if json.Unmarshal(raw, &head) != nil {
			return false
		}
		m, ok := members[head.Ref]
		if !ok || !m.joining {
			return false
		}
		m.joining = false
		members[head.Ref] = m
		return true
	}
	// wake brings the stream back to update when a wait runs out: a joining
	// thread's slot, or a failed thread's retry.
	var wake <-chan time.Time
	settle := func() bool {
		in, ok := update()
		if in > 0 {
			wake = time.After(in)
		} else {
			wake = nil
		}
		flusher.Flush()
		return ok
	}
	if !settle() {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-stream.DoneChannel():
			return // the backlog overflowed; the browser reconnects with its markers
		case <-stream.ReadyChannel():
			// A first message frees a joining slot; an error means a pane
			// failed and leaves on the next update.
			resettle := false
			for _, raw := range stream.Take() {
				if !write(raw) {
					return
				}
				resettle = joined(raw) || bytes.HasPrefix(raw, []byte(`{"type":"error"`)) || resettle
			}
			flusher.Flush()
			if resettle && !settle() {
				return
			}
		case <-wake:
			if !settle() {
				return
			}
		case _, ok := <-sidebarEvents:
			if !ok {
				sidebarEvents = nil
				continue
			}
			if !settle() {
				return
			}
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

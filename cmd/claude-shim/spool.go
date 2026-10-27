// spool.go — the per-session event spool: every stdio line the shim observes,
// in both directions, is appended as sequenced NDJSON so a client that was
// away (nano restarting) can replay exactly the gap it missed.
//
// Adapted from nano's wire_spool.go — single writer goroutine, bounded queue
// with an append timeout, sticky first error — with two deliberate
// differences:
//
//   - The directory is durable (~/.nanod/claude-shim/spools), not
//     os.TempDir. Spools survive shim boot so Nano's `--resume` fallback can
//     continue the prior sequence instead of minting duplicate turn ids.
//   - Readers can force a flush barrier (readAfter does). Without it the
//     writer goroutine's buffered tail would be invisible to an attaching
//     client, which is exactly the gap the replay/live boundary must not have.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	spoolAppendTimeout = 250 * time.Millisecond
	spoolFlushInterval = 250 * time.Millisecond
	spoolChanSize      = 4096
	spoolScanInitial   = 1024 * 1024
	spoolScanMax       = 64 * 1024 * 1024
)

// Direction labels, matching nano's protocol.Direction values: a line the
// child wrote to stdout is "recv", a line we wrote to its stdin is "send".
const (
	dirSend = "send"
	dirRecv = "recv"
)

// spoolEvent is one spooled stdio line. Seq is session-scoped, assigned by the
// shim, monotonic from 1, and covers both directions.
type spoolEvent struct {
	Seq    int64  `json:"seq"`
	TS     string `json:"ts"`
	Dir    string `json:"dir"`
	Line   string `json:"line"`
	TurnID string `json:"turnId,omitempty"`
}

// spoolItem is either an event to write or, when done is non-nil, a flush
// barrier: the writer flushes everything queued ahead of it, then closes done.
type spoolItem struct {
	evt  spoolEvent
	done chan struct{}
}

type spool struct {
	path          string
	turnIDSession string
	lastSeq       int64
	ch            chan spoolItem
	wg            sync.WaitGroup

	mu     sync.Mutex
	closed bool
	err    error
}

// prepareSpoolDir keeps prior spools: their sequence is the durable turn-id
// source when Nano resumes a child after the shim process restarts.
func prepareSpoolDir(dir string) error {
	return os.MkdirAll(dir, 0700)
}

func newSpool(dir, sessionID string) (*spool, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	lastSeq, err := readSpoolLastSeq(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	sp := &spool{
		path: path, turnIDSession: sessionID, lastSeq: lastSeq,
		ch: make(chan spoolItem, spoolChanSize),
	}
	sp.wg.Add(1)
	go sp.writeLoop(f)
	return sp, nil
}

func readSpoolLastSeq(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	var lastSeq int64
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, spoolScanInitial), spoolScanMax)
	for scanner.Scan() {
		var evt spoolEvent
		if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil {
			return 0, err
		}
		if evt.Seq > lastSeq {
			lastSeq = evt.Seq
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return lastSeq, nil
}

func (sp *spool) append(evt spoolEvent) error {
	return sp.enqueue(spoolItem{evt: evt})
}

// readAfter returns every spooled event with Seq > fromSeq, in file order. It
// flushes first, so the caller sees every event queued before the call — the
// no-gap half of the replay guarantee. (The no-duplicate half is the caller's:
// hold the session lock across readAfter and taking the attachment.)
func (sp *spool) readAfter(fromSeq int64) ([]spoolEvent, error) {
	if err := sp.flush(); err != nil {
		return nil, err
	}
	f, err := os.Open(sp.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []spoolEvent
	activeTurnID := ""
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, spoolScanInitial), spoolScanMax)
	for scanner.Scan() {
		var evt spoolEvent
		if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil {
			sp.setErr(err)
			continue
		}
		assignSpoolTurnID(sp.turnIDSession, &evt, &activeTurnID)
		if evt.Seq <= fromSeq {
			continue
		}
		out = append(out, evt)
	}
	if err := scanner.Err(); err != nil {
		sp.setErr(err)
		return nil, err
	}
	return out, nil
}

// assignSpoolTurnID is shared by live writes and replay. A legacy spool has
// no turnId fields, but its submitted user frame still has the durable
// session sequence that originally identified the turn.
func assignSpoolTurnID(sessionID string, evt *spoolEvent, activeTurnID *string) {
	var probe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(evt.Line), &probe)
	if evt.Dir == dirSend && probe.Type == "user" {
		if evt.TurnID == "" {
			evt.TurnID = formatTurnID(sessionID, evt.Seq)
		}
		*activeTurnID = evt.TurnID
	} else if evt.TurnID == "" {
		evt.TurnID = *activeTurnID
	} else {
		*activeTurnID = evt.TurnID
	}
	if evt.Dir == dirRecv && probe.Type == "result" {
		*activeTurnID = ""
	}
}

func (sp *spool) close(deleteFile bool) error {
	sp.mu.Lock()
	if !sp.closed {
		sp.closed = true
		close(sp.ch)
	}
	sp.mu.Unlock()

	sp.wg.Wait()
	if deleteFile {
		if err := os.Remove(sp.path); err != nil && !os.IsNotExist(err) {
			sp.setErr(err)
		}
	}
	return sp.currentErr()
}

// flush blocks until the writer has committed everything queued so far.
func (sp *spool) flush() error {
	done := make(chan struct{})
	if err := sp.enqueue(spoolItem{done: done}); err != nil {
		return err
	}
	<-done
	return nil
}

func (sp *spool) enqueue(item spoolItem) error {
	timer := time.NewTimer(spoolAppendTimeout)
	defer timer.Stop()

	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.closed {
		if sp.err != nil {
			return sp.err
		}
		return errors.New("spool is closed")
	}

	select {
	case sp.ch <- item:
		return nil
	case <-timer.C:
		err := errors.New("spool queue full")
		if sp.err == nil {
			sp.err = err
		}
		return err
	}
}

func (sp *spool) writeLoop(f *os.File) {
	defer sp.wg.Done()
	defer f.Close()

	bw := bufio.NewWriter(f)
	ticker := time.NewTicker(spoolFlushInterval)
	defer ticker.Stop()

	flush := func() {
		if err := bw.Flush(); err != nil {
			sp.setErr(err)
		}
	}
	for {
		select {
		case item, ok := <-sp.ch:
			if !ok {
				flush()
				return
			}
			if item.done != nil {
				flush()
				close(item.done)
				continue
			}
			b, err := json.Marshal(item.evt)
			if err != nil {
				sp.setErr(err)
				continue
			}
			if _, err := bw.Write(b); err != nil {
				sp.setErr(err)
				continue
			}
			if err := bw.WriteByte('\n'); err != nil {
				sp.setErr(err)
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (sp *spool) setErr(err error) {
	if err == nil {
		return
	}
	sp.mu.Lock()
	if sp.err == nil {
		sp.err = err
	}
	sp.mu.Unlock()
}

func (sp *spool) currentErr() error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.err
}

package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	wireSpoolAppendTimeout = 250 * time.Millisecond
	wireSpoolFlushInterval = 250 * time.Millisecond
	wireSpoolChanSize      = 256
)

type wireSpool struct {
	path string
	ch   chan wireEvent
	wg   sync.WaitGroup

	mu     sync.Mutex
	closed bool
	err    error
}

func prepareWireSpoolDir(base string) (string, error) {
	if err := os.RemoveAll(base); err != nil {
		return "", err
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		return "", err
	}
	return base, nil
}

func newWireSpool(baseDir, sessionID string) (*wireSpool, error) {
	if err := os.MkdirAll(baseDir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(baseDir, sessionID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	sp := &wireSpool{
		path: path,
		ch:   make(chan wireEvent, wireSpoolChanSize),
	}
	sp.wg.Add(1)
	go sp.writeLoop(f)
	return sp, nil
}

func (sp *wireSpool) append(evt wireEvent) error {
	timer := time.NewTimer(wireSpoolAppendTimeout)
	defer timer.Stop()

	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.closed {
		err := sp.err
		if err == nil {
			err = fmt.Errorf("wire spool is closed")
		}
		return err
	}

	select {
	case sp.ch <- evt:
		return nil
	case <-timer.C:
		err := fmt.Errorf("wire spool queue full")
		if sp.err == nil {
			sp.err = err
		}
		return err
	}
}

func (sp *wireSpool) close(deleteFile bool) error {
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

func (sp *wireSpool) readBefore(beforeSeq int64, limit int) ([]wireEvent, bool, error) {
	if limit <= 0 {
		return nil, false, nil
	}
	f, err := os.Open(sp.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()

	var out []wireEvent
	seenBefore := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var evt wireEvent
		if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil {
			sp.setErr(err)
			continue
		}
		if evt.Seq >= beforeSeq {
			continue
		}
		seenBefore++
		out = append(out, evt)
		if len(out) > limit {
			copy(out, out[1:])
			out = out[:limit]
		}
	}
	if err := scanner.Err(); err != nil {
		sp.setErr(err)
		return out, false, err
	}
	return out, seenBefore > len(out), nil
}

func (sp *wireSpool) writeLoop(f *os.File) {
	defer sp.wg.Done()
	defer f.Close()

	bw := bufio.NewWriter(f)
	ticker := time.NewTicker(wireSpoolFlushInterval)
	defer ticker.Stop()

	flush := func() {
		if err := bw.Flush(); err != nil {
			sp.setErr(err)
		}
	}
	for {
		select {
		case evt, ok := <-sp.ch:
			if !ok {
				flush()
				return
			}
			b, err := json.Marshal(evt)
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

func (sp *wireSpool) setErr(err error) {
	if err == nil {
		return
	}
	sp.mu.Lock()
	if sp.err == nil {
		sp.err = err
	}
	sp.mu.Unlock()
}

func (sp *wireSpool) currentErr() error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.err
}

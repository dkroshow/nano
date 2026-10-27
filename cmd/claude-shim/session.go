// session.go — one owned child process and everything sequenced around it:
// spawn from {bin, args, cwd}, stdout read loop, stdin writes, sequence
// assignment, the native-session-id probe, exit reaping and graceful close.
//
// The session mutex is the ordering authority. Sequence assignment, the spool
// append, the attachment, and the notification to the attached client all
// happen under it, so an attaching client can snapshot the spool and take the
// attachment atomically: no line can slip between the replay and the live
// stream, and none can be delivered twice.
//
// No protocol translation happens here. Lines are framed verbatim in both
// directions; the only line inspection is the minimal init probe that learns
// the child's native session id (the analog of nano's captureInit).
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"
)

const (
	// closeGrace mirrors nano's claudeCloseGrace: closing stdin asks
	// `claude -p` to exit cleanly (flushing its native transcript); kill it
	// if it lingers past the grace period.
	closeGrace = 3 * time.Second

	// The child's stream-json lines can be very large (image payloads, big
	// tool results) — same buffer sizing nano's read loop uses.
	childScanInitial = 1024 * 1024
	childScanMax     = 64 * 1024 * 1024

	stateRunning = "running"
	stateExited  = "exited"

	shimIDPrefix    = "cs_"
	shimIDRandomLen = 8 // bytes → 16 lowercase hex chars
	exitCodeUnknown = -1

	// Every child this daemon owns is one of nano's, so it signs them all.
	// claude CLI 2.1.220 honors CLAUDE_CODE_ENTRYPOINT and writes the value
	// into its transcript rows as `entrypoint`; unsigned children read
	// `sdk-cli`, which nano's catalog classifies programmatic and the Thread
	// Explorer hides by default — operator work vanishing the moment its
	// session is closed. Nano's direct-spawn fallback stamps the same pair
	// (claude.go); this daemon stays dependency-free, so the literals are
	// repeated rather than imported, and both sides' tests assert the value.
	entrypointEnv  = "CLAUDE_CODE_ENTRYPOINT"
	nanoEntrypoint = "nano"
)

var (
	errSessionExited       = errors.New("session_exited")
	errSessionAttached     = errors.New("session_attached")
	errReplayClientDropped = errors.New("replay_client_dropped")
)

// sessionInfo is the session/list row shape (also the shape of the fields an
// attach result reports back).
type sessionInfo struct {
	ShimSessionID   string `json:"shimSessionId"`
	NativeSessionID string `json:"nativeSessionId"`
	Cwd             string `json:"cwd"`
	State           string `json:"state"`
	TurnActive      bool   `json:"turnActive"`
	LastSeq         int64  `json:"lastSeq"`
	Attached        bool   `json:"attached"`
	StartedAt       string `json:"startedAt"`
}

type shimSession struct {
	id            string
	turnIDSession string
	// bin and args are the spawn posture, recorded but not served: the
	// `--resume` fallback needs them to restore a session's original flags
	// rather than silently downgrading it (see the plan's attachments notes).
	bin       string
	args      []string
	cwd       string
	startedAt time.Time

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	spool  *spool
	reaped chan struct{} // closed once the child has been waited on

	mu           sync.Mutex
	seq          int64
	nativeID     string
	state        string
	turnActive   bool
	activeTurnID string
	exitCode     int
	attached     *conn
	closed       bool // session/close ran: no more input, no more notifications
}

// manager owns the live session records. Sessions are retained after their
// child exits (state "exited", spool intact) until an explicit session/close,
// so a client that re-attaches after being away learns the child died and can
// replay what it missed first.
type manager struct {
	spoolDir string

	mu       sync.Mutex
	sessions map[string]*shimSession
}

func newManager(spoolDir string) *manager {
	return &manager{spoolDir: spoolDir, sessions: map[string]*shimSession{}}
}

// start spawns a child and registers it with owner already attached, but does
// not read from it yet: the caller answers session/start first and then calls
// beginReading, so the starting connection has the shim session id before the
// child's first line is notified — and cannot miss that line either.
func (m *manager) start(bin string, args []string, cwd, turnIDSession string, owner *conn) (*shimSession, error) {
	id, err := newShimSessionID()
	if err != nil {
		return nil, err
	}
	if turnIDSession == "" {
		turnIDSession = id
	}
	sp, err := newSpool(m.spoolDir, turnIDSession)
	if err != nil {
		return nil, err
	}
	s := &shimSession{
		id:            id,
		turnIDSession: turnIDSession,
		bin:           bin,
		args:          args,
		cwd:           cwd,
		startedAt:     time.Now(),
		spool:         sp,
		reaped:        make(chan struct{}),
		seq:           sp.lastSeq,
		state:         stateRunning,
		exitCode:      exitCodeUnknown,
		attached:      owner,
	}

	cmd, stdin, stdout, err := spawnChild(bin, args, cwd)
	if err != nil {
		sp.close(false)
		return nil, err
	}
	s.cmd, s.stdin, s.stdout = cmd, stdin, stdout

	// Register before tracking the attachment: the lock order across the
	// daemon is manager → session → connection, and inverting it here would
	// close a deadlock cycle with byNativeID and attach.
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	if !owner.trackAttached(s) {
		s.detach(owner) // the client dropped mid-start; leave it attachable
	}

	log.Printf("session %s started: %s %v (cwd %s)", id, bin, args, cwd)
	return s, nil
}

func spawnChild(bin string, args []string, cwd string) (*exec.Cmd, io.WriteCloser, io.Reader, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	cmd.Stderr = os.Stderr // child diagnostics land in the shim's log
	// Appended, so it overrides an entrypoint the daemon itself inherited:
	// exec keeps the last occurrence of a duplicated key.
	cmd.Env = append(os.Environ(), entrypointEnv+"="+nanoEntrypoint)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("starting %s: %w", bin, err)
	}
	return cmd, stdin, stdout, nil
}

func (s *shimSession) beginReading() { go s.readLoop(s.stdout) }

func (m *manager) byShimID(id string) *shimSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *manager) byNativeID(nativeID string) *shimSession {
	if nativeID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.native() == nativeID {
			return s
		}
	}
	return nil
}

// take removes a session from the registry and returns it, so a concurrent
// attach cannot land on a session that is being closed.
func (m *manager) take(id string) *shimSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	delete(m.sessions, id)
	return s
}

func (m *manager) infos() []sessionInfo {
	m.mu.Lock()
	sessions := make([]*shimSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	out := make([]sessionInfo, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.info())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt != out[j].StartedAt {
			return out[i].StartedAt < out[j].StartedAt
		}
		return out[i].ShimSessionID < out[j].ShimSessionID
	})
	return out
}

func newShimSessionID() (string, error) {
	buf := make([]byte, shimIDRandomLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return shimIDPrefix + hex.EncodeToString(buf), nil
}

func (s *shimSession) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, childScanInitial), childScanMax)
	for scanner.Scan() {
		// Text() copies: the scanner reuses its buffer, and the line outlives
		// this iteration in the spool queue and the notification.
		s.record(dirRecv, scanner.Text(), true)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("session %s read loop: %v", s.id, err)
	}
	// Stdout is drained; safe to reap. Wait exactly once, here.
	s.cmd.Wait()
	code := exitCodeUnknown
	if s.cmd.ProcessState != nil {
		code = s.cmd.ProcessState.ExitCode()
	}
	s.recordExit(code)
	close(s.reaped)
}

func (s *shimSession) recordExit(code int) {
	s.mu.Lock()
	s.state = stateExited
	s.exitCode = code
	if !s.closed && s.attached != nil {
		s.attached.notify(methodExited, exitedParams{ShimSessionID: s.id, ExitCode: code})
	}
	s.mu.Unlock()
	log.Printf("session %s exited: code %d", s.id, code)
}

// record assigns the next sequence number, spools the line, and — for lines
// the attached client has not already seen — notifies it. All under the
// session lock: see the file comment on ordering.
func (s *shimSession) record(dir, line string, notify bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordLocked(dir, line, notify)
}

func (s *shimSession) recordLocked(dir, line string, notify bool) string {
	if dir == dirRecv && s.nativeID == "" {
		s.nativeID = probeNativeID(line)
	}
	s.seq++
	if active, ok := claudeTurnActivityTransition(dir, line); ok {
		s.turnActive = active
	}
	evt := spoolEvent{Seq: s.seq, TS: time.Now().Format(time.RFC3339Nano), Dir: dir, Line: line}
	assignSpoolTurnID(s.turnIDSession, &evt, &s.activeTurnID)
	if err := s.spool.append(evt); err != nil {
		log.Printf("session %s spool append: %v", s.id, err)
	}
	if notify && s.attached != nil {
		s.attached.notify(methodLine, lineParams{ShimSessionID: s.id, Seq: evt.Seq, TS: evt.TS, Dir: evt.Dir, Line: evt.Line, TurnID: evt.TurnID})
	}
	return evt.TurnID
}

func formatTurnID(sessionID string, promptSeq int64) string {
	return fmt.Sprintf("claude:%s:%d", sessionID, promptSeq)
}

func claudeTurnActivityTransition(dir, line string) (active, ok bool) {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(line), &probe) != nil {
		return false, false
	}
	if dir == dirSend && probe.Type == "user" {
		return true, true
	}
	if dir != dirRecv {
		return false, false
	}
	if probe.Type == "result" {
		return false, true
	}
	switch probe.Type {
	case "assistant", "stream_event", "user":
		return true, true
	default:
		return false, false
	}
}

// writeInput writes the line to the child, then spools it as accepted evidence.
// It is not notified back to the attached connection: whoever sent it already
// has it. A failed stdin write must not leave a replayable user message for a
// future Nano attachment — intent is not delivery.
func (s *shimSession) writeInputWithTurnID(line string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.state != stateRunning {
		return "", errSessionExited
	}
	if _, err := s.stdin.Write([]byte(line + "\n")); err != nil {
		return "", err
	}
	return s.recordLocked(dirSend, line, false), nil
}

// attach makes c the session's live connection and replays everything after
// fromSeq to it, followed by session/replayDone and — for a child that has
// already exited — session/exited. Both the snapshot and the attachment happen
// under the session lock, which is what makes the replay/live boundary exact.
//
// onAttached is called under that same lock, before the first replayed line,
// so the caller can put the attach result on the wire ahead of the replay: a
// client attaching by native id learns the shim session id from that result
// and must have it before any notification quoting it arrives.
func (s *shimSession) attach(c *conn, fromSeq int64, onAttached func(sessionInfo)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attached != nil && s.attached != c {
		return errSessionAttached
	}
	events, err := s.spool.readAfter(fromSeq)
	if err != nil {
		return fmt.Errorf("replaying %s from seq %d: %w", s.id, fromSeq, err)
	}
	s.attached = c
	if !c.trackAttached(s) {
		s.attached = nil // the client dropped mid-attach; leave it attachable
	}

	onAttached(s.infoLocked())
	for _, evt := range events {
		if !c.notify(methodLine, lineParams{ShimSessionID: s.id, Seq: evt.Seq, TS: evt.TS, Dir: evt.Dir, Line: evt.Line, TurnID: evt.TurnID}) {
			s.attached = nil
			c.forgetAttached(s.id)
			return errReplayClientDropped
		}
	}
	if !c.notify(methodReplayDone, replayDoneParams{ShimSessionID: s.id, LastSeq: s.seq}) {
		s.attached = nil
		c.forgetAttached(s.id)
		return errReplayClientDropped
	}
	if s.state == stateExited {
		if !c.notify(methodExited, exitedParams{ShimSessionID: s.id, ExitCode: s.exitCode}) {
			s.attached = nil
			c.forgetAttached(s.id)
			return errReplayClientDropped
		}
	}
	return nil
}

func (s *shimSession) detach(c *conn) {
	s.mu.Lock()
	if s.attached == c {
		s.attached = nil
	}
	s.mu.Unlock()
}

// close ends the child: stdin closed, grace period, then kill. The spool stays
// as the durable turn-id high-water mark for a later --resume child. The
// caller must have removed the session from the manager first.
func (s *shimSession) close() {
	s.mu.Lock()
	s.closed = true
	s.attached = nil
	running := s.state == stateRunning
	s.mu.Unlock()

	if running {
		s.stdin.Close()
		select {
		case <-s.reaped:
		case <-time.After(closeGrace):
			s.cmd.Process.Kill()
			<-s.reaped
		}
	}
	if err := s.spool.close(false); err != nil {
		log.Printf("session %s spool close: %v", s.id, err)
	}
	log.Printf("session %s closed", s.id)
}

func (s *shimSession) native() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nativeID
}

func (s *shimSession) info() sessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.infoLocked()
}

func (s *shimSession) infoLocked() sessionInfo {
	return sessionInfo{
		ShimSessionID:   s.id,
		NativeSessionID: s.nativeID,
		Cwd:             s.cwd,
		State:           s.state,
		TurnActive:      s.turnActive,
		LastSeq:         s.seq,
		Attached:        s.attached != nil,
		StartedAt:       s.startedAt.Format(time.RFC3339),
	}
}

// probeNativeID returns the child's own session id if line is its
// `{"type":"system","subtype":"init",…}` announcement, else "". Same shape as
// nano's captureInit (claude.go) — the one line the shim looks inside.
func probeNativeID(line string) string {
	var probe struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal([]byte(line), &probe) != nil || probe.Type != "system" || probe.Subtype != "init" {
		return ""
	}
	return probe.SessionID
}

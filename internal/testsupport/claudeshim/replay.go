package claudeshim

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	wire "github.com/dkroshow/nano/internal/wire/claude"
)

// spoolBuilder writes a Claude conversation the way the shim spools it: one
// frame per stdio line, prompted frames carrying their provider turn id.
type SpoolBuilder struct {
	Key     string
	EventID func(string, string) string
	Seq     int64
	Frames  []Frame
	// events maps a prompted turn to the submission id its user line carries.
	Events map[string]string
}

func (b *SpoolBuilder) Add(dir, TurnID, line string) int64 {
	b.Seq++
	b.Frames = append(b.Frames, Frame{Seq: b.Seq, Dir: dir, Line: line, TurnID: TurnID})
	return b.Seq
}

func (b *SpoolBuilder) TurnID(promptSeq int64) string {
	return fmt.Sprintf("claude:%s:%d", b.Key, promptSeq)
}

// prompt spools a Nano send of action's prompt, stamped with its submission
// id as Nano stamps it, and returns its provider turn id.
func (b *SpoolBuilder) Prompt(action string) string {
	id := b.TurnID(b.Seq + 1)
	event := b.EventID(b.Key, action)
	if b.Events == nil {
		b.Events = map[string]string{}
	}
	b.Events[id] = event
	b.Add("send", id, `{"type":"user","uuid":"`+event+`","message":{"role":"user","content":[{"type":"text","text":"prompt"}]}}`)
	return id
}

func (b *SpoolBuilder) BlockStart(TurnID string, index int) {
	raw, _ := json.Marshal(map[string]any{"type": "stream_event", "event": map[string]any{
		"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "text", "text": ""}}})
	b.Add("recv", TurnID, string(raw))
}

// openMessage starts a message whose blocks are complete except the last,
// which is still streaming. It returns the message_start sequence.
func (b *SpoolBuilder) OpenMessage(TurnID, id string, blocks ...string) int64 {
	start := b.Add("recv", TurnID, StartMessage(id))
	for index, text := range blocks {
		b.BlockStart(TurnID, index)
		b.Add("recv", TurnID, TextDelta(index, text))
		if index == len(blocks)-1 {
			break
		}
		b.Add("recv", TurnID, fmt.Sprintf(`{"type":"stream_event","event":{"type":"content_block_stop","index":%d}}`, index))
		row, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": fmt.Sprintf("%s-%d", id, index),
			"message": map[string]any{"id": "msg_" + id, "role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}})
		b.Add("recv", TurnID, string(row))
	}
	return start
}

// message spools a complete one-block text message.
func (b *SpoolBuilder) Message(TurnID, id, text string) int64 {
	start := b.OpenMessage(TurnID, id, text)
	b.Add("recv", TurnID, `{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`)
	row, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": id + "-0",
		"message": map[string]any{"id": "msg_" + id, "role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}})
	b.Add("recv", TurnID, string(row))
	b.Add("recv", TurnID, `{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"}}}`)
	b.Add("recv", TurnID, MessageStop)
	return start
}

const ResultWithUsage = `{"type":"result","subtype":"success","is_error":false,"duration_ms":12,"session_id":"native-1","usage":{"input_tokens":11,"cache_read_input_tokens":5,"cache_creation_input_tokens":2,"output_tokens":7}}`

// result spools turnID's result. A prompted turn's names its prompt, as
// Claude's does; a background turn's names none.
func (b *SpoolBuilder) Result(TurnID string) int64 {
	line := ResultWithUsage
	if event := b.Events[TurnID]; event != "" {
		line = strings.Replace(line, `{"type":"result",`, `{"type":"result","user_message_uuid":"`+event+`",`, 1)
	}
	return b.Add("recv", TurnID, line)
}

// background spools finished turns no Nano prompt started until at least n
// more frames exist.
func (b *SpoolBuilder) Background(n int) {
	target := b.Seq + int64(n)
	for turn := 0; b.Seq < target; turn++ {
		b.Message("", fmt.Sprintf("bg%d", turn), fmt.Sprintf("background %d", turn))
		b.Result("")
	}
}

// spoolShim is a fake shim backed by frames it actually holds: attach replays
// exactly the frames after fromSeq, like cmd/claude-shim's readAfter.
type SpoolShim struct {
	*Server
	Mu           sync.Mutex
	Frames       []Frame
	State        string
	TurnActive   bool
	LastSeq      int64 // overrides the frames' own end when set
	BeforeAttach func()
	FromSeqs     []int64
	Replayed     int
}

func NewSpoolShim(t *testing.T, Frames []Frame) *SpoolShim {
	shim := &SpoolShim{Frames: Frames, State: "running"}
	shim.Server = NewServer(t, func(c *Conn) {
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			switch call.Method {
			case wire.MethodAttach:
				shim.Attach(c, call)
			case wire.MethodStart:
				c.Reply(call, map[string]any{"shimSessionId": "replacement"})
			default:
				c.Reply(call, nil)
			}
		}
	})
	return shim
}

func (s *SpoolShim) Attach(c *Conn, call Call) {
	var params struct {
		FromSeq int64 `json:"fromSeq"`
	}
	_ = json.Unmarshal(call.Params, &params)
	s.Mu.Lock()
	hook := s.BeforeAttach
	Frames := append([]Frame(nil), s.Frames...)
	last := s.LastSeq
	if last == 0 && len(Frames) > 0 {
		last = Frames[len(Frames)-1].Seq
	}
	State, active := s.State, s.TurnActive
	s.FromSeqs = append(s.FromSeqs, params.FromSeq)
	s.Mu.Unlock()
	if hook != nil {
		hook()
	}
	c.Reply(call, map[string]any{"shimSessionId": "shim-1", "nativeSessionId": "native-1", "state": State, "turnActive": active, "lastSeq": last})
	sent := 0
	for _, frame := range Frames {
		if frame.Seq <= params.FromSeq {
			continue
		}
		c.Notify(wire.MethodLine, map[string]any{"shimSessionId": "shim-1", "seq": frame.Seq, "ts": "2026-09-18T12:00:00Z", "dir": frame.Dir, "turnId": frame.TurnID, "line": frame.Line})
		sent++
	}
	s.Mu.Lock()
	s.Replayed = sent
	s.Mu.Unlock()
	c.Notify(wire.MethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": last})
}

func (s *SpoolShim) LastFromSeq() int64 {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if len(s.FromSeqs) == 0 {
		return -1
	}
	return s.FromSeqs[len(s.FromSeqs)-1]
}

func (s *SpoolShim) ReplayedFrames() int {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return s.Replayed
}

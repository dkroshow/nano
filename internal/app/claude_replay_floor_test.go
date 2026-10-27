package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// claudeTestFrame is one anonymized spool line in the shim's own format.
type claudeTestFrame struct {
	Seq    int64  `json:"seq"`
	TS     string `json:"ts"`
	Dir    string `json:"dir"`
	Line   string `json:"line"`
	TurnID string `json:"turnId,omitempty"`
}

func recvFrame(seq int64, line string) claudeTestFrame {
	return claudeTestFrame{Seq: seq, Dir: "recv", Line: line}
}

func sendFrame(seq int64, line string) claudeTestFrame {
	return claudeTestFrame{Seq: seq, Dir: "send", Line: line}
}

func (f claudeTestFrame) turn(id string) claudeTestFrame { f.TurnID = id; return f }

func claudeTestSpoolBytes(t *testing.T, frames ...claudeTestFrame) []byte {
	t.Helper()
	var out []byte
	for _, frame := range frames {
		if frame.TS == "" {
			frame.TS = "2026-09-18T12:00:00Z"
		}
		raw, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		out = append(append(out, raw...), '\n')
	}
	return out
}

func writeClaudeTestSpool(t *testing.T, dir, key string, frames ...claudeTestFrame) string {
	t.Helper()
	path := filepath.Join(dir, key+".jsonl")
	if err := os.WriteFile(path, claudeTestSpoolBytes(t, frames...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	testMessageStart = `{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_%s"}}}`
	testMessageStop  = `{"type":"stream_event","event":{"type":"message_stop"}}`
	testUserPrompt   = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"prompt"}]}}`
)

func messageStart(id string) string { return strings.Replace(testMessageStart, "%s", id, 1) }

func textDelta(index int, text string) string {
	raw, _ := json.Marshal(map[string]any{"type": "stream_event", "event": map[string]any{
		"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": text}}})
	return string(raw)
}

func scanTestSpool(t *testing.T, prefix string, wanted []int64, frames ...claudeTestFrame) (int64, *claudeSpoolFacts, error) {
	t.Helper()
	path := writeClaudeTestSpool(t, t.TempDir(), "spool", frames...)
	facts := newClaudeSpoolFacts(prefix, 0, wanted)
	if missing, err := scanClaudeSpool(path, facts); err != nil || missing {
		return 0, facts, errors.Join(err, map[bool]error{true: errors.New("missing")}[missing])
	}
	floor, err := facts.floor()
	return floor, facts, err
}

func TestClaudeReplayFloorFacts(t *testing.T) {
	const turn = "claude:nht_x:2"
	idle := []claudeTestFrame{
		recvFrame(1, claudeInitLine),
		sendFrame(2, testUserPrompt).turn(turn),
		recvFrame(3, messageStart("a")).turn(turn),
		recvFrame(4, textDelta(0, "hi")).turn(turn),
		recvFrame(5, testMessageStop).turn(turn),
		recvFrame(6, claudeReplayResultLine).turn(turn),
	}
	cases := []struct {
		name   string
		wanted []int64
		frames []claudeTestFrame
		want   int64
	}{
		{name: "idle spool ending in result replays nothing", frames: idle, want: 6},
		{name: "accepted prompt without terminal record replays its last message", wanted: []int64{2}, frames: append(append([]claudeTestFrame(nil), idle...),
			recvFrame(7, messageStart("bg")), recvFrame(8, testMessageStop), recvFrame(9, claudeReplayResultLine)), want: 2},
		{name: "open message replays from its start", frames: append(append([]claudeTestFrame(nil), idle...),
			recvFrame(7, messageStart("bg")), recvFrame(8, textDelta(0, "partial"))), want: 6},
		{name: "interrupted message closed by result does not pin the floor", frames: append(append([]claudeTestFrame(nil), idle...),
			recvFrame(7, messageStart("cut")), recvFrame(8, textDelta(0, "partial")), recvFrame(9, claudeReplayResultLine)), want: 9},
		{name: "a later message start keeps the earlier open one", frames: append(append([]claudeTestFrame(nil), idle...),
			recvFrame(7, messageStart("first")), recvFrame(8, messageStart("second"))), want: 6},
		{name: "unanswered request inside a message replays from that message", frames: append(append([]claudeTestFrame(nil), idle...),
			recvFrame(7, messageStart("ask")), recvFrame(8, `{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool"}}`)), want: 6},
		{name: "unanswered request after a message replays from the request", frames: append(append([]claudeTestFrame(nil), idle...),
			recvFrame(7, `{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool"}}`), recvFrame(8, `{"type":"system","subtype":"status"}`)), want: 6},
		{name: "answered request does not pin the floor", frames: append(append([]claudeTestFrame(nil), idle...),
			recvFrame(7, `{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool"}}`),
			sendFrame(8, `{"type":"control_response","response":{"subtype":"success","request_id":"r1"}}`)), want: 8},
		{name: "request retired by its own turn's result does not pin the floor", frames: append(append([]claudeTestFrame(nil), idle...),
			recvFrame(7, `{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool"}}`), recvFrame(8, claudeReplayResultLine)), want: 8},
		{name: "a different turn's result does not retire a request", frames: append(append([]claudeTestFrame(nil), idle[:1]...),
			recvFrame(2, `{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool"}}`),
			sendFrame(3, testUserPrompt).turn("claude:nht_x:3"), recvFrame(4, claudeReplayResultLine).turn("claude:nht_x:3")), want: 1},
		// A result finishes only the turn it names, so a prompt that a later
		// prompt's result follows can never be finished by a replay. Holding
		// the floor for it replayed tens of thousands of frames on every
		// restart.
		{name: "prompt answered under a later prompt's turn does not pin the floor", wanted: []int64{2}, frames: []claudeTestFrame{
			recvFrame(1, claudeInitLine), sendFrame(2, testUserPrompt).turn(turn), sendFrame(3, testUserPrompt).turn("claude:nht_x:3"),
			recvFrame(4, messageStart("a")).turn("claude:nht_x:3"), recvFrame(5, claudeReplayResultLine).turn("claude:nht_x:3")}, want: 5},
		{name: "prompt whose turn ended without a result does not pin the floor once a later turn ends", wanted: []int64{2}, frames: []claudeTestFrame{
			recvFrame(1, claudeInitLine), sendFrame(2, testUserPrompt).turn(turn), recvFrame(3, messageStart("cut")).turn(turn),
			sendFrame(4, testUserPrompt).turn("claude:nht_x:4"), recvFrame(5, messageStart("b")).turn("claude:nht_x:4"),
			recvFrame(6, testMessageStop).turn("claude:nht_x:4"), recvFrame(7, claudeReplayResultLine).turn("claude:nht_x:4")}, want: 7},
		{name: "prompt queued before any result still replays from the prompt", wanted: []int64{2}, frames: []claudeTestFrame{
			recvFrame(1, claudeInitLine), sendFrame(2, testUserPrompt).turn(turn), sendFrame(3, testUserPrompt).turn("claude:nht_x:3"),
			recvFrame(4, messageStart("a")).turn("claude:nht_x:3")}, want: 1},
		{name: "prompt with its own result still replays it though a later turn ended", wanted: []int64{2}, frames: append(append([]claudeTestFrame(nil), idle...),
			sendFrame(7, testUserPrompt).turn("claude:nht_x:7"), recvFrame(8, claudeReplayResultLine).turn("claude:nht_x:7")), want: 2},
		// The shim gives an untagged legacy result the turn of the prompt
		// before it, so replay can still finish that prompt with it.
		{name: "untagged result after a legacy prompt still replays it though a later turn ended", wanted: []int64{2}, frames: []claudeTestFrame{
			recvFrame(1, claudeInitLine), sendFrame(2, testUserPrompt), recvFrame(3, messageStart("a")), recvFrame(4, claudeReplayResultLine),
			sendFrame(5, testUserPrompt).turn("claude:nht_x:5"), recvFrame(6, claudeReplayResultLine).turn("claude:nht_x:5")}, want: 2},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			floor, _, err := scanTestSpool(t, "claude:nht_x:", test.wanted, test.frames...)
			if err != nil || floor != test.want {
				t.Fatalf("floor = %d, %v; want %d", floor, err, test.want)
			}
		})
	}
}

// Captured spool structures with content, tool data and identifying metadata
// removed. Boundary: file planning must keep every replay fact while avoiding
// the body copy and decode for fact-free recv lines.
func TestClaudeSpoolFactFreeCapturedLines(t *testing.T) {
	const turn = "claude:spool:2"
	const meta = `,"session_id":"native","parent_tool_use_id":null,"uuid":"event"}`
	cases := []struct {
		name     string
		dir      string
		line     string
		skipped  bool
		open     int
		requests int
	}{
		{"init", "recv", `{"type":"system","subtype":"init","session_id":"native"}`, false, 0, 0},
		{"user prompt send", "send", `{"type":"user","message":{"content":[{"text":"","type":"text"}],"role":"user"},"uuid":"anchor"}`, false, 0, 0},
		{"message_start", "recv", `{"type":"stream_event","event":{"type":"message_start","message":{"type":"message","role":"assistant","content":[]}}` + meta, false, 1, 0},
		{"content_block_start", "recv", `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}` + meta, true, 1, 0},
		{"content_block_delta", "recv", `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}` + meta, true, 1, 0},
		{"content_block_stop", "recv", `{"type":"stream_event","event":{"type":"content_block_stop","index":0}` + meta, true, 1, 0},
		{"message_delta", "recv", `{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0},"context_management":{"applied_edits":[]}}` + meta, true, 1, 0},
		{"recv assistant", "recv", `{"type":"assistant","message":{"role":"assistant","content":[]},"session_id":"native","parent_tool_use_id":null,"uuid":"event"}`, true, 1, 0},
		{"recv user", "recv", `{"type":"user","message":{"role":"user","content":[]},"session_id":"native","parent_tool_use_id":null,"uuid":"event"}`, true, 1, 0},
		{"control_request", "recv", `{"request":{"subtype":"can_use_tool"},"request_id":"request","type":"control_request"}`, false, 1, 1},
		{"control_cancel_request", "recv", `{"type":"control_cancel_request","request_id":"request"}`, false, 1, 0},
		{"request before response", "recv", `{"request":{"subtype":"can_use_tool"},"request_id":"request","type":"control_request"}`, false, 1, 1},
		{"control_response", "send", `{"type":"control_response","response":{"subtype":"success","request_id":"request","response":{}}}`, false, 1, 0},
		{"message_stop", "recv", `{"type":"stream_event","event":{"type":"message_stop"}` + meta, false, 0, 0},
		{"request before result", "recv", `{"request":{"subtype":"can_use_tool"},"request_id":"request","type":"control_request"}`, false, 0, 1},
		{"result", "recv", `{"session_id":"native","type":"result","subtype":"success","is_error":false,"result":""}`, false, 0, 0},
	}
	var frames []claudeTestFrame
	for i, tc := range cases {
		frame := claudeTestFrame{Seq: int64(i + 1), Dir: tc.dir, Line: tc.line, TurnID: turn}
		frames = append(frames, frame)
		t.Run(tc.name, func(t *testing.T) {
			fast, ok := claudeSpoolFactFreeFrame(bytes.TrimSpace(claudeTestSpoolBytes(t, frame)))
			if ok != tc.skipped || ok && (fast.Line != nil || fast.Seq != frame.Seq || string(fast.Dir) != frame.Dir || fast.TurnID != turn) {
				t.Fatalf("fast frame = %+v, %v; want skipped %v with original metadata", fast, ok, tc.skipped)
			}
			floor, facts, err := scanTestSpool(t, "claude:spool:", []int64{2}, frames...)
			// A prompt after the flushed tail does not lower the floor yet.
			if i == 0 {
				if err != nil || floor != 1 || facts.nativeID != "native" {
					t.Fatalf("init facts = %+v, %v", facts, err)
				}
				return
			}
			wantFloor := int64(2)
			if i == 1 {
				wantFloor = 1
			}
			if err != nil || floor != wantFloor || facts.last != frame.Seq || facts.nativeID != "native" || len(facts.open) != tc.open || len(facts.requests) != tc.requests || len(facts.prompts) != 1 || facts.prompts[0].Anchor != "anchor" || facts.wanted[2].answered != (tc.name == "result") {
				t.Fatalf("floor %d, facts %+v, err %v; want floor %d, open %d, requests %d", floor, facts, err, wantFloor, tc.open, tc.requests)
			}
		})
	}
}

// Prefixes outside the CLI's compact spelling use the original decoder.
func TestClaudeSpoolFactFreeFallsBackForUnrecognisedLines(t *testing.T) {
	const delta = `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":""}}}`
	for _, tc := range []struct {
		name string
		line string
	}{
		{"unknown event", strings.Replace(delta, "content_block_delta", "future_delta", 1)},
		{"reordered fields", `{"event":{"type":"content_block_delta"},"type":"stream_event"}`},
		{"whitespace", strings.Replace(delta, `,"event":`, `, "event":`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := bytes.TrimSpace(claudeTestSpoolBytes(t, recvFrame(1, tc.line)))
			if _, ok := claudeSpoolFactFreeFrame(raw); ok {
				t.Fatal("unrecognised line took the fast path")
			}
			_, facts, err := scanTestSpool(t, "", nil, recvFrame(1, tc.line))
			if err != nil || facts.last != 1 {
				t.Fatalf("fallback scan = %+v, %v", facts, err)
			}
		})
	}
}

func TestClaudeReplayFloorRejectsUnprovableEvidence(t *testing.T) {
	cases := []struct {
		name   string
		wanted []int64
		raw    []byte
	}{
		{name: "malformed complete line", raw: []byte("{\"seq\":1,\"dir\":\"recv\",\"line\":\"{}\"}\nnot json\n")},
		{name: "sequence goes backwards", raw: claudeTestSpoolBytes(t, recvFrame(2, claudeInitLine), recvFrame(1, claudeInitLine))},
		{name: "foreign turn id", raw: claudeTestSpoolBytes(t, sendFrame(1, testUserPrompt).turn("claude:other:1"))},
		{name: "accepted prompt sequence is not a prompt", wanted: []int64{1}, raw: claudeTestSpoolBytes(t, recvFrame(1, claudeInitLine), recvFrame(2, claudeReplayResultLine))},
		// Owner review probes: a gap could hide an open message.
		{name: "missing prefix", raw: claudeTestSpoolBytes(t, recvFrame(10, textDelta(0, "x")))},
		{name: "missing message start", raw: claudeTestSpoolBytes(t, recvFrame(1, claudeInitLine), recvFrame(3, textDelta(0, "x")))},
		{name: "invalid direction", raw: claudeTestSpoolBytes(t, claudeTestFrame{Seq: 1, Dir: "sideways", Line: claudeInitLine})},
		{name: "missing line", raw: []byte("{\"seq\":1,\"dir\":\"recv\"}\n")},
		{name: "fact-free frame with a gap", raw: claudeTestSpoolBytes(t, recvFrame(2, `{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`))},
		{name: "fact-free frame with foreign turn", raw: claudeTestSpoolBytes(t, recvFrame(1, `{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`).turn("claude:other:1"))},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "spool.jsonl")
			if err := os.WriteFile(path, test.raw, 0o600); err != nil {
				t.Fatal(err)
			}
			facts := newClaudeSpoolFacts("claude:nht_x:", 0, test.wanted)
			_, err := scanClaudeSpool(path, facts)
			if err == nil {
				_, err = facts.floor()
			}
			if !errors.Is(err, errClaudeRecoveryUnproven) {
				t.Fatalf("err = %v, want errClaudeRecoveryUnproven", err)
			}
		})
	}
}

// A stopped earlier child can leave one last commands_changed notice in its
// thread's spool, numbered by its own lower sequence, between two frames of
// the running one. That used to refuse the thread's re-attach for good. Such a
// notice carries no replay fact and is skipped; a lower frame of any other
// kind, another thread's turn, or sequence zero still proves nothing and is
// refused. Boundary: the spool scan the re-attach plans from.
func TestClaudeReplayFloorSkipsAStoppedChildsLastSystemNotice(t *testing.T) {
	const notice = `{"type":"system","subtype":"commands_changed"}`
	for _, tc := range []struct {
		name  string
		stray claudeTestFrame
		ok    bool
	}{
		{"lower system notice", recvFrame(1, notice), true},
		{"lower message", recvFrame(1, textDelta(0, "x")), false},
		{"lower init", recvFrame(1, claudeInitLine), false},
		{"lower other system subtype", recvFrame(1, `{"type":"system","subtype":"compact_boundary"}`), false},
		{"lower sent prompt", sendFrame(1, testUserPrompt), false},
		{"lower notice of another thread's turn", recvFrame(1, notice).turn("claude:other:1"), false},
		{"sequence zero", recvFrame(0, notice), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, facts, err := scanTestSpool(t, "claude:nht_x:", nil,
				recvFrame(1, claudeInitLine), recvFrame(2, notice), tc.stray, recvFrame(3, notice))
			if tc.ok && (err != nil || facts.last != 3) {
				t.Fatalf("scan = %v, last %d; want the stray skipped", err, facts.last)
			}
			if !tc.ok && !errors.Is(err, errClaudeRecoveryUnproven) {
				t.Fatalf("scan = %v, want errClaudeRecoveryUnproven", err)
			}
		})
	}
}

// The shim flushes on its own schedule: a scan can see a line the writer has
// only half written. That tail is not evidence yet, and it replays anyway
// because it is after every complete line.
func TestClaudeReplayFloorIgnoresPartialTrailingWrite(t *testing.T) {
	raw := claudeTestSpoolBytes(t, recvFrame(1, claudeInitLine), recvFrame(2, claudeReplayResultLine))
	raw = append(raw, []byte(`{"seq":3,"dir":"recv","line":"{\"type\":\"stream_ev`)...)
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	facts := newClaudeSpoolFacts("", 0, nil)
	if missing, err := scanClaudeSpool(path, facts); err != nil || missing {
		t.Fatalf("scan = missing %v, %v", missing, err)
	}
	if floor, err := facts.floor(); err != nil || floor != 2 || facts.last != 2 {
		t.Fatalf("floor = %d (last %d), %v; want 2", floor, facts.last, err)
	}
}

// A replay follows the same facts to decide which replayed requests are still
// open: an unanswered request retired by its own turn's result or a cancel is
// reported so it is not resurrected as a dead dialog.
func TestClaudeReplayFactsReportRetiredRequests(t *testing.T) {
	facts := newClaudeSpoolFacts("", 0, nil)
	observe := func(frame claudeTestFrame) []string {
		retired, err := facts.observe(claudeSpoolFrame{Seq: frame.Seq, Dir: protocol.Direction(frame.Dir), Line: []byte(frame.Line), TurnID: frame.TurnID})
		if err != nil {
			t.Fatal(err)
		}
		return retired
	}
	observe(recvFrame(1, `{"type":"control_request","request_id":"a","request":{"subtype":"can_use_tool"}}`))
	observe(recvFrame(2, `{"type":"control_request","request_id":"b","request":{"subtype":"can_use_tool"}}`).turn("claude:k:9"))
	if retired := observe(recvFrame(3, `{"type":"control_cancel_request","request_id":"a"}`)); len(retired) != 1 || retired[0] != "a" {
		t.Fatalf("cancel retired %v, want a", retired)
	}
	if retired := observe(recvFrame(4, claudeReplayResultLine)); len(retired) != 0 {
		t.Fatalf("unrelated result retired %v", retired)
	}
	if retired := observe(recvFrame(5, claudeReplayResultLine).turn("claude:k:9")); len(retired) != 1 || retired[0] != "b" {
		t.Fatalf("own result retired %v, want b", retired)
	}
}

// A prompt accepted under a provisional shim handle proves its native only
// through its own send line and the root init the shim tagged with that same
// written turn; anything weaker leaves the handle unresolved rather than
// guessing from order or from the spool's last init.
func TestClaudeSpoolFactsResolveAProvisionalHandleOnlyFromItsOwnTaggedInit(t *testing.T) {
	prompt := func(uuid string) string {
		return `{"type":"user","uuid":"` + uuid + `","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`
	}
	const init = `{"type":"system","subtype":"init","session_id":"native-1"}`
	const other = `{"type":"system","subtype":"init","session_id":"native-2"}`
	for _, tc := range []struct {
		name    string
		frames  []claudeTestFrame
		anchors []string
		want    string
	}{
		{"own tagged init", []claudeTestFrame{sendFrame(1, prompt("a")).turn("claude:h:1"), recvFrame(2, init).turn("claude:h:1")}, nil, "native-1"},
		{"legacy untagged lines", []claudeTestFrame{sendFrame(1, prompt("a")), recvFrame(2, init)}, nil, ""},
		{"init tagged with another prompt", []claudeTestFrame{sendFrame(1, prompt("a")).turn("claude:h:1"), sendFrame(2, prompt("b")).turn("claude:h:2"), recvFrame(3, init).turn("claude:h:2")}, nil, ""},
		{"anchor on two send lines", []claudeTestFrame{sendFrame(1, prompt("a")).turn("claude:h:1"), recvFrame(2, init).turn("claude:h:1"), sendFrame(3, prompt("a")).turn("claude:h:3")}, nil, ""},
		{"tagged send and an untagged duplicate", []claudeTestFrame{sendFrame(1, prompt("a")).turn("claude:h:1"), recvFrame(2, init).turn("claude:h:1"), sendFrame(3, prompt("a"))}, nil, ""},
		{"subagent init", []claudeTestFrame{sendFrame(1, prompt("a")).turn("claude:h:1"), recvFrame(2, `{"type":"system","subtype":"init","session_id":"native-1","parent_tool_use_id":"toolu_1"}`).turn("claude:h:1")}, nil, ""},
		{"two natives on one tag", []claudeTestFrame{sendFrame(1, prompt("a")).turn("claude:h:1"), recvFrame(2, init).turn("claude:h:1"), recvFrame(3, other).turn("claude:h:1")}, nil, ""},
		{"another anchor's tag names two natives", []claudeTestFrame{sendFrame(1, prompt("a")).turn("claude:h:1"), recvFrame(2, init).turn("claude:h:1"),
			sendFrame(3, prompt("b")).turn("claude:h:3"), recvFrame(4, init).turn("claude:h:3"), recvFrame(5, other).turn("claude:h:3")}, []string{"a", "b"}, ""},
		{"two anchors prove different natives", []claudeTestFrame{sendFrame(1, prompt("a")).turn("claude:h:1"), recvFrame(2, init).turn("claude:h:1"),
			sendFrame(3, prompt("b")).turn("claude:h:3"), recvFrame(4, other).turn("claude:h:3")}, []string{"a", "b"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := newClaudeSpoolFacts("", 0, nil)
			for _, frame := range tc.frames {
				if _, err := facts.observe(claudeSpoolFrame{Seq: frame.Seq, Dir: protocol.Direction(frame.Dir), Line: []byte(frame.Line), TurnID: frame.TurnID}); err != nil {
					t.Fatal(err)
				}
			}
			anchors := tc.anchors
			if anchors == nil {
				anchors = []string{"a"}
			}
			if got, _ := facts.resolvedNative(anchors); got != tc.want {
				t.Fatalf("resolved %q, want %q", got, tc.want)
			}
		})
	}
}

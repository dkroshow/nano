package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/threadindex"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// This viewer consumes the session's real HTTP stream. Every message is
// applied before the assertion, including intermediate appends and handoffs.
type grokHTTPViewer struct {
	t               *testing.T
	messages        chan liveTextMessage
	status          *liveTextStatus
	view            *livePaneView
	replies         map[string]string // stable live id -> scrubbed saved segment text
	allowEarlySaved bool              // the documented early-notice seam, in its one regression case
}

func openGrokHTTP(t *testing.T, h *liveTextHarness) *grokHTTPViewer {
	t.Helper()
	stream := httptest.NewServer(http.HandlerFunc(h.sess.HandleLiveStream))
	response, err := http.Get(stream.URL + "?view=enhanced&limit=40")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close(); stream.Close() })
	v := &grokHTTPViewer{t: t, messages: make(chan liveTextMessage, 128), view: &livePaneView{expectSaved: map[string]string{}, neverSaved: map[string]bool{}}, replies: map[string]string{}}
	go func() {
		defer close(v.messages)
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
				var m liveTextMessage
				if json.Unmarshal([]byte(data), &m) == nil {
					v.messages <- m
				}
			}
		}
	}()
	v.expect("initial tail", func(m liveTextMessage) bool { return m.Type == "tail" })
	return v
}

func (v *grokHTTPViewer) expect(what string, until func(liveTextMessage) bool) {
	v.t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case m, ok := <-v.messages:
			if !ok {
				v.t.Fatalf("stream ended waiting for %s", what)
			}
			if m.Status != nil {
				v.status = m.Status
			}
			// Grok saves the newest chunk id, while the live entry keeps its first.
			// Use the captured mapping, independently of the loop's private key.
			before := append([]liveTextEntry(nil), v.view.live...)
			for id := range v.replies {
				v.view.neverSaved[id] = true
			}
			v.view.apply(v.t, m)
			if m.Type == "error" {
				v.t.Fatal(m.Error)
			}
			for _, e := range before {
				if e.Kind == "reply" && !containsEntry(v.view.live, e.ID) && !v.hasReply(v.replies[e.ID]) && !v.view.neverSaved["unsaved:"+e.ID] {
					v.t.Fatalf("%s left without its captured saved row (%s)", e.ID, m.Type)
				}
			}
			for _, e := range v.view.live {
				if e.Kind == "reply" && v.hasReply(v.replies[e.ID]) && !v.allowEarlySaved {
					v.t.Fatalf("%s shows both live and saved (%s)", e.ID, m.Type)
				}
			}
			if until(m) {
				return
			}
		case <-timer.C:
			v.t.Fatalf("waiting for %s: rows=%d live=%+v", what, len(v.view.rows), v.view.live)
		}
	}
}

func grokWireTurns(t *testing.T) [][][]byte {
	t.Helper()
	var turns [][][]byte
	var frames [][]byte
	for _, line := range fixtureLines(t, "grok-turns.wire.jsonl") {
		if strings.Contains(string(line), `"session/prompt"`) {
			continue
		}
		frames = append(frames, line)
		if strings.Contains(string(line), `"turn_completed"`) {
			turns = append(turns, frames)
			frames = nil
		}
	}
	return turns
}

// The HTTP boundary must stream a reply after thinking in the same response,
// but never a partial reply first seen after restart or a load replay.
// The first reply chunk checks its new segment before the HTTP stream sees it.
// Later appends do not read; the segment's end triggers one more read.
func TestGrokFileLiveHTTPChunkStarts(t *testing.T) {
	allFrames := grokWireTurns(t)[0]
	for _, mode := range []string{"thinking then reply", "restart mid-response", "load replay"} {
		t.Run(mode, func(t *testing.T) {
			frames := allFrames
			h := newLiveTextHarnessFor(t, "grok", threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40})
			v := openGrokHTTP(t, h)
			if mode == "restart mid-response" {
				frames = frames[3:4]
			}
			if mode == "thinking then reply" {
				h.hold()
				h.save(fixtureLines(t, "grok-turns.saved.jsonl")[:1])
				waitGrokRows(t, h, 1)
				v.barrier(h)
				reads := h.catalogReads.Load()
				if reads == 0 {
					t.Fatal("catalog read counter was not exercised")
				}
				// Two thinking frames precede four captured reply chunks.
				// Wait after each chunk so every append has its own inbox drain.
				h.feed(frames[:2])
				for i, text := range []string{"First ", "First re", "First reply", "First reply."} {
					h.feed(frames[i+2 : i+3])
					v.expect("reply chunk", func(liveTextMessage) bool { return v.view.reply() != nil && v.view.reply().Text == text })
					if got := h.catalogReads.Load() - reads; got != 1 {
						t.Fatalf("reply chunk %d catalog reads=%d, want 1 (new segment only)", i+1, got)
					}
				}
				h.feed(frames[len(frames)-1:])
				v.expect("segment ended", func(liveTextMessage) bool { return v.view.reply() != nil && v.view.reply().State == "done" })
				if got := h.catalogReads.Load() - reads; got != 2 {
					t.Fatalf("segment end catalog reads=%d, want 2 (creation and end)", got)
				}
			}
			for _, line := range frames {
				if mode == "thinking then reply" {
					break
				}
				if mode == "load replay" {
					var msg map[string]any
					json.Unmarshal(line, &msg)
					p := msg["params"].(map[string]any)
					p["_meta"].(map[string]any)["isReplay"] = true
					line, _ = json.Marshal(msg)
				}
				h.projection.ApplyWireLive(protocol.Recv, line, time.Now(), true, false)
			}
			// An outbox change follows all frames and is the stream's ordered barrier.
			h.sess.PublishLiveOutbox(prompts.OutboxEntry{ActionID: "barrier", PresentationText: "Barrier.", State: prompts.Uncertain})
			v.expect("ordered barrier", func(liveTextMessage) bool { return containsEntry(v.view.live, "prompt:barrier") })
			reply := v.view.reply()
			if mode == "thinking then reply" {
				want := "reply:" + grokFixtureThread + "-1283963"
				if reply == nil || reply.ID != want || reply.Text != "First reply." {
					t.Fatalf("reply=%+v, want full segment with stable first-chunk id %s", reply, want)
				}
			} else if reply != nil {
				t.Fatalf("%s showed partial/replayed live text: %+v", mode, reply)
			}
			if mode != "thinking then reply" {
				h.save(fixtureLines(t, "grok-turns.saved.jsonl")[:4])
				v.expect("saved-only reply after recovery", func(liveTextMessage) bool { return v.hasReply("First reply.") })
				if v.view.reply() != nil {
					t.Fatal("recovery streamed a partial reply beside its saved row")
				}
			}
		})
	}
}

func (v *grokHTTPViewer) hasReply(text string) bool {
	if text == "" {
		return false
	}
	for _, row := range v.view.rows {
		if row.Type == "agent_message" && row.Text == text {
			return true
		}
	}
	return false
}

func (v *grokHTTPViewer) barrier(h *liveTextHarness) {
	v.t.Helper()
	h.barriers++
	action := fmt.Sprintf("http-barrier-%d", h.barriers)
	e := prompts.OutboxEntry{ActionID: action, PresentationText: "Barrier.", State: prompts.Uncertain}
	h.sess.PublishLiveOutbox(e)
	v.expect("ordered barrier", func(liveTextMessage) bool { return containsEntry(v.view.live, "prompt:"+action) })
	e.State = prompts.Refused
	h.sess.PublishLiveOutbox(e)
	v.expect("barrier removed", func(liveTextMessage) bool { return !containsEntry(v.view.live, "prompt:"+action) })
}

func capturedGrokPrompt(h *liveTextHarness, n int, turn, key string) prompts.OutboxEntry {
	e := prompts.OutboxEntry{HostedThreadID: h.sess.HostedThreadID(), ActionID: fmt.Sprintf("action-%d", n), Sequence: uint64(n), PresentationText: fmt.Sprintf("Neutral prompt %d.", n), State: prompts.Accepted}
	if turn != "" || key != "" {
		e.Binding = &prompts.Binding{EventID: key, Scope: model.Scope{Provider: model.ProviderGrok, NativeTurnID: &turn}}
	}
	return e
}

// This is the mounted HTTP failure surface: every streamed message is checked
// for simultaneous saved/live text and for a removal without the saved row.
func TestGrokFileLiveHTTPHandoffs(t *testing.T) {
	wire := grokWireTurns(t)
	saved := fixtureLines(t, "grok-turns.saved.jsonl")

	for _, mode := range []string{"prompt and reply", "saved commit before final chunk; notice after segment end", "early saved notice overlaps until segment end", "old prompt without key", "old binding after save", "Stop saved at once", "Stop saved at next prompt", "stuck outbox", "task-completed turn"} {
		t.Run(mode, func(t *testing.T) {
			native := grokFixtureThread
			if mode == "task-completed turn" {
				native = "1247675c-27b4-5553-9b66-4b599cf603ab"
			}
			h := newLiveTextHarnessForThread(t, "grok", threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40}, native)
			v := openGrokHTTP(t, h)
			first := "reply:" + grokFixtureThread + "-1283963"
			switch mode {
			case "stuck outbox":
				if _, _, err := h.sess.PromptStore().Submit(context.Background(), prompts.Submission{HostedThreadID: h.sess.HostedThreadID(), Provider: model.ProviderGrok, ActionID: "action-1", ProviderText: "Neutral prompt 1.", PresentationText: "Neutral prompt 1.", TextHash: "neutral-1", RequestHash: "neutral-1", Origin: prompts.HumanOrigin}); err != nil {
					t.Fatal(err)
				}
				for _, state := range []prompts.DeliveryState{prompts.Queued, prompts.Parked, prompts.Delivering, prompts.Uncertain} {
					e := capturedGrokPrompt(h, 1, "", "")
					e.State = state
					h.sess.PublishLiveOutbox(e)
					v.expect("stuck prompt state", func(liveTextMessage) bool {
						return len(v.view.live) == 1 && v.view.live[0].State == string(state) && v.view.live[0].ActionID == "action-1"
					})
				}
				// A refused operation leaves; a resend has a new action and key.
				// The delivery journal still supplies the stuck prompt's text
				// and status for the existing Resend and Discard controls.
				e := capturedGrokPrompt(h, 1, "", "")
				e.State = prompts.Refused
				h.sess.PublishLiveOutbox(e)
				v.expect("refused prompt", func(liveTextMessage) bool { return len(v.view.live) == 0 })
				if _, err := h.sess.PromptStore().Cancel(context.Background(), "action-1"); err != nil {
					t.Fatal(err)
				}
				seedUncertainTransactionalTurn(t, h.sess.PromptStore(), h.sess.HostedThreadID(), model.ProviderGrok, h.sess.ThreadID(), "action-2", "Neutral prompt 2.")
				record, _, err := h.sess.PromptStore().Get(context.Background(), "action-2")
				if err != nil {
					t.Fatal(err)
				}
				h.sess.PublishLiveOutbox(record.Outbox)
				v.expect("resend action", func(liveTextMessage) bool { return containsEntry(v.view.live, "prompt:action-2") })
				// This harness's prompt store is separate from its control
				// metadata store, so use the journal's ordinary direct read.
				// The harness routes journal reads to its prompt database.
				rec := httptest.NewRecorder()
				h.sess.HandleTurnDeliveryList(rec, httptest.NewRequest("GET", "/turn-delivery?thread="+grokFixtureThread, nil))
				var delivery struct {
					Operations []promptView `json:"operations"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &delivery); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, op := range delivery.Operations {
					if op.ClientActionID == "action-2" && op.State == string(prompts.Uncertain) && slices.Contains(op.AllowedActions, promptResend) && op.Text == "Neutral prompt 2." {
						found = true
					}
				}
				if !found {
					t.Fatalf("stuck prompt lacks text/state for Resend and Discard: %s", rec.Body.String())
				}
				h.save(saved[:1])
				v.barrier(h)
				if !containsEntry(v.view.live, "prompt:action-2") {
					t.Fatal("unrelated saved prompt claimed the resend")
				}
				return
			case "old prompt without key", "old binding after save":
				var row map[string]any
				json.Unmarshal(saved[0], &row)
				delete(row["params"].(map[string]any)["update"].(map[string]any)["content"].(map[string]any), "_meta")
				unkeyed, _ := json.Marshal(row)
				turn := "988cfc40-93a9-5677-be8b-ef8992ca08d1"
				e := capturedGrokPrompt(h, 1, "", "")
				if mode == "old prompt without key" {
					e = capturedGrokPrompt(h, 1, turn, "")
				}
				h.sess.PublishLiveOutbox(e)
				v.expect("old prompt", func(liveTextMessage) bool { return containsEntry(v.view.live, "prompt:action-1") })
				// The file binds the prompt's turn only when its first output arrives.
				if mode == "old binding after save" {
					h.hold()
				}
				h.save([][]byte{unkeyed, saved[1]})
				if mode == "old binding after save" {
					waitGrokRows(t, h, 2)
					e = capturedGrokPrompt(h, 1, turn, "")
					h.sess.PublishLiveOutbox(e)
				}
				v.expect("old prompt turn handoff", func(liveTextMessage) bool { return len(v.view.live) == 0 && len(v.view.rows) > 0 })
				return
			case "task-completed turn":
				// Constructed wire envelopes from actual saved rows; ids,
				// chunk counts and content stay exact. No live capture exists.
				// Source: task-completed-saved/updates-excerpt.jsonl; wire
				// shapes: thread2-nano-wire.jsonl and
				// probe3-exact-block-think-stop-wire.jsonl.
				rows := fixtureLines(t, "grok-task.saved.jsonl")
				for _, line := range rows {
					var m map[string]any
					json.Unmarshal(line, &m)
					delete(m, "timestamp")
					m["jsonrpc"] = "2.0"
					frame, _ := json.Marshal(m)
					h.feed([][]byte{frame})
				}
				v.barrier(h)
				if len(v.view.live) != 0 {
					t.Fatalf("provider turn invented live text: %+v", v.view.live)
				}
				h.save(rows)
				waitGrokRows(t, h, 2)
				v.barrier(h)
				if !v.hasReply("The sample background task is complete.") {
					t.Fatal("provider turn's saved reply missing")
				}
				return
			}
			stop := strings.HasPrefix(mode, "Stop")
			n, turn, key := 1, "988cfc40-93a9-5677-be8b-ef8992ca08d1", "11111111-aaaa-4aaa-8aaa-000000000001"
			frames, rows := wire[0], saved[:4]
			if stop {
				n, turn, key = 2, "88ce28f6-903f-56ee-b7ac-f9c8426e4262", "11111111-aaaa-4aaa-8aaa-000000000002"
				frames, rows = wire[1], saved[4:8]
				first = "reply:" + grokFixtureThread + "-1284520"
			}
			text := "First reply."
			if stop {
				text = "Cut reply."
			}
			v.replies[first] = text
			e := capturedGrokPrompt(h, n, turn, key)
			id := "prompt:" + e.ActionID
			v.view.expectSaved[id] = e.PresentationText
			h.sess.PublishLiveOutbox(e)
			v.expect("prompt live", func(liveTextMessage) bool { return containsEntry(v.view.live, id) })
			h.save(rows[:1])
			v.expect("prompt saved", func(liveTextMessage) bool { return !containsEntry(v.view.live, id) })
			h.feed(frames[:4])
			v.expect("reply prefix", func(liveTextMessage) bool { return v.view.reply() != nil && len(v.view.reply().Text) > 5 })
			savedBeforeFinal := mode == "early saved notice overlaps until segment end" || mode == "saved commit before final chunk; notice after segment end"
			if mode == "saved commit before final chunk; notice after segment end" {
				// Commit the real final row after the preceding live chunk,
				// but keep its apply notice behind the segment-end read.
				h.feed(frames[4:5])
				v.expect("pre-final reply", func(liveTextMessage) bool { return v.view.reply() != nil && v.view.reply().Text == "First reply" })
				h.hold()
				h.save(rows[1:])
				waitGrokRows(t, h, 3)
				h.feed(frames[5 : len(frames)-1])
				v.expect("final chunk stays live before segment end", func(liveTextMessage) bool { return v.view.reply() != nil && v.view.reply().Text == text })
				h.feed(frames[len(frames)-1:])
				v.expect("segment-end read hands over before apply notice", func(m liveTextMessage) bool {
					return v.hasReply(text) && !containsEntry(v.view.live, first) && slices.Contains(m.Saved, first)
				})
				h.release()
				v.barrier(h)
			} else if mode == "early saved notice overlaps until segment end" {
				// Grok 1.0.40 measurement (window-measure/result-summary.txt):
				// eight saved replies followed the last live chunk by 352-566 ms
				// (median 520 ms); catalog publication followed by 534-4219 ms
				// (median 769 ms). None arrived earlier in that sample.
				// Force the opposite order here to cover delayed live delivery.
				// Approved documented seam: the saved row initially has no
				// alias for the live prefix. The segment-end read settles it.
				v.allowEarlySaved = true
				h.save(rows[1:])
				v.expect("saved reply before final chunk", func(liveTextMessage) bool { return v.hasReply(text) })
				if !containsEntry(v.view.live, first) {
					t.Fatal("early save removed text without exact proof")
				}
				h.feed(frames[4 : len(frames)-1])
				v.expect("early save still overlaps after final chunk", func(liveTextMessage) bool {
					return v.hasReply(text) && v.view.reply() != nil && v.view.reply().Text == text
				})
				h.feed(frames[len(frames)-1:])
				v.expect("segment end settles early save", func(m liveTextMessage) bool {
					return v.hasReply(text) && !containsEntry(v.view.live, first) && slices.Contains(m.Saved, first)
				})
				v.allowEarlySaved = false
			} else {
				h.feed(frames[4:])
			}
			if !savedBeforeFinal {
				v.expect("ended full reply", func(liveTextMessage) bool {
					return v.view.reply() != nil && v.view.reply().State == "done" && v.view.reply().Text == text
				})
			}
			if mode == "Stop saved at next prompt" {
				h.save([][]byte{rows[1], rows[3]})
				v.barrier(h)
				if v.view.reply() == nil {
					t.Fatal("saved turn end cleared Stop-cut text")
				}
				rec := httptest.NewRecorder()
				h.sess.HandleConversationView(rec, httptest.NewRequest("GET", "/conversation?view=messages", nil), conversationViewMessages)
				if strings.Contains(rec.Body.String(), "Cut reply.") {
					t.Fatal("agent read exposed unsaved Stop-cut live text")
				}
				reload := openGrokHTTP(t, h)
				if reload.view.reply() == nil {
					t.Fatal("reload cleared Stop-cut text")
				}
				v.view.neverSaved["unsaved:"+first] = true
				next := capturedGrokPrompt(h, 3, "faba0187-d4df-57e1-a801-ee8918d62ba0", "11111111-aaaa-4aaa-8aaa-000000000003")
				h.sess.PublishLiveOutbox(next)
				// The actual cut row arrives with the next prompt; same exact key.
				h.save([][]byte{rows[2], saved[8]})
			} else if !savedBeforeFinal {
				h.save(rows[1:])
			}
			if !savedBeforeFinal {
				v.expect("saved reply handoff", func(liveTextMessage) bool { return v.hasReply(text) && !containsEntry(v.view.live, first) })
			}
			rec := httptest.NewRecorder()
			h.sess.HandleConversationView(rec, httptest.NewRequest("GET", "/conversation?view=messages", nil), conversationViewMessages)
			if !strings.Contains(rec.Body.String(), text) || strings.Contains(rec.Body.String(), `"live"`) {
				t.Fatalf("agent read=%s", rec.Body.String())
			}
		})
	}
}

func waitGrokRows(t *testing.T, h *liveTextHarness, count int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if len(h.page(threadindex.TranscriptQuery{View: threadindex.DevThreadTranscriptView, HiddenSpecified: true, Limit: 40})) >= count {
			return
		}
	}
	t.Fatal("captured rows never indexed")
}

// The first case is thread2's actual tool/new-response order. The second is
// explicitly constructed from its tool frames and probe3 reply chunks: no
// capture showed reply/tool/reply with the chunk count continuing. Only tool
// session/turn/response scope is changed; all reply ids/chunk counts are exact.
// Sources use one-based JSONL line numbers: probe3-exact-block-think-stop-wire.jsonl
// lines 38-39 supply the thinking predecessor; lines 40-41 are segment A
// (reply chunks 18-19);
// lines 42 and 326 supply segment B (reply chunks 20 and 304).
// thread2-nano-wire.jsonl lines 34, 36, 38 and 41 supply the inserted tools.
// Its actual captured order ends that response and restarts the reply at chunk 1.
// Segment B is eligible because segment A's chunk 19 precedes its chunk 20.
func TestGrokFileLiveHTTPTools(t *testing.T) {
	for _, mode := range []string{"captured new response", "constructed continuing chunk count"} {
		t.Run(mode, func(t *testing.T) {
			native := grokFixtureThread
			if mode == "captured new response" {
				native = "d9ef87b3-7f46-5aad-8933-9ed509cbe590"
			}
			h := newLiveTextHarnessForThread(t, "grok", threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40}, native)
			frames := fixtureLines(t, "grok-tools.wire.jsonl")
			if mode == "captured new response" {
				v := openGrokHTTP(t, h)
				h.feed(frames)
				v.barrier(h)
				e := v.view.reply()
				if e == nil || e.ID != "reply:d9ef87b3-7f46-5aad-8933-9ed509cbe590-1283331" || e.Text != "Neutral." || e.State != "done" {
					t.Fatalf("captured tool/new-response reply=%+v", e)
				}
				return
			}
			v := openGrokHTTP(t, h)
			wire := grokWireTurns(t)[0]
			h.feed(wire[:4])
			first := "reply:" + grokFixtureThread + "-1283963"
			v.expect("first segment", func(liveTextMessage) bool { return containsEntry(v.view.live, first) })
			for _, line := range frames {
				var m map[string]any
				json.Unmarshal(line, &m)
				p := m["params"].(map[string]any)
				u := p["update"].(map[string]any)
				if u["sessionUpdate"] != "tool_call" && u["sessionUpdate"] != "tool_call_update" {
					continue
				}
				p["sessionId"] = grokFixtureThread
				meta, _ := p["_meta"].(map[string]any)
				if meta != nil {
					meta["promptId"] = "988cfc40-93a9-5677-be8b-ef8992ca08d1"
					meta["streamStartMs"] = 1790948816349
					meta["turnStartMs"] = 1790948815434
				}
				b, _ := json.Marshal(m)
				h.feed([][]byte{b})
			}
			second := "reply:" + grokFixtureThread + "-1283965"
			h.feed(wire[4:])
			v.expect("two segments", func(liveTextMessage) bool {
				return len(v.view.live) == 2 && v.view.live[0].ID == first && v.view.live[0].State == "done" && v.view.live[1].ID == second && v.view.live[1].Text == "ply."
			})
			// The captured final-chunk row proves only the second constructed segment.
			// No row exists for the first, so rule 6 keeps it until the next prompt.
			saved := fixtureLines(t, "grok-turns.saved.jsonl")
			var row map[string]any
			json.Unmarshal(saved[2], &row)
			row["params"].(map[string]any)["update"].(map[string]any)["content"].(map[string]any)["text"] = "ply."
			final, _ := json.Marshal(row)
			v.replies[second] = "ply."
			v.view.neverSaved["unsaved:"+first] = true
			v.replies[first] = "no saved row for this constructed segment"
			h.save([][]byte{saved[0], saved[1], final, saved[3]})
			v.expect("second segment handed over", func(liveTextMessage) bool {
				return v.hasReply("ply.") && containsEntry(v.view.live, first) && !containsEntry(v.view.live, second)
			})
			next := capturedGrokPrompt(h, 2, "88ce28f6-903f-56ee-b7ac-f9c8426e4262", "11111111-aaaa-4aaa-8aaa-000000000002")
			h.sess.PublishLiveOutbox(next)
			v.barrier(h)
			h.save(saved[4:5])
			v.expect("unsaved segment superseded", func(liveTextMessage) bool { return len(v.view.live) == 0 })
		})
	}
}

// The session HTTP pane receives saved rows, unsaved text and runtime together.
func TestGrokSessionHTTPHasOneFileTranscriptSource(t *testing.T) {
	h := newLiveTextHarnessFor(t, "grok", threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40})
	h.save(fixtureLines(t, "grok-turns.saved.jsonl")[:1])
	waitGrokRows(t, h, 1)
	h.projection.UpdateRuntime(liveProjectionRuntime{SessionStatus: "ready", TurnActive: true, TurnID: "turn-http"})
	v := openGrokHTTP(t, h)
	if len(v.view.rows) != 1 || v.view.rows[0].Text != "Neutral prompt 1." {
		t.Fatalf("saved HTTP rows = %#v", v.view.rows)
	}
	if v.status == nil || !v.status.Runtime.TurnActive {
		t.Fatalf("initial working status = %#v", v.status)
	}
	h.feed(grokWireTurns(t)[0][:3])
	v.expect("unsaved reply", func(liveTextMessage) bool { return v.view.reply() != nil && v.view.reply().Text == "First " })
	if len(v.view.rows) != 1 {
		t.Fatalf("saved HTTP rows after live text = %#v", v.view.rows)
	}
	h.projection.UpdateRuntime(liveProjectionRuntime{SessionStatus: "ready"})
	v.expect("stopped status", func(m liveTextMessage) bool { return m.Status != nil && !m.Status.Runtime.TurnActive })
	rec := httptest.NewRecorder()
	h.sess.HandleConversationView(rec, httptest.NewRequest(http.MethodGet, "/conversation?view=messages", nil), conversationViewMessages)
	var snapshot liveProjectionSnapshot
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &snapshot) != nil ||
		len(snapshot.Items) != 1 || snapshot.Items[0].Text != "Neutral prompt 1." || snapshot.Runtime.TurnActive {
		t.Fatalf("saved agent HTTP snapshot = %d %s", rec.Code, rec.Body.String())
	}
}

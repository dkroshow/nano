package sessions

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/diagnostics"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/setup"
	"github.com/dkroshow/nano/internal/threadindex"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// The fixture is one real Claude Code 2.1.285 turn (Haiku 4.5), scrubbed: the
// stream-json frames Nano receives and the session file Claude saved. The
// prompt carries the UUID Nano would stamp on it.
const (
	liveTextFixtureSession = "94f881e3-d06d-5b44-814c-194b17d7aada"
	liveTextFixturePrompt  = "11111111-2222-4333-8444-555555555555"
	fixtureReply           = "msg_fc823e6c7ddd5dd584d9bd9b60795fa8:1"
)

type liveTextHarness struct {
	*liveTextViewer
	t           *testing.T
	diagnostics *diagnostics.Store
	sess        *Session
	projection  *LiveProjection
	queue       Queue
	journal     Journal
	metadata    *hostedstore.MetadataStore
	service     *threadindex.Service
	store       *history.Store
	file        string
	// held keeps catalog apply notices from the loop.
	held         atomic.Bool
	barriers     int
	catalogReads atomic.Int64
}

// liveTextViewer is one open pane and the browser's view of it.
type liveTextViewer struct {
	t    *testing.T
	pane *liveTextPane
	view *livePaneView
}

func newLiveTextHarness(t *testing.T) *liveTextHarness {
	return newLiveTextHarnessWith(t, threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40})
}

func newLiveTextHarnessWith(t *testing.T, query threadindex.TranscriptQuery) *liveTextHarness {
	return newLiveTextHarnessFor(t, "claude", query)
}

// The Codex fixture is three real Codex 0.159.2 turns (gpt-6-luna), scrubbed:
// the frames Nano received and the file Codex saved. Stop cut the second
// reply, so Codex saved no reply for it and recorded its turn aborted.
const (
	codexFixtureThread  = "16952569-d518-579a-914e-2c422f5d5946"
	codexFixtureTurn1   = "ee5e8c7a-a3a0-5879-91b2-50c6f4f376d1"
	codexFixtureTurn2   = "df92b81a-36af-5aba-b849-7b8925975a10"
	codexFixtureTurn3   = "88ae4484-14e5-500d-af4c-2f578aae6819"
	codexFixtureReply1  = "msg_7aabb76bc73f5cf08cbeeebad2bec9fb"
	codexFixtureStopped = "msg_898d377a915b5fb689f4fbb2855ba760"
	codexFixtureReply3  = "msg_e2bc6795bd175cd3834899b1adf128b7"
)

func newLiveTextHarnessFor(t *testing.T, provider string, query threadindex.TranscriptQuery) *liveTextHarness {
	return newLiveTextHarnessForThread(t, provider, query, grokFixtureThread)
}

func newLiveTextHarnessForThread(t *testing.T, provider string, query threadindex.TranscriptQuery, grokThread string, configure ...func(*Config)) *liveTextHarness {
	t.Helper()
	root := t.TempDir()
	file := filepath.Join(root, "-disposable", liveTextFixtureSession+".jsonl")
	rootKind, nativeThread := history.RootClaudeProjects, liveTextFixtureSession
	if provider == "codex" {
		file = filepath.Join(root, "2026", "09", "30", "rollout-2026-09-30T23-54-42-"+codexFixtureThread+".jsonl")
		rootKind, nativeThread = history.RootCodexSessions, codexFixtureThread
	}
	if provider == "grok" {
		file = filepath.Join(root, "disposable", grokThread, "updates.jsonl")
		rootKind, nativeThread = history.RootGrokSessions, grokThread
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		summary := `{"info":{"id":"` + grokThread + `","cwd":"/disposable"},"created_at":"2026-10-02T13:46:52Z","updated_at":"2026-10-02T13:48:40Z"}`
		if err := os.WriteFile(filepath.Join(filepath.Dir(file), "summary.json"), []byte(summary), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(t.TempDir(), "history.sqlite")
	h := &liveTextHarness{t: t, file: file}
	probe := registerLiveReadCounter(t, &h.catalogReads, nil)
	store, err := history.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h.store = store
	installLiveReadCounter(t, databasePath, probe)
	service, err := threadindex.New(context.Background(), store, threadindex.Options{
		Roots: []history.SourceRoot{{Provider: history.Provider(provider), Kind: rootKind, Path: root}}, DisableWatcher: true,
		NativeIDProvisional: NativeThreadIDProvisional, ClaudeProjectSlug: setup.ClaudeProjectSlug,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	sink := diagnostics.NewStore(64, "live-text-fixture")
	cfg, generation := liveTextTestConfig(t, provider)
	for _, apply := range configure {
		apply(&cfg)
	}
	cfg.Projection = NewLiveProjection(provider, cfg.HostedThreadID, false)
	h.metadata, _ = cfg.Metadata.(*hostedstore.MetadataStore)
	cfg.Metadata = journalReadMetadata{Metadata: cfg.Metadata, db: cfg.QueuedTurns.Database()}
	sess := New(cfg)
	t.Cleanup(sess.lifecycle.Close)
	if err := sess.InitializeBinding(generation); err != nil {
		t.Fatal(err)
	}
	sess.AttachLiveText(NewLiveTextLoop(LiveTextSources{Catalog: service, Diagnostics: sink}, sess))
	sess.ReserveNativeID(nativeThread)
	h.diagnostics, h.sess, h.service = sink, sess, service
	h.projection = cfg.Projection
	h.queue = cfg.QueuedTurns
	h.journal = cfg.SendAttempts
	service.SetTranscriptChanged(func(context.Context, threadindex.TranscriptChange) {
		if !h.held.Load() {
			sess.view.NotifySavedTranscript()
		}
	})
	h.liveTextViewer = h.open(query)
	return h
}

// open subscribes another pane and waits for its tail.
func (h *liveTextHarness) open(query threadindex.TranscriptQuery) *liveTextViewer {
	v := &liveTextViewer{t: h.t, pane: newLiveTextPane(query), view: &livePaneView{expectSaved: map[string]string{}, neverSaved: map[string]bool{}}}
	liveTextOf(h.sess).subscribe(v.pane)
	v.expect("tail", func(m LiveTextMessage) bool { return m.Type == "tail" })
	return v
}

// hold keeps catalog apply notices from the loop, so a test can act between
// a committed save and the loop learning of it; release delivers one.
func (h *liveTextHarness) hold() { h.held.Store(true) }

func (h *liveTextHarness) release() {
	h.held.Store(false)
	h.sess.view.NotifySavedTranscript()
}

// page reads the catalog directly: the test's own view of what is saved.
func (h *liveTextHarness) page(query threadindex.TranscriptQuery) []threadindex.TranscriptItem {
	h.t.Helper()
	id, err := h.service.ResolveThreadReference(context.Background(), h.sess.stateOwner.ThreadID())
	if err != nil {
		return nil
	}
	response, err := h.service.LoadTranscript(context.Background(), id, query)
	if err != nil {
		h.t.Fatal(err)
	}
	return response.Items
}

// waitTurnEnded waits until the catalog's newest page shows this native turn
// ended.
func (h *liveTextHarness) waitTurnEnded(nativeTurnID string) {
	h.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		id, err := h.service.ResolveThreadReference(context.Background(), h.sess.stateOwner.ThreadID())
		if err != nil {
			continue // not indexed yet
		}
		response, err := h.service.LoadTranscript(context.Background(), id, threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40})
		if err != nil {
			h.t.Fatal(err)
		}
		for _, turn := range response.Turns {
			if turn.NativeTurnID == nativeTurnID && (turn.Status == string(history.TurnCompleted) || turn.Status == string(history.TurnInterrupted)) {
				return
			}
		}
	}
	h.t.Fatalf("turn %s never read as ended", nativeTurnID)
}

// waitSaved waits until the catalog holds a row with this native id.
func (h *liveTextHarness) waitSaved(nativeID string) {
	h.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		for _, row := range h.page(threadindex.TranscriptQuery{View: threadindex.DevThreadTranscriptView, HiddenSpecified: true, Limit: 40}) {
			if row.NativeItemID == nativeID {
				return
			}
		}
	}
	h.t.Fatalf("%s was never saved", nativeID)
}

func lineWith(lines [][]byte, text string) int {
	for i, line := range lines {
		if strings.Contains(string(line), text) {
			return i
		}
	}
	return -1
}

func fixtureLines(t *testing.T, name string) [][]byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "live_text", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines [][]byte
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		lines = append(lines, append([]byte(nil), scanner.Bytes()...))
	}
	return lines
}

// feed applies frames the way a live Claude attachment does.
func (h *liveTextHarness) feed(lines [][]byte) {
	for _, line := range lines {
		// As the session loop does, a result first names its prompt's turn:
		// here, the turn the test's prompt was given.
		var frame struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &frame) == nil && frame.Type == "result" {
			turn := h.projection.currentTurnID()
			h.projection.setClaudeResultTurnID(turn)
		}
		h.projection.applyWireLive(protocol.Recv, line, time.Now(), true, false)
	}
}

// save appends lines to the provider file and waits for the catalog to apply
// them.
func (h *liveTextHarness) save(lines [][]byte) {
	h.t.Helper()
	f, err := os.OpenFile(h.file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, line := range lines {
		f.Write(append(line, '\n'))
	}
	f.Close()
	h.service.RequestRefresh()
}

// expect applies the pane's messages to the simulated browser, checking
// continuity after each, until one satisfies until.
func (v *liveTextViewer) expect(what string, until func(LiveTextMessage) bool) {
	v.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-v.pane.ready:
		case <-time.After(50 * time.Millisecond):
		}
		for _, raw := range v.pane.take() {
			var message LiveTextMessage
			if err := json.Unmarshal(raw, &message); err != nil {
				v.t.Fatal(err)
			}
			v.view.apply(v.t, message)
			if until(message) {
				return
			}
		}
	}
	v.t.Fatalf("timed out waiting for %s; view rows=%d live=%v", what, len(v.view.rows), v.view.live)
}

// livePaneView is the browser's side, reduced to what continuity needs.
type livePaneView struct {
	rows  []threadindex.TranscriptItem
	live  []liveTextEntry
	shown map[string]bool // live entry ids ever shown
	// expectSaved names prompts the test will save, by their unique text, so
	// prompt continuity is checked independently of the code under test.
	expectSaved map[string]string
	// neverSaved names replies the test never saves, which may leave live
	// text without a row.
	neverSaved map[string]bool
}

func (v *livePaneView) apply(t *testing.T, m LiveTextMessage) {
	t.Helper()
	if v.shown == nil {
		v.shown = map[string]bool{}
	}
	switch m.Type {
	case "tail":
		v.rows = m.Rows
	case "update":
		for _, row := range m.Rows {
			replaced := false
			for i := range v.rows {
				if v.rows[i].ID == row.ID {
					v.rows[i], replaced = row, true
				}
			}
			if !replaced {
				v.rows = append(v.rows, row)
			}
		}
		removed := map[string]bool{}
		for _, id := range m.Removed {
			removed[id] = true
		}
		kept := v.rows[:0]
		for _, row := range v.rows {
			if !removed[row.ID] {
				kept = append(kept, row)
			}
		}
		v.rows = kept
	case "append":
		for i := range v.live {
			if v.live[i].ID == m.ID {
				v.live[i].Text += m.Append
			}
		}
	}
	if m.Live != nil {
		before := v.live
		v.live = append([]liveTextEntry(nil), (*m.Live)...)
		// Never neither: live text that leaves must have its saved row, except
		// a prompt the test does not save (refused, or never sent to the file).
		for _, gone := range before {
			if containsEntry(v.live, gone.ID) || gone.Kind == "prompt" && v.expectSaved[gone.ID] == "" || v.neverSaved[gone.ID] {
				continue
			}
			if !v.hasSaved(gone) {
				t.Fatalf("%s left live text without its saved row mounted (message %s)", gone.ID, m.Type)
			}
		}
	}
	// Never both: no live text beside the saved row that carries it.
	for _, entry := range v.live {
		v.shown[entry.ID] = true
		if v.hasSaved(entry) {
			t.Fatalf("%s shown twice: as live text and as a saved row (message %s)", entry.ID, m.Type)
		}
	}
}

// hasSaved reports a mounted saved row for live text: a reply by its native
// id, an expected prompt by its unique text.
func (v *livePaneView) hasSaved(entry liveTextEntry) bool {
	if entry.Kind == "reply" {
		return v.hasNative(strings.TrimPrefix(entry.ID, "reply:"))
	}
	text := v.expectSaved[entry.ID]
	for _, row := range v.rows {
		if text != "" && row.Type == "user_message" && row.Text == text {
			return true
		}
	}
	return false
}

func (v *livePaneView) reply() *liveTextEntry {
	for i := range v.live {
		if v.live[i].Kind == "reply" {
			return &v.live[i]
		}
	}
	return nil
}

func (v *livePaneView) hasNative(id string) bool {
	for _, row := range v.rows {
		if row.NativeItemID == id {
			return true
		}
	}
	return false
}

func containsEntry(live []liveTextEntry, id string) bool {
	for _, entry := range live {
		if entry.ID == id {
			return true
		}
	}
	return false
}

// A stamped prompt and a streamed reply each hand over to their saved rows in
// the message that adds the rows: never both, never neither. Confirmation
// alone does not remove the prompt; the turn's own saved prompt does not
// supersede its ended reply, nor does a scan between two saved blocks of one
// message. A pane opening reads the file for every pane at once: the panes
// already open get the saved row and lose its live text in the same read.
func TestLiveTextHandsOverToSavedRowsInOneMessage(t *testing.T) {
	h := newLiveTextHarness(t)
	saved := fixtureLines(t, "claude-turn.saved.jsonl")
	userLine, thinkingLine := lineWith(saved, liveTextFixturePrompt), lineWith(saved, `"thinking"`)
	accepted := prompts.OutboxEntry{
		HostedThreadID: h.sess.HostedThreadID(), ActionID: "action-1", Sequence: 1,
		PresentationText: "Reply with two short sentences about rain.", State: prompts.Accepted,
		Binding: &prompts.Binding{EventID: liveTextFixturePrompt},
	}
	h.view.expectSaved["prompt:action-1"] = accepted.PresentationText
	h.sess.view.PublishLiveOutbox(accepted)
	h.expect("prompt", func(LiveTextMessage) bool { return len(h.view.live) == 1 })
	confirmed := accepted
	confirmed.State = prompts.Confirmed
	h.sess.view.PublishLiveOutbox(confirmed)
	h.barrier()
	if !containsEntry(h.view.live, "prompt:action-1") {
		t.Fatalf("confirmation removed the prompt before its saved row: %+v", h.view.live)
	}
	// The shim tags a delivered prompt's frames with its turn.
	h.projection.setClaudeProviderTurnID("claude:" + h.sess.HostedThreadID() + ":1")
	h.feed(fixtureLines(t, "claude-turn.wire.jsonl"))
	// A finished reply reads done before its saved row lands, so the pane
	// stops showing it as streaming.
	h.expect("finished reply", func(LiveTextMessage) bool { return h.view.reply() != nil && h.view.reply().State == "done" })
	h.save(saved[:userLine+1])
	h.expect("prompt saved", func(LiveTextMessage) bool { return !containsEntry(h.view.live, "prompt:action-1") })
	h.save(saved[userLine+1 : thinkingLine+1])
	h.barrier()
	if h.view.reply() == nil {
		t.Fatalf("the reply left live text before its own row was saved: %+v", h.view.live)
	}
	// The catalog holds the reply before the loop hears of it.
	h.hold()
	h.save(saved[thinkingLine+1:])
	h.waitSaved(fixtureReply)
	second := h.open(threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40})
	if second.view.reply() != nil || !second.view.hasNative(fixtureReply) {
		t.Fatalf("the second pane shows rows %+v and live text %+v, want the saved reply only as a row", second.view.rows, second.view.live)
	}
	h.expect("reply saved", func(m LiveTextMessage) bool {
		if h.view.reply() != nil {
			return false
		}
		if m.Type != "update" || !slices.Contains(m.Saved, "reply:"+fixtureReply) {
			t.Fatalf("handoff came as %q naming %v saved, want one update naming the reply", m.Type, m.Saved)
		}
		return true
	})
	h.release()
	var reply bool
	for _, row := range h.view.rows {
		reply = reply || (row.Type == "agent_message" && strings.Contains(strings.ToLower(row.Text), "rain"))
	}
	if !reply {
		t.Fatalf("saved rows after handoff = %+v, want the reply", h.view.rows)
	}
}

// A prompt that reaches Claude mid-turn is saved only as a queued_command
// attachment: its own uuid is random, and Nano's submission id is its
// source_uuid. The fixture line is a real one (Claude 2.1.285), scrubbed and
// placed inside the fixture turn. Without a saved row carrying that id, the
// live prompt stayed at the end of the pane forever.
func TestLiveTextQueuedPromptLeavesOnItsSavedRow(t *testing.T) {
	const queuedPrompt = "22222222-3333-4444-8555-666666666666"
	h := newLiveTextHarness(t)
	saved := fixtureLines(t, "claude-turn.saved.jsonl")
	thinkingLine := lineWith(saved, `"thinking"`)
	h.save(saved[:thinkingLine+1])
	queued := prompts.OutboxEntry{
		HostedThreadID: h.sess.HostedThreadID(), ActionID: "action-2", Sequence: 2,
		PresentationText: "Also mention snow.", State: prompts.Accepted,
		Binding: &prompts.Binding{EventID: queuedPrompt},
	}
	h.view.expectSaved["prompt:action-2"] = queued.PresentationText
	h.sess.view.PublishLiveOutbox(queued)
	h.expect("queued prompt", func(LiveTextMessage) bool { return containsEntry(h.view.live, "prompt:action-2") })
	h.save(fixtureLines(t, "claude-queued-prompt.jsonl"))
	h.expect("queued prompt saved", func(LiveTextMessage) bool { return !containsEntry(h.view.live, "prompt:action-2") })
}

// The browser shows its own send until the stream lists the prompt or names
// it saved. A prompt whose saved row is in the file before Nano admits it is
// never listed live, so the message that settles it must name it saved.
func TestLiveTextNamesAPromptSavedBeforeItWasShown(t *testing.T) {
	const prompt = "22222222-3333-4444-8555-666666666666"
	h := newLiveTextHarness(t)
	saved := fixtureLines(t, "claude-turn.saved.jsonl")
	h.save(saved[:lineWith(saved, `"thinking"`)+1])
	h.save(fixtureLines(t, "claude-queued-prompt.jsonl"))
	h.expect("the prompt's row saved", func(LiveTextMessage) bool {
		for _, row := range h.view.rows {
			if row.Type == "user_message" && row.Text == "Also mention snow." {
				return true
			}
		}
		return false
	})
	entry := prompts.OutboxEntry{
		HostedThreadID: h.sess.HostedThreadID(), ActionID: "action-2", Sequence: 2,
		PresentationText: "Also mention snow.", State: prompts.Accepted,
		Binding: &prompts.Binding{EventID: prompt},
	}
	h.view.expectSaved["prompt:action-2"] = entry.PresentationText
	h.sess.view.PublishLiveOutbox(entry)
	h.expect("the prompt named saved", func(m LiveTextMessage) bool { return slices.Contains(m.Saved, "prompt:action-2") })
	if h.view.shown["prompt:action-2"] {
		t.Fatal("a prompt saved before Nano admitted it was listed live")
	}
}

// A pane that reconnects after a send of its own was saved, while the live
// loop forgot it (it restarts when idle), must get the word in the tail that
// installs the row, and only for rows that tail holds: a prompt the store
// settled whose row is not in the page (saved after the page was read, say)
// must not be named, or the browser's copy would vanish before its row.
func TestLiveTextTailNamesPromptsWhoseSavedRowsItHolds(t *testing.T) {
	ctx := context.Background()
	h := newLiveTextHarness(t)
	native := h.sess.stateOwner.ThreadID()
	settle := func(action, event string) {
		t.Helper()
		text, turn := "prompt "+action, "claude:"+action
		if _, _, err := h.sess.delivery.PromptStore().Submit(ctx, prompts.Submission{HostedThreadID: h.sess.HostedThreadID(), Provider: model.ProviderClaude, NativeThreadID: native,
			ActionID: action, ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := h.sess.delivery.PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: h.sess.HostedThreadID(), Token: "claim-" + action, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", action, claimed, err)
		}
		if _, err := h.sess.delivery.PromptStore().Accepted(ctx, prompts.Acceptance{ActionID: action, ClaimToken: "claim-" + action, AttachmentEpoch: 1,
			Binding: prompts.Binding{EventID: event, Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}}); err != nil {
			t.Fatal(err)
		}
		if _, settled, err := h.sess.delivery.PromptStore().Settle(ctx, action); err != nil || !settled {
			t.Fatalf("settle %s: %v %v", action, settled, err)
		}
	}
	saved := fixtureLines(t, "claude-turn.saved.jsonl")
	h.save(saved[:lineWith(saved, `"thinking"`)+1])
	h.save(fixtureLines(t, "claude-queued-prompt.jsonl"))
	h.expect("the prompt's row saved", func(LiveTextMessage) bool {
		for _, row := range h.view.rows {
			if row.Type == "user_message" && row.Text == "Also mention snow." {
				return true
			}
		}
		return false
	})
	settle("in-page", "22222222-3333-4444-8555-666666666666")
	settle("not-in-page", "99999999-3333-4444-8555-666666666666")

	later := &liveTextViewer{t: t, pane: newLiveTextPane(threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40}),
		view: &livePaneView{expectSaved: map[string]string{}, neverSaved: map[string]bool{}}}
	liveTextOf(h.sess).subscribe(later.pane)
	later.expect("its tail names the prompt whose row it holds, and only that one", func(m LiveTextMessage) bool {
		if m.Type != "tail" {
			return false
		}
		if !slices.Contains(m.Saved, "prompt:in-page") || slices.Contains(m.Saved, "prompt:not-in-page") {
			t.Fatalf("tail saved=%v, want prompt:in-page named and prompt:not-in-page not", m.Saved)
		}
		return true
	})
}

// A tail that cannot learn which of its rows settle a prompt must not go out
// without that word: the browser would show its own copy beside the row. The
// pane fails visibly, as for an unreadable saved transcript.
func TestLiveTextTailFailsWhenItCannotNameItsSavedPrompts(t *testing.T) {
	h := newLiveTextHarness(t)
	saved := fixtureLines(t, "claude-turn.saved.jsonl")
	h.save(saved[:lineWith(saved, `"thinking"`)+1])
	h.expect("rows saved", func(LiveTextMessage) bool { return len(h.view.rows) > 0 })
	if err := h.queue.Database().Close(); err != nil {
		t.Fatal(err)
	}
	later := &liveTextViewer{t: t, pane: newLiveTextPane(threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40}),
		view: &livePaneView{expectSaved: map[string]string{}, neverSaved: map[string]bool{}}}
	liveTextOf(h.sess).subscribe(later.pane)
	later.expect("the pane fails", func(m LiveTextMessage) bool {
		if m.Type == "tail" {
			t.Fatalf("a tail went out without its saved prompts: saved=%v", m.Saved)
		}
		return m.Type == "error"
	})
}

// Claude can merge prompts that queued together into one saved user row, one
// text block per prompt: the row's uuid is one send's id, and
// mergedSendUuids names the others'. Each prompt's live row must leave with
// that saved row; the merged ones stayed at the end of the pane for good. The
// fixture is a real merged row's shape (Claude 2.1.300), scrubbed.
func TestLiveTextMergedPromptsLeaveOnTheirSavedRow(t *testing.T) {
	h := newLiveTextHarness(t)
	h.save(fixtureLines(t, "claude-turn.saved.jsonl"))
	sent := []struct{ action, event, text string }{
		{"merged-first", "22be6161-80bb-51df-b5a3-b18279bc0951", "Now write about snow."},
		{"merged-second", "5d2d3d1c-7a90-59ce-b58e-3ff630a864b4", "Also mention hail."},
		{"merged-third", "ad97b687-aedd-510c-acd4-6b9fc9924aff", "Keep it to one line."},
	}
	for i, prompt := range sent {
		entry := prompts.OutboxEntry{
			HostedThreadID: h.sess.HostedThreadID(), ActionID: prompt.action, Sequence: uint64(i + 2),
			PresentationText: prompt.text, State: prompts.Accepted, Binding: &prompts.Binding{EventID: prompt.event},
		}
		h.view.expectSaved["prompt:"+prompt.action] = prompt.text
		h.sess.view.PublishLiveOutbox(entry)
	}
	h.expect("merged prompts", func(LiveTextMessage) bool {
		return containsEntry(h.view.live, "prompt:merged-first") && containsEntry(h.view.live, "prompt:merged-third")
	})
	h.save(fixtureLines(t, "claude-merged-prompts.jsonl"))
	h.expect("merged prompts saved", func(LiveTextMessage) bool {
		for _, prompt := range sent {
			if containsEntry(h.view.live, "prompt:"+prompt.action) {
				return false
			}
		}
		return true
	})
}

// Live text is checked against the saved file before any pane sees it, by
// exact identity alone: a reply the provider resends after a reconnect, saved
// before the pane's page or carrying only a counted block index, and a prompt
// whose outbox change arrives after its row was saved never show; a prompt
// with equal text but another identity does.
func TestLiveTextMatchesOnArrivalAndOnlyByExactIdentity(t *testing.T) {
	// A one-row page, and a later turn saved after the reply: the reply is not
	// on the page.
	query := threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 1}
	h := newLiveTextHarnessWith(t, query)
	later := fixtureLines(t, "claude-tool-error.saved.jsonl")
	h.save(append(fixtureLines(t, "claude-turn.saved.jsonl"), later...))
	h.waitSaved("msg_6289003a54e45a93b0f31403ba5b1278:1")
	for _, row := range h.page(query) {
		if row.NativeItemID == fixtureReply {
			t.Fatalf("page = %+v, want the first turn's reply off it", h.page(query))
		}
	}
	h.sess.view.PublishLiveOutbox(prompts.OutboxEntry{
		HostedThreadID: h.sess.HostedThreadID(), ActionID: "action-1", Sequence: 1,
		PresentationText: "Reply with two short sentences about rain.", State: prompts.Accepted,
		Binding: &prompts.Binding{EventID: liveTextFixturePrompt},
	})
	h.sess.view.PublishLiveOutbox(prompts.OutboxEntry{
		HostedThreadID: h.sess.HostedThreadID(), ActionID: "action-2", Sequence: 2,
		PresentationText: "Reply with two short sentences about rain.", State: prompts.Accepted,
		Binding: &prompts.Binding{EventID: "99999999-2222-4333-8444-555555555555"},
	})
	// Resent as a stream the reply's identity is exact and already saved.
	// Resent after a reconnect as one bare frame, its block index is only
	// counted, so it is not exact.
	wire := fixtureLines(t, "claude-turn.wire.jsonl")
	h.feed(wire)
	// Reconstruct the real projection as a reconnect does, retaining the
	// existing live loop whose saved/live identity boundary this test observes.
	turn := h.projection.currentTurnID()
	h.projection = NewLiveProjection("claude", h.sess.HostedThreadID(), false)
	h.projection.setNativeThreadID(h.sess.stateOwner.ThreadID())
	h.projection.setClaudeProviderTurnID(turn)
	h.projection.setLiveText(h.sess.view.LiveTextService())
	for _, line := range wire {
		if strings.HasPrefix(string(line), `{"type": "assistant"`) && strings.Contains(string(line), `"type": "text"`) {
			h.feed([][]byte{line})
		}
	}
	h.barrier()
	for id := range h.view.shown {
		if strings.HasPrefix(id, "reply:") {
			t.Fatalf("saved reply %s was shown as live text", id)
		}
	}
	for _, id := range []string{"prompt:action-1"} {
		if h.view.shown[id] {
			t.Fatalf("saved %s was shown as live text", id)
		}
	}
	if len(h.view.live) != 1 || h.view.live[0].ID != "prompt:action-2" {
		t.Fatalf("live = %+v, want only the unsaved equal-text prompt", h.view.live)
	}
}

// An already-saved reply that arrives again is dropped unseen and proves no
// later turn: an ended reply of a newer turn that is not saved yet keeps its
// live text, however late the replay arrives.
func TestLiveTextReplayProvesNoLaterTurn(t *testing.T) {
	h := newLiveTextHarness(t)
	h.save(fixtureLines(t, "claude-tool-error.saved.jsonl"))
	h.waitSaved("msg_6289003a54e45a93b0f31403ba5b1278:1")
	wire := fixtureLines(t, "claude-turn.wire.jsonl")
	h.projection.setClaudeProviderTurnID("claude:" + h.sess.HostedThreadID() + ":2")
	h.feed(wire)
	h.expect("ended reply", func(LiveTextMessage) bool { return h.view.reply() != nil && h.view.reply().State == "done" })
	// The same frames, carrying the saved reply's identity: a replay.
	var replay [][]byte
	for _, line := range wire {
		replay = append(replay, []byte(strings.ReplaceAll(string(line), "msg_fc823e6c7ddd5dd584d9bd9b60795fa8", "msg_6289003a54e45a93b0f31403ba5b1278")))
	}
	h.projection.setClaudeProviderTurnID("claude:" + h.sess.HostedThreadID() + ":3")
	h.feed(replay)
	h.barrier()
	if h.view.shown["reply:msg_6289003a54e45a93b0f31403ba5b1278:1"] {
		t.Fatal("the saved reply's replay was shown as live text")
	}
	if h.view.reply() == nil || h.view.reply().ID != "reply:"+fixtureReply {
		t.Fatalf("live = %+v, want the newer turn's unsaved reply kept", h.view.live)
	}
}

// A reply that finished but is never saved keeps its live text until the
// saved row of the next prompt, delivered after it ended, also across a
// reload. A save window after it finished it is recorded for investigation,
// whatever the panes show by then. A reply saved in time is not, even before
// any pane has its row; nor is a Stop-cut reply, which never finished.
func TestLiveTextUnsavedReplyStaysUntilNextPrompt(t *testing.T) {
	t.Parallel() // each case waits out the real save window
	wire := fixtureLines(t, "claude-turn.wire.jsonl")
	turn := fixtureLines(t, "claude-turn.saved.jsonl")
	reply := "reply:" + fixtureReply
	unsaved := func(h *liveTextHarness) []diagnostics.Event {
		return h.diagnostics.Recent(10, "", "", "file.reply.unsaved")
	}
	finished := func(h *liveTextHarness) func(LiveTextMessage) bool {
		return func(LiveTextMessage) bool { return h.view.reply() != nil && h.view.reply().State == "done" }
	}

	// A Stop-cut reply: the stream stops after a few text chunks, and the
	// turn's result follows.
	cut := newLiveTextHarness(t)
	cut.view.neverSaved[reply] = true
	cut.projection.setClaudeProviderTurnID("claude:" + cut.sess.HostedThreadID() + ":1")
	cut.feed(append(append([][]byte(nil), wire[:lineWith(wire, `"type": "text_delta"`)+3]...), wire[len(wire)-1]))
	cut.expect("cut reply", finished(cut))

	// Saved in time, while the loop has not heard of the save.
	inTime := newLiveTextHarness(t)
	inTime.hold()
	inTime.projection.setClaudeProviderTurnID("claude:" + inTime.sess.HostedThreadID() + ":1")
	inTime.feed(wire)
	inTime.expect("finished reply", finished(inTime))
	inTime.save(turn)
	inTime.waitSaved(fixtureReply)

	// A Codex reply whose start the loop missed, as after a restart, never
	// shows; finished and never saved, it is still recorded.
	missed := newLiveTextHarnessFor(t, "codex", threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40})
	var codexTurn [][]byte
	for _, line := range fixtureLines(t, "codex-turns.wire.jsonl") {
		if !strings.Contains(string(line), `"item/started","params":{"item":{"type":"agentMessage"`) {
			codexTurn = append(codexTurn, line)
		}
		if strings.Contains(string(line), `"turn/completed"`) {
			break
		}
	}
	missed.feed(codexTurn)
	missed.barrier()
	if missed.view.shown["reply:"+codexFixtureReply1] {
		t.Fatal("a Codex reply whose start was missed showed as live text")
	}

	// Never saved: the turn's prompt is saved, its reply never is.
	h := newLiveTextHarness(t)
	h.view.neverSaved[reply] = true
	h.save(turn[:lineWith(turn, liveTextFixturePrompt)+1])
	h.expect("prompt row", func(LiveTextMessage) bool { return len(h.view.rows) == 1 })
	h.projection.setClaudeProviderTurnID("claude:" + h.sess.HostedThreadID() + ":1")
	h.feed(wire)
	h.expect("finished reply", finished(h))
	reloaded := h.open(threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40})
	reloaded.view.neverSaved[reply] = true
	if reloaded.view.reply() == nil {
		t.Fatal("a reload hid the unsaved reply")
	}
	// The next prompt, delivered after the reply ended, is saved: the reply
	// leaves every pane with that row.
	next := prompts.OutboxEntry{HostedThreadID: h.sess.HostedThreadID(), ActionID: "next", Sequence: 2,
		PresentationText: "next", State: prompts.Accepted, Binding: &prompts.Binding{EventID: "22222222-3333-4444-8555-666666666666"}}
	h.sess.view.PublishLiveOutbox(next)
	h.expect("next prompt", func(LiveTextMessage) bool { return containsEntry(h.view.live, "prompt:next") })
	saved := fixtureLines(t, "claude-tool-error.saved.jsonl")
	h.save(saved[:lineWith(saved, "22222222-3333-4444-8555-666666666666")+1])
	h.expect("next prompt saved", func(m LiveTextMessage) bool {
		if h.view.reply() != nil {
			return false
		}
		if m.Type != "update" || len(m.Rows) == 0 || containsEntry(h.view.live, "prompt:next") || slices.Contains(m.Saved, reply) {
			t.Fatalf("the reply left as %q with %d rows, live %+v and saved %v, want the update that saves the next prompt, not the reply",
				m.Type, len(m.Rows), h.view.live, m.Saved)
		}
		return true
	})
	reloaded.expect("next prompt saved", func(LiveTextMessage) bool { return reloaded.view.reply() == nil })

	// The save window passes.
	for deadline := time.Now().Add(liveTextSaveWindow + 10*time.Second); len(unsaved(h)) == 0 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if events := unsaved(h); len(events) != 1 || events[0].Code != fixtureReply {
		t.Fatalf("unsaved diagnostics = %+v, want one for %s", events, fixtureReply)
	}
	// Their windows passed first; a barrier waits for their checks.
	for name, other := range map[string]*liveTextHarness{"a Stop-cut reply": cut, "a reply saved in time": inTime} {
		other.barrier()
		if events := unsaved(other); len(events) != 0 {
			t.Fatalf("%s was recorded as finished but unsaved: %+v", name, events)
		}
	}
	missed.barrier()
	if events := unsaved(missed); len(events) != 1 || events[0].Code != codexFixtureReply1 {
		t.Fatalf("missed-start Codex diagnostics = %+v, want one for %s", events, codexFixtureReply1)
	}
}

// barrier waits until the loop has handled everything posted before it: the
// loop applies its inbox in arrival order, so a queued prompt posted now shows
// only after every earlier event. The prompt is then refused and gone.
func (h *liveTextHarness) barrier() {
	h.t.Helper()
	h.barriers++
	id := fmt.Sprintf("barrier-%d", h.barriers)
	entry := prompts.OutboxEntry{HostedThreadID: h.sess.HostedThreadID(), ActionID: id,
		Sequence: 99, PresentationText: "barrier", State: prompts.Queued}
	h.sess.view.PublishLiveOutbox(entry)
	h.expect("barrier", func(LiveTextMessage) bool { return containsEntry(h.view.live, "prompt:"+id) })
	entry.State = prompts.Refused
	h.sess.view.PublishLiveOutbox(entry)
	h.expect("barrier gone", func(LiveTextMessage) bool { return !containsEntry(h.view.live, "prompt:"+id) })
}

// A file-only pane takes its status (Working, the turn, approval cards) from
// its own stream, where the stream handler alone writes it: first, before any
// rows, then each change in revision order. On the shared page feed, a Stop's
// status was lost when the pane's hidden rows stalled and it read Working.
func TestLiveTextStreamCarriesStatusFirst(t *testing.T) {
	h := newLiveTextHarness(t)
	h.save(fixtureLines(t, "claude-turn.saved.jsonl"))
	h.waitSaved(fixtureReply)
	stream := httptest.NewServer(http.HandlerFunc(h.sess.view.HandleLiveStream))
	defer stream.Close()
	response, err := http.Get(stream.URL + "?view=enhanced&limit=40")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	lines := bufio.NewScanner(response.Body)
	next := func() LiveTextMessage {
		for lines.Scan() {
			if data, ok := strings.CutPrefix(lines.Text(), "data: "); ok {
				var message LiveTextMessage
				if err := json.Unmarshal([]byte(data), &message); err != nil {
					t.Fatal(err)
				}
				return message
			}
		}
		t.Fatal("the stream ended")
		return LiveTextMessage{}
	}
	first := next()
	if first.Type != "status" || first.Status == nil || first.Status.Runtime.TurnActive || len(first.Rows) > 0 {
		t.Fatalf("first message is %q (status %+v, %d rows), want an idle status with no rows", first.Type, first.Status, len(first.Rows))
	}
	revision := first.Status.Revision
	expect := func(want string) {
		t.Helper()
		for {
			message := next()
			if message.Type != "status" {
				if message.Status != nil {
					t.Fatalf("%s message carries status", message.Type)
				}
				continue
			}
			got := fmt.Sprintf("working=%t cards=%d", message.Status.Runtime.TurnActive, len(message.Status.Interactions))
			if message.Status.Revision <= revision || len(message.Rows) > 0 || got != want {
				t.Fatalf("status %s at revision %d after %d with %d rows, want %s at a later revision and no rows", got, message.Status.Revision, revision, len(message.Rows), want)
			}
			revision = message.Status.Revision
			return
		}
	}
	h.projection.updateRuntime(liveview.Runtime{SessionStatus: "ready", TurnActive: true, TurnID: "turn-2"})
	expect("working=true cards=0")
	h.projection.upsertInteraction(interact.PendingInteraction{ID: "ask-1", Kind: "question", CreatedAt: "2026-10-03T00:00:00Z"})
	expect("working=true cards=1")
	// Live rows change the projection too, with compact row records that
	// carry no status; they must not read as a turn ending or a card leaving.
	h.feed(fixtureLines(t, "claude-turn.wire.jsonl"))
	h.projection.removeInteraction("ask-1")
	expect("working=true cards=0")
	h.projection.updateRuntime(liveview.Runtime{SessionStatus: "ready"})
	expect("working=false cards=0")
}

// The projection drops a subscriber that falls behind; the stream must then
// end, so the browser reconnects to a fresh status instead of keeping a stale
// one.
func TestLiveTextStreamEndsWhenStatusLags(t *testing.T) {
	h := newLiveTextHarness(t)
	writer, done := h.blockedStream()
	for i := range 300 {
		h.projection.updateError(fmt.Sprintf("status %d", i))
	}
	close(writer.release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream kept running after its status subscription fell behind")
	}
}

// Updates queued behind a slow write are each forwarded at their own
// revision. Rereading the latest status instead skipped a start and Stop that
// both landed while the stream was busy, and with them the turn flips that
// refresh the delivery record.
func TestLiveTextStreamKeepsEveryQueuedFlip(t *testing.T) {
	h := newLiveTextHarness(t)
	writer, done := h.blockedStream()
	h.projection.updateRuntime(liveview.Runtime{SessionStatus: "ready", TurnActive: true, TurnID: "turn-2"})
	h.projection.updateRuntime(liveview.Runtime{SessionStatus: "ready"})
	close(writer.release)
	deadline := time.Now().Add(10 * time.Second)
	for len(writer.statuses()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	writer.cancel()
	<-done
	var working []bool
	for _, status := range writer.statuses() {
		working = append(working, status.Runtime.TurnActive)
	}
	if !slices.Equal(working, []bool{false, true, false}) {
		t.Fatalf("status working sequence = %v, want idle, the start, then the Stop", working)
	}
}

// blockedStream runs the pane stream into a writer that holds its first write
// until released, so updates queue behind it.
func (h *liveTextHarness) blockedStream() (*streamRecorder, chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	writer := &streamRecorder{header: http.Header{}, wrote: make(chan struct{}), release: make(chan struct{}), cancel: cancel}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.sess.view.HandleLiveStream(writer, httptest.NewRequest("GET", "/live?view=enhanced&limit=40", nil).WithContext(ctx))
	}()
	<-writer.wrote
	h.t.Cleanup(cancel)
	return writer, done
}

// streamRecorder is a pane stream's client: it holds the first write until
// released, and keeps what it received.
type streamRecorder struct {
	header  http.Header
	once    atomic.Bool
	wrote   chan struct{}
	release chan struct{}
	cancel  context.CancelFunc
	mu      sync.Mutex
	body    strings.Builder
}

func (w *streamRecorder) Header() http.Header { return w.header }

func (w *streamRecorder) WriteHeader(int) {}

func (w *streamRecorder) Flush() {}

func (w *streamRecorder) Write(b []byte) (int, error) {
	if w.once.CompareAndSwap(false, true) {
		close(w.wrote)
		<-w.release
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(b)
}

func (w *streamRecorder) statuses() []liveview.Status {
	w.mu.Lock()
	body := w.body.String()
	w.mu.Unlock()
	var statuses []liveview.Status
	for _, line := range strings.Split(body, "\n") {
		var message LiveTextMessage
		if data, ok := strings.CutPrefix(line, "data: "); ok && json.Unmarshal([]byte(data), &message) == nil && message.Status != nil {
			statuses = append(statuses, *message.Status)
		}
	}
	return statuses
}

// A rebuild gives the saved rows a new projection key: an open pane gets one
// fresh tail instead of a patch, and its live text survives the swap. Live
// text the rebuilt file saves leaves with the tail, which names it saved so
// the browser records its lag.
func TestLiveTextRebuildSendsFreshTail(t *testing.T) {
	h := newLiveTextHarness(t)
	h.save(fixtureLines(t, "claude-tool-error.saved.jsonl"))
	h.expect("saved rows", func(LiveTextMessage) bool { return len(h.view.rows) >= 2 })
	bound := prompts.OutboxEntry{HostedThreadID: h.sess.HostedThreadID(), ActionID: "bound", Sequence: 1,
		PresentationText: "Reply with two short sentences about rain.", State: prompts.Accepted,
		Binding: &prompts.Binding{EventID: liveTextFixturePrompt}}
	h.view.expectSaved["prompt:bound"] = bound.PresentationText
	h.sess.view.PublishLiveOutbox(bound)
	h.sess.view.PublishLiveOutbox(prompts.OutboxEntry{HostedThreadID: h.sess.HostedThreadID(), ActionID: "next",
		Sequence: 2, PresentationText: "next prompt", State: prompts.Queued})
	h.expect("prompts", func(LiveTextMessage) bool { return len(h.view.live) == 2 })
	// Rewriting the file in place (new inode) is a new source generation: the
	// catalog rebuilds the projection under a new key.
	if err := os.Remove(h.file); err != nil {
		t.Fatal(err)
	}
	h.save(fixtureLines(t, "claude-turn.saved.jsonl"))
	var saved []string
	h.expect("fresh tail", func(m LiveTextMessage) bool {
		saved = m.Saved
		return m.Type == "tail"
	})
	if len(h.view.rows) < 2 || len(h.view.live) != 1 || h.view.live[0].ID != "prompt:next" {
		t.Fatalf("after rebuild rows=%d live=%+v, want the saved rows and the queued prompt", len(h.view.rows), h.view.live)
	}
	if !slices.Contains(saved, "prompt:bound") {
		t.Fatalf("the rebuild tail names %v saved, want the prompt it saved", saved)
	}
	// A tail larger than a pane's backlog limit still lands.
	h.sess.view.PublishLiveOutbox(prompts.OutboxEntry{HostedThreadID: h.sess.HostedThreadID(), ActionID: "large",
		Sequence: 3, PresentationText: strings.Repeat("x", liveTextPaneBacklog+1), State: prompts.Queued})
	reopened := h.open(threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40})
	if !containsEntry(reopened.view.live, "prompt:large") {
		t.Fatalf("reopened pane live = %d entries, want the large queued prompt", len(reopened.view.live))
	}
	// A failed read is shown, and the stream ends instead of retrying.
	h.store.Close()
	h.sess.view.NotifySavedTranscript()
	reopened.expect("read error", func(m LiveTextMessage) bool { return m.Type == "error" })
	if !reopened.pane.isClosed() {
		t.Fatal("the pane stream stayed open after its read failed")
	}
}

// Queued prompts show in the order delivery claims them: a human prompt
// queued after an agent's goes first.
func TestLiveTextShowsQueueInDeliveryOrder(t *testing.T) {
	h := newLiveTextHarness(t)
	for _, origin := range []prompts.Origin{prompts.AgentOrigin, prompts.HumanOrigin} {
		record, _, err := h.sess.delivery.PromptStore().Submit(context.Background(), prompts.Submission{
			HostedThreadID: h.sess.HostedThreadID(), Provider: "claude", ActionID: string(origin),
			ProviderText: string(origin), PresentationText: string(origin), TextHash: string(origin),
			RequestHash: string(origin), Origin: origin,
		})
		if err != nil {
			t.Fatal(err)
		}
		h.sess.view.PublishLiveOutbox(record.Outbox)
	}
	h.expect("queue", func(LiveTextMessage) bool { return len(h.view.live) == 2 })
	if h.view.live[0].ID != "prompt:human" {
		t.Fatalf("queue = %+v, want the human prompt first", h.view.live)
	}

	// A prompt whose delivery is uncertain shows as uncertain until its saved
	// row settles it (or it expires: TestLiveTextShowsNoExpiredPrompt).
	uncertain := prompts.OutboxEntry{
		HostedThreadID: h.sess.HostedThreadID(), ActionID: "action-uncertain", Sequence: 3,
		PresentationText: "Reply with two short sentences about rain.", State: prompts.Uncertain,
		Binding: &prompts.Binding{EventID: liveTextFixturePrompt},
	}
	h.view.expectSaved["prompt:action-uncertain"] = uncertain.PresentationText
	h.sess.view.PublishLiveOutbox(uncertain)
	h.expect("uncertain prompt", func(LiveTextMessage) bool {
		for _, entry := range h.view.live {
			if entry.ID == "prompt:action-uncertain" {
				return entry.State == string(prompts.Uncertain)
			}
		}
		return false
	})
	h.save(fixtureLines(t, "claude-turn.saved.jsonl"))
	h.expect("uncertain prompt saved", func(LiveTextMessage) bool { return !containsEntry(h.view.live, "prompt:action-uncertain") })
}

// A failed Claude tool result is part of the agent's work while its turn runs,
// so a category profile that shows errors but hides tool results hides it.
// Claude saves the final message's thinking block, marked end_turn, before
// its answer: the turn then reads as ended without an answer and a failed
// non-shell tool shows as an error. Once the answer is saved the row is
// revised in place to a tool result again, and the open pane removes it. A
// failed shell command never shows: the profile hides every shell command.
// The fixture is a real failed Bash call; the non-shell case renames its tool.
// Default Enhanced shows messages only, so the pane uses that custom profile.
func TestLiveTextRevisedRowLeavesItsProfile(t *testing.T) {
	for _, tool := range []string{"Bash", "Read"} {
		t.Run(tool, func(t *testing.T) {
			h := newLiveTextHarnessWith(t, threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40, HiddenSpecified: true,
				HiddenCategories: []string{"reasoning", "toolCall", "toolResult", "system", "unclassified"}})
			saved := fixtureLines(t, "claude-tool-error.saved.jsonl")
			if tool != "Bash" {
				for i, line := range saved {
					saved[i] = bytes.ReplaceAll(line, []byte(`"name":"Bash"`), []byte(`"name":"`+tool+`"`))
				}
			}
			result := lineWith(saved, `"is_error":true`)
			ended := lineWith(saved, `"stop_reason":"end_turn"`)
			h.save(saved[:result+1])
			for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
				rows := h.page(threadindex.TranscriptQuery{View: threadindex.DevThreadTranscriptView, HiddenSpecified: true, Limit: 40})
				if len(rows) > 0 && rows[len(rows)-1].Type == "tool_result" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the failed tool result was never saved")
				}
			}
			h.barrier()
			errorRows := func() (ids []string) {
				for _, row := range h.view.rows {
					if row.Category == "error" {
						ids = append(ids, row.ID)
					}
				}
				return ids
			}
			if ids := errorRows(); len(ids) > 0 {
				t.Fatalf("while the turn runs the pane shows %v as an error", ids)
			}
			h.save(saved[result+1 : ended+1])
			if tool == "Bash" {
				// The turn now reads as ended without an answer.
				for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
					rows := h.page(threadindex.TranscriptQuery{View: threadindex.DevThreadTranscriptView, HiddenSpecified: true, Limit: 40})
					if len(rows) > 0 && rows[len(rows)-1].Type == "reasoning" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("the final thinking was never saved")
					}
				}
				h.barrier()
				if ids := errorRows(); len(ids) > 0 {
					t.Fatalf("the failed shell command shows as an error: %v", ids)
				}
				return
			}
			var errorRow string
			h.expect("error row", func(LiveTextMessage) bool {
				if ids := errorRows(); len(ids) > 0 {
					errorRow = ids[0]
				}
				return errorRow != ""
			})
			h.save(saved[ended+1:])
			h.expect("turn answered", func(LiveTextMessage) bool {
				for _, row := range h.view.rows {
					if row.ID == errorRow {
						return false
					}
				}
				return true
			})
			if last := h.view.rows[len(h.view.rows)-1]; last.Type != "agent_message" {
				t.Fatalf("rows after the turn = %+v, want its final reply last", h.view.rows)
			}
		})
	}
}

// Codex hands a prompt over by the turn it started and a reply by its item
// id. A reply Stop cut is never saved: its text stays through applies, and a
// reload keeps it until the file records its turn's end; a reload after that
// clears it from every pane. A thread an older Codex CLI created (0.144 in the
// captured case) still saves each message first as an event without its item
// id, then as the item that carries it; its replies hand over the same way.
func TestLiveTextCodexHandsOverByTurnAndItem(t *testing.T) {
	for _, file := range []string{"codex-turns.saved.jsonl", "codex-turns-legacy.saved.jsonl"} {
		t.Run(file, func(t *testing.T) {
			query := threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40}
			h := newLiveTextHarnessFor(t, "codex", query)
			saved := fixtureLines(t, file)
			var turns [][][]byte
			var frames [][]byte
			for _, line := range fixtureLines(t, "codex-turns.wire.jsonl") {
				frames = append(frames, line)
				if strings.Contains(string(line), `"turn/completed"`) {
					turns, frames = append(turns, frames), nil
				}
			}
			thread := codexFixtureThread
			submit := func(action, text, turn string) {
				// The fixture's prompts were system prompts: Codex saved the footer
				// Nano delivers with them.
				h.view.expectSaved["prompt:"+action] = text + "\n\n[from nano]"
				// Delivery binds a Codex prompt to its turn only once the turn starts.
				entry := prompts.OutboxEntry{HostedThreadID: h.sess.HostedThreadID(), ActionID: action, Sequence: 1, PresentationText: text, State: prompts.Delivering}
				h.sess.view.PublishLiveOutbox(entry)
				entry.State = prompts.Accepted
				entry.Binding = &prompts.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: "codex", NativeThreadID: &thread, NativeTurnID: &turn}}
				h.sess.view.PublishLiveOutbox(entry)
			}
			ended := func(v *livePaneView) func(LiveTextMessage) bool {
				return func(LiveTextMessage) bool { return v.reply() != nil && v.reply().State == "done" }
			}
			submit("action-1", "Reply with two short sentences about rain.", codexFixtureTurn1)
			h.feed(turns[0])
			h.expect("finished reply", ended(h.view))
			h.save(saved[:lineWith(saved, `"task_complete"`)+1])
			h.expect("turn 1 saved", func(LiveTextMessage) bool { return len(h.view.live) == 0 })
			if !h.view.hasNative(codexFixtureReply1) {
				t.Fatalf("rows = %+v, want the first reply saved", h.view.rows)
			}

			stopped := "reply:" + codexFixtureStopped
			h.view.neverSaved[stopped] = true
			submit("action-2", "Write 80 numbered lines. Line N must be CODEX_STOP_NN for NN 01 through 80. Do not use tools.", codexFixtureTurn2)
			h.feed(turns[1])
			h.expect("stopped reply", ended(h.view))
			aborted := lineWith(saved, `"turn_aborted"`)
			h.save(saved[lineWith(saved, `"task_complete"`)+1 : aborted])
			h.expect("prompt 2 saved", func(LiveTextMessage) bool { return !containsEntry(h.view.live, "prompt:action-2") })
			early := h.open(query)
			early.view.neverSaved[stopped] = true
			if h.view.reply() == nil || early.view.reply() == nil {
				t.Fatal("the stopped reply left before the file recorded its turn's end")
			}
			h.save(saved[aborted : aborted+1])
			h.waitTurnEnded(codexFixtureTurn2)
			h.barrier()
			if h.view.reply() == nil {
				t.Fatal("an apply cleared the stopped reply without a reload")
			}
			reloaded := h.open(query)
			if reloaded.view.reply() != nil {
				t.Fatalf("a reload after the turn's recorded end still shows the stopped reply: %+v", reloaded.view.live)
			}
			early.expect("stopped reply cleared", func(LiveTextMessage) bool { return early.view.reply() == nil })

			submit("action-3", "Reply with exactly: CODEX_AFTER_STOP", codexFixtureTurn3)
			h.feed(turns[2])
			h.save(saved[aborted+1:])
			h.expect("turn 3 saved", func(LiveTextMessage) bool { return len(h.view.live) == 0 && h.view.hasNative(codexFixtureReply3) })
		})
	}
}

// A correction steered into a running Codex turn shares the turn with the
// prompt that started it, so a turn key no longer names one saved user row.
// A prompt with a client id leaves on the row carrying that id; a prompt
// delivered before client ids (a turn spanning the deploy) keeps its turn key
// and leaves on the turn's one row that no stored prompt claims by id. A
// third prompt in the turn still matches, and that holds after a restart too,
// when a correction is already confirmed and no longer live. While the stored
// claims cannot be read, the legacy prompt stays: the turn's only saved row
// may be another prompt's. None may stay beside its saved row or leave
// without it.
func TestLiveTextCodexPromptsSharingATurnLeaveOnTheirOwnRows(t *testing.T) {
	for _, mode := range []string{"live", "restarted", "unreadable claims"} {
		t.Run(mode, func(t *testing.T) {
			restarted := mode != "live"
			ctx := context.Background()
			query := threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40}
			h := newLiveTextHarnessFor(t, "codex", query)
			thread, turn := codexFixtureThread, "01a0f63e-0000-7000-8000-00000000a001"
			accept := func(action, text, event string) prompts.OutboxEntry {
				if _, _, err := h.sess.delivery.PromptStore().Submit(ctx, prompts.Submission{HostedThreadID: h.sess.HostedThreadID(), Provider: model.ProviderCodex, NativeThreadID: thread,
					ActionID: action, ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
					t.Fatal(err)
				}
				if _, claimed, err := h.sess.delivery.PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: h.sess.HostedThreadID(), Token: "claim-" + action, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
					t.Fatalf("claim %s: %v %v", action, claimed, err)
				}
				record, err := h.sess.delivery.PromptStore().Accepted(ctx, prompts.Acceptance{ActionID: action, ClaimToken: "claim-" + action, AttachmentEpoch: 1,
					Binding: prompts.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &thread, NativeTurnID: &turn}, EventID: event}})
				if err != nil {
					t.Fatal(err)
				}
				return record.Outbox
			}
			pair := func(n int, text, clientID string) [][]byte {
				client := ""
				if clientID != "" {
					client = fmt.Sprintf(`,"client_id":%q`, clientID)
				}
				return [][]byte{
					[]byte(fmt.Sprintf(`{"timestamp":"2026-10-01T07:00:0%dZ","type":"response_item","payload":{"type":"message","id":"msg-%d","role":"user","content":[{"type":"input_text","text":%q}],"internal_chat_message_metadata_passthrough":{"turn_id":%q}}}`, n, n, text, turn)),
					[]byte(fmt.Sprintf(`{"timestamp":"2026-10-01T07:00:0%dZ","type":"event_msg","payload":{"type":"item_completed","thread_id":%q,"turn_id":%q,"item":{"type":"UserMessage","id":"item-%d"%s,"content":[{"type":"text","text":%q,"text_elements":[]}]}}}`, n, thread, turn, n, client, text)),
				}
			}
			legacy := accept("legacy", "Started before client ids.", "")
			correction := accept("correction", "Correction steered in.", "client-correction")
			second := accept("second", "A second correction.", "client-second")
			h.view.expectSaved["prompt:legacy"] = legacy.PresentationText
			h.view.expectSaved["prompt:second"] = second.PresentationText
			h.sess.view.PublishLiveOutbox(legacy)
			h.sess.view.PublishLiveOutbox(second)
			if restarted {
				// Confirmed before this attach, so it is not live and only its
				// stored binding still claims its row.
				if _, _, err := h.sess.delivery.PromptStore().Settle(ctx, "correction"); err != nil {
					t.Fatal(err)
				}
			} else {
				h.view.expectSaved["prompt:correction"] = correction.PresentationText
				h.sess.view.PublishLiveOutbox(correction)
			}
			lines := [][]byte{fixtureLines(t, "codex-turns.saved.jsonl")[0],
				[]byte(fmt.Sprintf(`{"timestamp":"2026-10-01T07:00:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":%q}}`, turn))}
			if mode == "unreadable claims" {
				db := h.queue.Database()
				var stored string
				if err := db.QueryRow(`SELECT binding_json FROM queued_turns WHERE client_action_id='correction'`).Scan(&stored); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`UPDATE queued_turns SET binding_json='{"scope":1}' WHERE client_action_id='correction'`); err != nil {
					t.Fatal(err)
				}
				// The confirmed correction's row is the turn's only saved row, so a
				// turn key without its claims would wrongly take it.
				lines = append(lines, pair(2, "Correction steered in.", "client-correction")...)
				h.save(lines)
				h.expect("correction row saved", func(LiveTextMessage) bool {
					for _, row := range h.view.rows {
						if row.Type == "user_message" && row.Text == "Correction steered in." {
							return true
						}
					}
					return false
				})
				h.barrier()
				if !containsEntry(h.view.live, "prompt:legacy") {
					t.Fatal("the legacy prompt left on another prompt's row while the claims were unreadable")
				}
				if _, err := db.Exec(`UPDATE queued_turns SET binding_json=? WHERE client_action_id='correction'`, stored); err != nil {
					t.Fatal(err)
				}
				lines = append(pair(1, "Started before client ids.", ""), pair(3, "A second correction.", "client-second")...)
			} else {
				lines = append(lines, pair(1, "Started before client ids.", "")...)
				lines = append(lines, pair(2, "Correction steered in.", "client-correction")...)
				lines = append(lines, pair(3, "A second correction.", "client-second")...)
			}
			h.save(lines)
			h.expect("every prompt saved", func(LiveTextMessage) bool {
				return !containsEntry(h.view.live, "prompt:legacy") && !containsEntry(h.view.live, "prompt:correction") && !containsEntry(h.view.live, "prompt:second")
			})
		})
	}
}

// Codex evidence arrives in any order. A prompt saved before delivery binds it
// to its turn hands over when the binding arrives. A Stop-cut reply stays
// through a reload while the file leaves its turn's end unknown and through an
// agent stream opening; a reload clears it once the file proves the turn
// ended, before the live stream reports the end; a rebuild's fresh tail counts
// as a reload. A reply first seen mid-stream, as after a restart, shows only
// once saved.
func TestLiveTextCodexEvidenceInAnyOrder(t *testing.T) {
	query := threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40}
	saved := fixtureLines(t, "codex-turns.saved.jsonl")
	var turns [][][]byte
	var frames [][]byte
	for _, line := range fixtureLines(t, "codex-turns.wire.jsonl") {
		frames = append(frames, line)
		if strings.Contains(string(line), `"turn/completed"`) {
			turns, frames = append(turns, frames), nil
		}
	}
	without := func(lines [][]byte, fragments ...string) [][]byte {
		var kept [][]byte
		for _, line := range lines {
			drop := false
			for _, fragment := range fragments {
				drop = drop || strings.Contains(string(line), fragment)
			}
			if !drop {
				kept = append(kept, line)
			}
		}
		return kept
	}
	thread := codexFixtureThread
	bound := func(h *liveTextHarness, action, text, turn string, state prompts.DeliveryState) prompts.OutboxEntry {
		entry := prompts.OutboxEntry{HostedThreadID: h.sess.HostedThreadID(), ActionID: action, Sequence: 1, PresentationText: text, State: state}
		if turn != "" {
			entry.Binding = &prompts.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: "codex", NativeThreadID: &thread, NativeTurnID: &turn}}
		}
		return entry
	}
	firstText := "Reply with two short sentences about rain."
	stopText := "Write 80 numbered lines. Line N must be CODEX_STOP_NN for NN 01 through 80. Do not use tools."
	complete, aborted := lineWith(saved, `"task_complete"`), lineWith(saved, `"turn_aborted"`)
	stopped := "reply:" + codexFixtureStopped

	h := newLiveTextHarnessFor(t, "codex", query)
	h.sess.view.PublishLiveOutbox(bound(h, "action-1", firstText, "", prompts.Delivering))
	h.expect("unbound prompt", func(LiveTextMessage) bool { return containsEntry(h.view.live, "prompt:action-1") })
	h.save(saved[:complete+1])
	h.waitSaved(codexFixtureReply1)
	h.barrier()
	// Unbound, the prompt cannot be matched yet; its binding hands it over.
	h.view.expectSaved["prompt:action-1"] = firstText + "\n\n[from nano]"
	h.sess.view.PublishLiveOutbox(bound(h, "action-1", firstText, codexFixtureTurn1, prompts.Accepted))
	h.expect("bound prompt handed over", func(LiveTextMessage) bool { return !containsEntry(h.view.live, "prompt:action-1") })

	h.view.expectSaved["prompt:action-2"] = stopText + "\n\n[from nano]"
	h.view.neverSaved[stopped] = true
	h.sess.view.PublishLiveOutbox(bound(h, "action-2", stopText, codexFixtureTurn2, prompts.Accepted))
	h.feed(without(turns[1], `"turn/completed"`))
	h.expect("streaming stopped reply", func(LiveTextMessage) bool { return h.view.reply() != nil })
	// The file without task_started leaves the turn's end unknown.
	h.save(without(saved[complete+1:aborted], `"task_started"`))
	h.expect("prompt 2 saved", func(LiveTextMessage) bool { return !containsEntry(h.view.live, "prompt:action-2") })
	unknown := h.open(query)
	unknown.view.neverSaved[stopped] = true
	if unknown.view.reply() == nil {
		t.Fatal("a reload cleared the stopped reply while its turn's end was unknown")
	}
	h.save(saved[aborted : aborted+1])
	h.waitTurnEnded(codexFixtureTurn2)
	agent := newLiveTextPane(query)
	agent.rowsOnly = true // as handleFileConversationEvents subscribes
	liveTextOf(h.sess).subscribe(agent)
	h.barrier()
	if h.view.reply() == nil {
		t.Fatal("an agent stream opening cleared the operator's stopped reply")
	}
	reloaded := h.open(query)
	if reloaded.view.reply() != nil {
		t.Fatal("a reload after the file proved the turn ended still shows the stopped reply")
	}
	unknown.expect("cleared in every pane", func(LiveTextMessage) bool { return unknown.view.reply() == nil })
	h.feed(turns[1][len(turns[1])-1:]) // the live end arrives late
	h.barrier()

	h.sess.view.PublishLiveOutbox(bound(h, "action-3", "Reply with exactly: CODEX_AFTER_STOP", codexFixtureTurn3, prompts.Accepted))
	h.view.expectSaved["prompt:action-3"] = "Reply with exactly: CODEX_AFTER_STOP\n\n[from nano]"
	h.feed(without(turns[2], `"item/started","params":{"item":{"type":"agentMessage"`))
	h.save(saved[aborted+1:])
	h.expect("turn 3 saved", func(LiveTextMessage) bool { return len(h.view.live) == 0 && h.view.hasNative(codexFixtureReply3) })
	if h.view.shown["reply:"+codexFixtureReply3] {
		t.Fatal("a reply first seen mid-stream showed as live text")
	}

	// A rebuild's fresh tail is a reload too.
	r := newLiveTextHarnessFor(t, "codex", query)
	r.view.neverSaved[stopped] = true
	r.view.expectSaved["prompt:action-2"] = stopText + "\n\n[from nano]"
	r.sess.view.PublishLiveOutbox(bound(r, "action-2", stopText, codexFixtureTurn2, prompts.Accepted))
	r.feed(turns[1])
	r.expect("stopped reply", func(LiveTextMessage) bool { return r.view.reply() != nil && r.view.reply().State == "done" })
	r.save(saved[:aborted+1])
	r.waitTurnEnded(codexFixtureTurn2)
	r.barrier()
	if r.view.reply() == nil {
		t.Fatal("an apply cleared the stopped reply without a reload")
	}
	if err := os.Remove(r.file); err != nil {
		t.Fatal(err)
	}
	r.save(saved[:aborted+1])
	r.expect("rebuild tail clears the stopped reply", func(m LiveTextMessage) bool { return m.Type == "tail" && r.view.reply() == nil })
}

// deliveryJournal reads the thread's delivery journal as a reloading browser
// does, by action id.
func (h *liveTextHarness) deliveryJournal() map[string]prompts.View {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.sess.delivery.HandleTurnDeliveryList(rec, httptest.NewRequest(http.MethodGet, "/turn-delivery?thread="+h.sess.stateOwner.ThreadID(), nil))
	var body struct {
		Operations []prompts.View `json:"operations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		h.t.Fatalf("journal = %d %s: %v", rec.Code, rec.Body.String(), err)
	}
	// One vocabulary: a row carries the stored state and its presentation,
	// never a second status word for a reader to map.
	var raw struct {
		Operations []map[string]json.RawMessage `json:"operations"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	for _, row := range raw.Operations {
		for _, legacy := range []string{"status", "reason", "reasonCode", "confirmed", "confirmedAt", "inputSaved"} {
			if _, ok := row[legacy]; ok {
				h.t.Fatalf("journal row carries legacy %q: %s", legacy, rec.Body.String())
			}
		}
	}
	rows := map[string]prompts.View{}
	for _, row := range body.Operations {
		rows[row.ClientActionID] = row
	}
	return rows
}

// liveEntry is the pane's live entry with this id, or nil.
func liveEntry(live []liveTextEntry, id string) *liveTextEntry {
	for i := range live {
		if live[i].ID == id {
			return &live[i]
		}
	}
	return nil
}

// acceptCodexPrompt stores a prompt submitted at createdAt (now when empty)
// as accepted into turn, bound by a client id the saved fixture does not
// carry, and publishes it.
func (h *liveTextHarness) acceptCodexPrompt(action, text, turn string, origin prompts.Origin, createdAt string) {
	h.t.Helper()
	ctx := context.Background()
	hosted, native := h.sess.HostedThreadID(), h.sess.stateOwner.ThreadID()
	if _, _, err := h.sess.delivery.PromptStore().Submit(ctx, prompts.Submission{HostedThreadID: hosted, Provider: model.ProviderCodex, NativeThreadID: native, ActionID: action,
		ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: origin, CreatedAt: createdAt}); err != nil {
		h.t.Fatal(err)
	}
	if _, claimed, err := h.sess.delivery.PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: action, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		h.t.Fatalf("claim %s = %v, %v", action, claimed, err)
	}
	accepted, err := h.sess.delivery.PromptStore().Accepted(ctx, prompts.Acceptance{ActionID: action, ClaimToken: action, AttachmentEpoch: 1, Binding: prompts.Binding{
		EventID: submissionEventID(hosted, action), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}}})
	if err != nil {
		h.t.Fatal(err)
	}
	h.sess.view.PublishLiveOutbox(accepted.Outbox)
}

// The live stream and a reload's journal present each prompt from its one
// stored record, so they agree in every open state: a queued addendum is
// "Queued for the next turn" with Discard only, a send Nano stopped waiting
// for warns that resending may deliver twice, a refused one says why it
// was not delivered, and one the provider has not taken past the slow
// threshold warns that sending again delivers twice: the stream relabels it
// at the threshold on its own, with no other change. Boundary: the live
// stream and the journal routes, for the same stored prompts.
func TestLiveTextAndJournalPresentEachPromptAlike(t *testing.T) {
	ctx := context.Background()
	h := newLiveTextHarnessFor(t, "codex", threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40})
	sess := h.sess
	hosted, native := sess.HostedThreadID(), sess.stateOwner.ThreadID()
	submit := func(action string, nextTurn bool) {
		text := "prompt " + action
		if _, _, err := sess.delivery.PromptStore().Submit(ctx, prompts.Submission{HostedThreadID: hosted, Provider: model.ProviderCodex, NativeThreadID: native, ActionID: action,
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: hostedstore.TurnRequestHash(text, nextTurn), Origin: prompts.AgentOrigin, NextTurn: nextTurn}); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(action string) prompts.Record {
		claimed, ok, err := sess.delivery.PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: action, AttachmentEpoch: 1, Ready: true})
		if err != nil || !ok || claimed.Outbox.ActionID != action {
			t.Fatalf("claim %s = %s %v, %v", action, claimed.Outbox.ActionID, ok, err)
		}
		return claimed
	}
	submit("timed-out", false)
	claim("timed-out")
	uncertain, err := sess.delivery.PromptStore().Uncertain(ctx, prompts.Uncertainty{ActionID: "timed-out", ClaimToken: "timed-out", AttachmentEpoch: 1, Reason: context.DeadlineExceeded.Error()})
	if err != nil {
		t.Fatal(err)
	}
	submit("parked", false)
	claim("parked")
	parked, err := sess.delivery.PromptStore().Refused(ctx, prompts.Refusal{ActionID: "parked", ClaimToken: "parked", AttachmentEpoch: 1, State: prompts.Parked, Reason: "rpc error -32000: provider is not accepting input"})
	if err != nil {
		t.Fatal(err)
	}
	// Delivery began shortly before the threshold: the provider has not taken
	// it yet, and nothing about it changes again.
	submit("slow", false)
	slow, ok, err := sess.delivery.PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: "slow", AttachmentEpoch: 1, Ready: true,
		At: time.Now().Add(2*time.Second - promptSlowDelivery).UTC().Format(time.RFC3339Nano)})
	if err != nil || !ok || slow.Outbox.ActionID != "slow" {
		t.Fatalf("claim slow = %s %v, %v", slow.Outbox.ActionID, ok, err)
	}
	submit("addendum", true)
	queued, _, err := sess.delivery.PromptStore().Get(ctx, "addendum")
	if err != nil {
		t.Fatal(err)
	}
	sess.view.PublishLiveOutbox(uncertain.Outbox, parked.Outbox, slow.Outbox, queued.Outbox)
	h.expect("four prompts shown", func(LiveTextMessage) bool { return len(h.view.live) == 4 })
	if entry := liveEntry(h.view.live, "prompt:slow"); entry.State != string(prompts.Delivering) || entry.Label != "" || entry.AllowedActions != nil {
		t.Fatalf("slow entry before the threshold = %+v; want delivering with no label or action", entry)
	}
	const slowLabel = "Codex hasn't taken it yet; sending again will deliver twice"
	h.expect("slow prompt relabeled", func(LiveTextMessage) bool {
		entry := liveEntry(h.view.live, "prompt:slow")
		return entry != nil && entry.Label == slowLabel
	})

	want := map[string]struct {
		state   prompts.DeliveryState
		actions []string
		label   string
	}{
		"addendum":  {prompts.Queued, []string{promptDiscard}, "Queued for the next turn"},
		"timed-out": {prompts.Uncertain, []string{promptResend, promptDiscard}, "Codex hasn't answered in 30 s; resending may deliver twice"},
		"parked":    {prompts.Parked, []string{promptResend, promptDiscard}, "Not delivered: rpc error -32000: provider is not accepting input"},
		"slow":      {prompts.Delivering, nil, slowLabel},
	}
	rec := httptest.NewRecorder()
	sess.delivery.HandleTurnDeliveryRead(rec, httptest.NewRequest(http.MethodGet, "/turn-delivery/slow", nil), "slow")
	var read prompts.View
	if err := json.Unmarshal(rec.Body.Bytes(), &read); err != nil || read.State != string(prompts.Delivering) || read.Label != slowLabel || read.AllowedActions != nil {
		t.Fatalf("single read of slow = %d %s, %v", rec.Code, rec.Body.String(), err)
	}
	journal := h.deliveryJournal()
	for id, w := range want {
		entry, row := liveEntry(h.view.live, "prompt:"+id), journal[id]
		if entry == nil || entry.State != string(w.state) || !slices.Equal(entry.AllowedActions, w.actions) || entry.Label != w.label {
			t.Fatalf("%s stream entry = %+v, want %+v", id, entry, w)
		}
		if row.State != entry.State || !slices.Equal(row.AllowedActions, entry.AllowedActions) || row.Label != entry.Label {
			t.Fatalf("%s: journal %q %v %q, stream %q %v %q", id, row.State, row.AllowedActions, row.Label, entry.State, entry.AllowedActions, entry.Label)
		}
	}
}

// An Enhanced pane's rows and turns carry only what the Enhanced view draws;
// the developer view's raw provider copy, tags and turn config were over a
// quarter of every tail. A dev pane keeps them.
func TestLiveTextEnhancedPaneOmitsDeveloperFields(t *testing.T) {
	h := newLiveTextHarness(t)
	h.save(fixtureLines(t, "claude-tool-error.saved.jsonl"))
	h.expect("saved rows", func(LiveTextMessage) bool { return len(h.view.rows) >= 2 })
	rawTail := func(query threadindex.TranscriptQuery) string {
		pane := newLiveTextPane(query)
		liveTextOf(h.sess).subscribe(pane)
		defer liveTextOf(h.sess).unsubscribe(pane)
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-pane.ready:
			case <-time.After(50 * time.Millisecond):
			}
			for _, raw := range pane.take() {
				if strings.HasPrefix(string(raw), `{"type":"tail"`) {
					return string(raw)
				}
			}
		}
		t.Fatal("no tail")
		return ""
	}
	dev := rawTail(threadindex.TranscriptQuery{View: threadindex.DevThreadTranscriptView, HiddenSpecified: true, Limit: 40})
	for _, field := range []string{`"detail":`, `"filterTags":`, `"config":`} {
		if !strings.Contains(dev, field) {
			t.Fatalf("the dev tail lacks %s, so this test cannot show the Enhanced tail dropping it", field)
		}
	}
	enhanced := rawTail(threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40})
	for _, field := range []string{`"detail":`, `"filterTags":`, `"synthetic":`, `"config":`, `"nativeTurnId":`, `"finalitySource":`} {
		if strings.Contains(enhanced, field) {
			t.Fatalf("the Enhanced tail carries %s", field)
		}
	}
	for _, field := range []string{`"text":`, `"createdAt":`, `"projectedSeq":`, `"startSeq":`} {
		if !strings.Contains(enhanced, field) {
			t.Fatalf("the Enhanced tail lacks %s", field)
		}
	}
	if len(enhanced) >= len(dev) {
		t.Fatalf("Enhanced tail %d bytes, dev tail %d bytes", len(enhanced), len(dev))
	}
}

// journalReadMetadata keeps the harness's metadata owner while reading prompts
// from its separately seeded canonical prompt database.
type journalReadMetadata struct {
	Metadata
	db *sql.DB
}

func (m journalReadMetadata) ReadPool() *sql.DB { return m.db }
func liveTextOf(sess *Session) *LiveTextLoop    { return sess.view.liveText }
func liveTextTestConfig(t *testing.T, provider string) (Config, int64) {
	t.Helper()
	metadataDB, _, err := testTurnDatabase(t, filepath.Join(t.TempDir(), "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := hostedstore.NewMetadataStore(metadataDB, metadataDB)
	if err != nil {
		t.Fatal(err)
	}
	const hostedID = "nht_claude_test"
	if err := metadata.EnsureHostedThread(hostedID, provider, "shared", "", "", ""); err != nil {
		t.Fatal(err)
	}
	generation, err := metadata.BeginTransportBinding(hostedID)
	if err != nil {
		t.Fatal(err)
	}
	queue := mustQueuedTurnStore(t)
	attempts, err := newSendAttemptStore(queue.Database(), queue.Path(), "claude-test", testPromptStore(t, queue.Database()))
	if err != nil {
		t.Fatal(err)
	}
	turns := mustBindTransactionalTurnStore(t, queue, attempts)
	return Config{ID: "sess-1", Provider: provider, HostedThreadID: hostedID, CWD: t.TempDir(), CreatedAt: time.Now(), ProcessStarted: time.Now(), Metadata: metadata, Turns: turns, QueuedTurns: queue, SendAttempts: attempts}, generation
}

// seedUncertainTransactionalTurn stores a prompt whose delivery got no answer.
func seedUncertainTransactionalTurn(t *testing.T, store *prompts.Store, hosted string, provider model.Provider, native, action, prompt string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := store.Submit(ctx, prompts.Submission{
		HostedThreadID: hosted, Provider: provider, NativeThreadID: native, ActionID: action,
		ProviderText: prompt, PresentationText: prompt, TextHash: sendTextHash(prompt), RequestHash: sendTextHash(prompt), Origin: prompts.HumanOrigin,
	}); err != nil {
		t.Fatal(err)
	}
	token := "fixture-claim-" + action
	if _, claimed, err := store.Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: token, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim fixture %s: claimed=%v err=%v", action, claimed, err)
	}
	if _, err := store.Uncertain(ctx, prompts.Uncertainty{ActionID: action, ClaimToken: token, AttachmentEpoch: 1, Reason: "no answer"}); err != nil {
		t.Fatal(err)
	}
}

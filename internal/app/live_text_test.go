package app

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
	"github.com/dkroshow/nano/internal/history"
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
	t          *testing.T
	srv        *server
	sess       *session
	projection *liveProjection
	queue      sessionQueue
	journal    sessionJournal
	metadata   *controlMetadataStore
	service    *threadCatalogService
	store      *history.Store
	file       string
	// held keeps catalog apply notices from the loop.
	held         atomic.Bool
	barriers     int
	catalogReads atomic.Int64
	lastQueries  atomic.Int64 // pages asked in the newest read
}

// Count the real catalog boundary so streaming tests can distinguish an
// append from a read without replacing the saved-row behavior.
type countingLiveTextCatalog struct {
	*threadCatalogService
	reads, queries *atomic.Int64
}

func (c *countingLiveTextCatalog) LoadTranscripts(ctx context.Context, id string, queries []transcriptListQuery, matches []history.TranscriptMatch) ([]transcriptResponse, map[string]string, error) {
	c.reads.Add(1)
	c.queries.Store(int64(len(queries)))
	return c.threadCatalogService.LoadTranscripts(ctx, id, queries, matches)
}

// liveTextViewer is one open pane and the browser's view of it.
type liveTextViewer struct {
	t    *testing.T
	pane *liveTextPane
	view *livePaneView
}

func newLiveTextHarness(t *testing.T) *liveTextHarness {
	return newLiveTextHarnessWith(t, transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40})
}

func newLiveTextHarnessWith(t *testing.T, query transcriptListQuery) *liveTextHarness {
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

func newLiveTextHarnessFor(t *testing.T, provider string, query transcriptListQuery) *liveTextHarness {
	return newLiveTextHarnessForThread(t, provider, query, grokFixtureThread)
}

func newLiveTextHarnessForThread(t *testing.T, provider string, query transcriptListQuery, grokThread string, configure ...func(*sessionConfig)) *liveTextHarness {
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
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h := &liveTextHarness{t: t, store: store, file: file}
	service, err := newThreadCatalogService(context.Background(), store, threadCatalogServiceOptions{
		Roots: []history.SourceRoot{{Provider: history.Provider(provider), Kind: rootKind, Path: root}}, DisableWatcher: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	srv := &server{catalog: &countingLiveTextCatalog{service, &h.catalogReads, &h.lastQueries}, browserDiagnostics: newBrowserDiagnosticStore(64)}
	cfg, generation, _ := providerSessionConfig(t, provider)
	for _, apply := range configure {
		apply(&cfg)
	}
	cfg.Projection = srv.newSessionProjection(provider, cfg.HostedThreadID)
	h.metadata, _ = cfg.Metadata.(*controlMetadataStore)
	cfg.Metadata = journalReadMetadata{sessionMetadata: cfg.Metadata, db: cfg.QueuedTurns.Database()}
	sess := newSession(cfg)
	if err := sess.InitializeBinding(generation); err != nil {
		t.Fatal(err)
	}
	srv.startLiveText(sess)
	sess.ReserveNativeID(nativeThread)
	h.srv, h.sess, h.service = srv, sess, service
	h.projection = cfg.Projection.(*liveProjection)
	h.queue = cfg.QueuedTurns
	h.journal = cfg.SendAttempts
	service.SetTranscriptChanged(func(context.Context, transcriptChange) {
		if !h.held.Load() {
			sess.LiveTextService().Applied()
		}
	})
	h.liveTextViewer = h.open(query)
	return h
}

// open subscribes another pane and waits for its tail.
func (h *liveTextHarness) open(query transcriptListQuery) *liveTextViewer {
	v := &liveTextViewer{t: h.t, pane: newLiveTextPane(query), view: &livePaneView{expectSaved: map[string]string{}, neverSaved: map[string]bool{}}}
	liveTextOf(h.sess).subscribe(v.pane)
	v.expect("tail", func(m liveTextMessage) bool { return m.Type == "tail" })
	return v
}

// hold keeps catalog apply notices from the loop, so a test can act between
// a committed save and the loop learning of it; release delivers one.
func (h *liveTextHarness) hold() { h.held.Store(true) }

func (h *liveTextHarness) release() {
	h.held.Store(false)
	h.sess.LiveTextService().Applied()
}

// page reads the catalog directly: the test's own view of what is saved.
func (h *liveTextHarness) page(query transcriptListQuery) []transcriptItem {
	h.t.Helper()
	id, err := h.service.ResolveThreadReference(context.Background(), h.sess.ThreadID())
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
		id, err := h.service.ResolveThreadReference(context.Background(), h.sess.ThreadID())
		if err != nil {
			continue // not indexed yet
		}
		response, err := h.service.LoadTranscript(context.Background(), id, transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40})
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
		for _, row := range h.page(transcriptListQuery{View: devThreadTranscriptView, HiddenSpecified: true, Limit: 40}) {
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
			h.projection.mu.Lock()
			turn := h.projection.claudeTurnID
			h.projection.mu.Unlock()
			h.projection.SetClaudeResultTurnID(turn)
		}
		h.projection.ApplyWireLive(protocol.Recv, line, time.Now(), true, false)
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
func (v *liveTextViewer) expect(what string, until func(liveTextMessage) bool) {
	v.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-v.pane.ready:
		case <-time.After(50 * time.Millisecond):
		}
		for _, raw := range v.pane.take() {
			var message liveTextMessage
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
	rows  []transcriptItem
	live  []liveTextEntry
	shown map[string]bool // live entry ids ever shown
	// expectSaved names prompts the test will save, by their unique text, so
	// prompt continuity is checked independently of the code under test.
	expectSaved map[string]string
	// neverSaved names replies the test never saves, which may leave live
	// text without a row.
	neverSaved map[string]bool
}

func (v *livePaneView) apply(t *testing.T, m liveTextMessage) {
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
	h.sess.LiveTextService().OutboxChanged(accepted)
	h.expect("prompt", func(liveTextMessage) bool { return len(h.view.live) == 1 })
	confirmed := accepted
	confirmed.State = prompts.Confirmed
	h.sess.LiveTextService().OutboxChanged(confirmed)
	h.barrier()
	if !containsEntry(h.view.live, "prompt:action-1") {
		t.Fatalf("confirmation removed the prompt before its saved row: %+v", h.view.live)
	}
	// The shim tags a delivered prompt's frames with its turn.
	h.projection.SetClaudeProviderTurnID("claude:" + h.sess.HostedThreadID() + ":1")
	h.feed(fixtureLines(t, "claude-turn.wire.jsonl"))
	// A finished reply reads done before its saved row lands, so the pane
	// stops showing it as streaming.
	h.expect("finished reply", func(liveTextMessage) bool { return h.view.reply() != nil && h.view.reply().State == "done" })
	h.save(saved[:userLine+1])
	h.expect("prompt saved", func(liveTextMessage) bool { return !containsEntry(h.view.live, "prompt:action-1") })
	h.save(saved[userLine+1 : thinkingLine+1])
	h.barrier()
	if h.view.reply() == nil {
		t.Fatalf("the reply left live text before its own row was saved: %+v", h.view.live)
	}
	// The catalog holds the reply before the loop hears of it.
	h.hold()
	h.save(saved[thinkingLine+1:])
	h.waitSaved(fixtureReply)
	second := h.open(transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40})
	if second.view.reply() != nil || !second.view.hasNative(fixtureReply) {
		t.Fatalf("the second pane shows rows %+v and live text %+v, want the saved reply only as a row", second.view.rows, second.view.live)
	}
	h.expect("reply saved", func(m liveTextMessage) bool {
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
	h.sess.LiveTextService().OutboxChanged(queued)
	h.expect("queued prompt", func(liveTextMessage) bool { return containsEntry(h.view.live, "prompt:action-2") })
	h.save(fixtureLines(t, "claude-queued-prompt.jsonl"))
	h.expect("queued prompt saved", func(liveTextMessage) bool { return !containsEntry(h.view.live, "prompt:action-2") })
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
	h.expect("the prompt's row saved", func(liveTextMessage) bool {
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
	h.sess.LiveTextService().OutboxChanged(entry)
	h.expect("the prompt named saved", func(m liveTextMessage) bool { return slices.Contains(m.Saved, "prompt:action-2") })
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
	native := h.sess.ThreadID()
	settle := func(action, event string) {
		t.Helper()
		text, turn := "prompt "+action, "claude:"+action
		if _, _, err := h.sess.PromptStore().Submit(ctx, prompts.Submission{HostedThreadID: h.sess.HostedThreadID(), Provider: model.ProviderClaude, NativeThreadID: native,
			ActionID: action, ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := h.sess.PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: h.sess.HostedThreadID(), Token: "claim-" + action, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", action, claimed, err)
		}
		if _, err := h.sess.PromptStore().Accepted(ctx, prompts.Acceptance{ActionID: action, ClaimToken: "claim-" + action, AttachmentEpoch: 1,
			Binding: prompts.Binding{EventID: event, Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}}); err != nil {
			t.Fatal(err)
		}
		if _, settled, err := h.sess.PromptStore().Settle(ctx, action); err != nil || !settled {
			t.Fatalf("settle %s: %v %v", action, settled, err)
		}
	}
	saved := fixtureLines(t, "claude-turn.saved.jsonl")
	h.save(saved[:lineWith(saved, `"thinking"`)+1])
	h.save(fixtureLines(t, "claude-queued-prompt.jsonl"))
	h.expect("the prompt's row saved", func(liveTextMessage) bool {
		for _, row := range h.view.rows {
			if row.Type == "user_message" && row.Text == "Also mention snow." {
				return true
			}
		}
		return false
	})
	settle("in-page", "22222222-3333-4444-8555-666666666666")
	settle("not-in-page", "99999999-3333-4444-8555-666666666666")

	later := &liveTextViewer{t: t, pane: newLiveTextPane(transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40}),
		view: &livePaneView{expectSaved: map[string]string{}, neverSaved: map[string]bool{}}}
	liveTextOf(h.sess).subscribe(later.pane)
	later.expect("its tail names the prompt whose row it holds, and only that one", func(m liveTextMessage) bool {
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
	h.expect("rows saved", func(liveTextMessage) bool { return len(h.view.rows) > 0 })
	if err := h.queue.Database().Close(); err != nil {
		t.Fatal(err)
	}
	later := &liveTextViewer{t: t, pane: newLiveTextPane(transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40}),
		view: &livePaneView{expectSaved: map[string]string{}, neverSaved: map[string]bool{}}}
	liveTextOf(h.sess).subscribe(later.pane)
	later.expect("the pane fails", func(m liveTextMessage) bool {
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
		h.sess.LiveTextService().OutboxChanged(entry)
	}
	h.expect("merged prompts", func(liveTextMessage) bool {
		return containsEntry(h.view.live, "prompt:merged-first") && containsEntry(h.view.live, "prompt:merged-third")
	})
	h.save(fixtureLines(t, "claude-merged-prompts.jsonl"))
	h.expect("merged prompts saved", func(liveTextMessage) bool {
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
	query := transcriptListQuery{View: defaultThreadTranscriptView, Limit: 1}
	h := newLiveTextHarnessWith(t, query)
	later := fixtureLines(t, "claude-tool-error.saved.jsonl")
	h.save(append(fixtureLines(t, "claude-turn.saved.jsonl"), later...))
	h.waitSaved("msg_6289003a54e45a93b0f31403ba5b1278:1")
	for _, row := range h.page(query) {
		if row.NativeItemID == fixtureReply {
			t.Fatalf("page = %+v, want the first turn's reply off it", h.page(query))
		}
	}
	h.sess.LiveTextService().OutboxChanged(prompts.OutboxEntry{
		HostedThreadID: h.sess.HostedThreadID(), ActionID: "action-1", Sequence: 1,
		PresentationText: "Reply with two short sentences about rain.", State: prompts.Accepted,
		Binding: &prompts.Binding{EventID: liveTextFixturePrompt},
	})
	h.sess.LiveTextService().OutboxChanged(prompts.OutboxEntry{
		HostedThreadID: h.sess.HostedThreadID(), ActionID: "action-2", Sequence: 2,
		PresentationText: "Reply with two short sentences about rain.", State: prompts.Accepted,
		Binding: &prompts.Binding{EventID: "99999999-2222-4333-8444-555555555555"},
	})
	// Resent as a stream the reply's identity is exact and already saved.
	// Resent after a reconnect as one bare frame, its block index is only
	// counted, so it is not exact.
	wire := fixtureLines(t, "claude-turn.wire.jsonl")
	h.feed(wire)
	h.projection.adapterMu.Lock()
	h.projection.adapter = newClaudeLiveAdapter()
	h.projection.adapterMu.Unlock()
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
	h.projection.SetClaudeProviderTurnID("claude:" + h.sess.HostedThreadID() + ":2")
	h.feed(wire)
	h.expect("ended reply", func(liveTextMessage) bool { return h.view.reply() != nil && h.view.reply().State == "done" })
	// The same frames, carrying the saved reply's identity: a replay.
	var replay [][]byte
	for _, line := range wire {
		replay = append(replay, []byte(strings.ReplaceAll(string(line), "msg_fc823e6c7ddd5dd584d9bd9b60795fa8", "msg_6289003a54e45a93b0f31403ba5b1278")))
	}
	h.projection.SetClaudeProviderTurnID("claude:" + h.sess.HostedThreadID() + ":3")
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
	unsaved := func(h *liveTextHarness) []browserDiagnosticEvent {
		return liveTextOf(h.sess).srv.browserDiagnostics.recent(10, "", "", "file.reply.unsaved")
	}
	finished := func(h *liveTextHarness) func(liveTextMessage) bool {
		return func(liveTextMessage) bool { return h.view.reply() != nil && h.view.reply().State == "done" }
	}

	// A Stop-cut reply: the stream stops after a few text chunks, and the
	// turn's result follows.
	cut := newLiveTextHarness(t)
	cut.view.neverSaved[reply] = true
	cut.projection.SetClaudeProviderTurnID("claude:" + cut.sess.HostedThreadID() + ":1")
	cut.feed(append(append([][]byte(nil), wire[:lineWith(wire, `"type": "text_delta"`)+3]...), wire[len(wire)-1]))
	cut.expect("cut reply", finished(cut))

	// Saved in time, while the loop has not heard of the save.
	inTime := newLiveTextHarness(t)
	inTime.hold()
	inTime.projection.SetClaudeProviderTurnID("claude:" + inTime.sess.HostedThreadID() + ":1")
	inTime.feed(wire)
	inTime.expect("finished reply", finished(inTime))
	inTime.save(turn)
	inTime.waitSaved(fixtureReply)

	// A Codex reply whose start the loop missed, as after a restart, never
	// shows; finished and never saved, it is still recorded.
	missed := newLiveTextHarnessFor(t, "codex", transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40})
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
	h.expect("prompt row", func(liveTextMessage) bool { return len(h.view.rows) == 1 })
	h.projection.SetClaudeProviderTurnID("claude:" + h.sess.HostedThreadID() + ":1")
	h.feed(wire)
	h.expect("finished reply", finished(h))
	reloaded := h.open(transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40})
	reloaded.view.neverSaved[reply] = true
	if reloaded.view.reply() == nil {
		t.Fatal("a reload hid the unsaved reply")
	}
	// The next prompt, delivered after the reply ended, is saved: the reply
	// leaves every pane with that row.
	next := prompts.OutboxEntry{HostedThreadID: h.sess.HostedThreadID(), ActionID: "next", Sequence: 2,
		PresentationText: "next", State: prompts.Accepted, Binding: &prompts.Binding{EventID: "22222222-3333-4444-8555-666666666666"}}
	h.sess.LiveTextService().OutboxChanged(next)
	h.expect("next prompt", func(liveTextMessage) bool { return containsEntry(h.view.live, "prompt:next") })
	saved := fixtureLines(t, "claude-tool-error.saved.jsonl")
	h.save(saved[:lineWith(saved, "22222222-3333-4444-8555-666666666666")+1])
	h.expect("next prompt saved", func(m liveTextMessage) bool {
		if h.view.reply() != nil {
			return false
		}
		if m.Type != "update" || len(m.Rows) == 0 || containsEntry(h.view.live, "prompt:next") || slices.Contains(m.Saved, reply) {
			t.Fatalf("the reply left as %q with %d rows, live %+v and saved %v, want the update that saves the next prompt, not the reply",
				m.Type, len(m.Rows), h.view.live, m.Saved)
		}
		return true
	})
	reloaded.expect("next prompt saved", func(liveTextMessage) bool { return reloaded.view.reply() == nil })

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
	h.sess.LiveTextService().OutboxChanged(entry)
	h.expect("barrier", func(liveTextMessage) bool { return containsEntry(h.view.live, "prompt:"+id) })
	entry.State = prompts.Refused
	h.sess.LiveTextService().OutboxChanged(entry)
	h.expect("barrier gone", func(liveTextMessage) bool { return !containsEntry(h.view.live, "prompt:"+id) })
}

// Agents read only saved rows: a reply still streaming is absent from every
// agent read of a file-only thread, and present once saved.
func TestLiveTextNeverReachesAgentReads(t *testing.T) {
	h := newLiveTextHarness(t)
	saved := fixtureLines(t, "claude-turn.saved.jsonl")
	userLine := -1
	for i, line := range saved {
		if strings.Contains(string(line), liveTextFixturePrompt) {
			userLine = i
		}
	}
	h.save(saved[:userLine+1])
	h.expect("prompt saved", func(liveTextMessage) bool { return len(h.view.rows) == 1 })
	h.feed(fixtureLines(t, "claude-turn.wire.jsonl"))
	h.barrier()
	reads := func() map[string]string {
		out := map[string]string{}
		for _, view := range []conversationView{conversationViewMessages, conversationViewFull} {
			rec := httptest.NewRecorder()
			h.sess.HandleConversationView(rec, httptest.NewRequest("GET", "/conversation?view="+string(view), nil), view)
			out["conversation "+string(view)] = rec.Body.String()
		}
		rec := httptest.NewRecorder()
		liveTextOf(h.sess).srv.handleConversationHistory(rec, httptest.NewRequest("GET", "/conversation/history", nil), h.sess)
		out["session history"] = rec.Body.String()
		return out
	}
	var reply string
	for _, entry := range h.view.live {
		if entry.Kind == "reply" {
			reply = strings.ToLower(entry.Text)[:20]
		}
	}
	if reply == "" {
		t.Fatalf("no streamed reply in live text: %+v", h.view.live)
	}
	for route, body := range reads() {
		if strings.Contains(strings.ToLower(body), reply) {
			t.Fatalf("%s carries live text before it is saved: %s", route, body)
		}
	}
	h.save(saved[userLine+1:])
	h.expect("reply saved", func(liveTextMessage) bool {
		for _, entry := range h.view.live {
			if entry.Kind == "reply" {
				return false
			}
		}
		return true
	})
	for route, body := range reads() {
		if !strings.Contains(strings.ToLower(body), reply) {
			t.Fatalf("%s lacks the saved reply: %s", route, body)
		}
	}
	// Agent snapshots carry the live status cursor, and the event stream
	// continues from it.
	cursor := h.projection.Status()
	rec := httptest.NewRecorder()
	h.sess.HandleConversationView(rec, httptest.NewRequest("GET", "/conversation?view=messages", nil), conversationViewMessages)
	var snapshot liveProjectionSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil || snapshot.Epoch != cursor.Epoch || snapshot.Revision != cursor.Revision {
		t.Fatalf("snapshot cursor = %s/%d (%v), want the status cursor %s/%d", snapshot.Epoch, snapshot.Revision, err, cursor.Epoch, cursor.Revision)
	}
	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.sess.HandleConversationEventsView(w, r, conversationViewMessages)
	}))
	defer stream.Close()
	response, err := http.Get(stream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	lines := bufio.NewScanner(response.Body)
	nextData := func() []byte {
		for lines.Scan() {
			if data, ok := strings.CutPrefix(lines.Text(), "data: "); ok {
				return []byte(data)
			}
		}
		t.Fatal("the event stream ended")
		return nil
	}
	var first liveProjectionUpdate
	json.Unmarshal(nextData(), &first)
	h.projection.UpdateError("status changed")
	var next liveProjectionUpdate
	for next.Op == "" || next.Op == "rows.changed" {
		next = liveProjectionUpdate{}
		json.Unmarshal(nextData(), &next)
	}
	if first.Epoch != next.Epoch || next.Revision <= first.Snapshot.Revision || next.Op != projectionTurnUpsert {
		t.Fatalf("stream = %s/%d then %s %s/%d, want a later status update in the snapshot's epoch",
			first.Epoch, first.Snapshot.Revision, next.Op, next.Epoch, next.Revision)
	}
	// A failed read is an error to agents, never an empty transcript.
	h.store.Close()
	rec = httptest.NewRecorder()
	h.sess.HandleConversationView(rec, httptest.NewRequest("GET", "/conversation?view=messages", nil), conversationViewMessages)
	if rec.Code == 200 {
		t.Fatalf("conversation read after a store failure = 200 %s, want an error", rec.Body.String())
	}
}

// Admission metadata belongs to each outbox action, never its text or the
// process-local operation cache. The HTTP stream must preserve it on restart.
func TestLiveTextStreamCarriesAgentSubmissionTime(t *testing.T) {
	const first = "2026-10-07T15:00:01.123456Z"
	const second = "2026-10-07T15:00:02.654321Z"
	for _, tc := range []struct {
		name, origin string
		times        []string
		noTimestamp  bool
		missing      bool
		restart      bool
	}{
		{name: "agent", origin: turnOriginAgent, times: []string{first}},
		{name: "human", origin: turnOriginHuman, times: []string{first}, noTimestamp: true},
		{name: "missing record", missing: true, times: []string{""}},
		{name: "equal text distinct admissions", origin: turnOriginAgent, times: []string{first, second}},
		{name: "restart", origin: turnOriginAgent, times: []string{first}, restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := newTestControlMetadata(t)
			store, err := newControlAgentExecutionStore(control, "before-restart")
			if err != nil {
				t.Fatal(err)
			}
			h := newLiveTextHarnessForThread(t, "claude", transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40}, grokFixtureThread, func(cfg *sessionConfig) { cfg.AgentExecutions = store })
			if _, err := store.ensureSession(h.sess.HostedThreadID(), "claude", presentationShared); err != nil {
				t.Fatal(err)
			}
			want := make(map[string]string)
			for index, stamp := range tc.times {
				action := fmt.Sprintf("prompt-%d", index)
				if !tc.missing {
					op, _, err := store.beginTurnAttributed(h.sess.HostedThreadID(), action, "hash", turnAttribution{
						Origin: tc.origin, SenderSubmittedAt: stamp,
					})
					if err != nil {
						t.Fatal(err)
					}
					action = op.TransportActionID
				}
				want["prompt:"+action] = stamp
				if tc.noTimestamp {
					want["prompt:"+action] = ""
				}
				if _, _, err := h.sess.PromptStore().Submit(context.Background(), prompts.Submission{
					HostedThreadID: h.sess.HostedThreadID(), Provider: "claude", NativeThreadID: h.sess.ThreadID(),
					ActionID: action, ProviderText: "The same prompt.", PresentationText: "The same prompt.",
					TextHash: sendTextHash("The same prompt."), RequestHash: sendTextHash("The same prompt."), Origin: prompts.HumanOrigin,
				}); err != nil {
					t.Fatal(err)
				}
			}
			// A fresh loop restores the committed outbox instead of receiving
			// fabricated live entries. The reopened operation cache is cleared
			// to require the scoped committed lookup even after a failed reload.
			liveTextOf(h.sess).unsubscribe(h.pane)
			if tc.restart {
				store, err = newControlAgentExecutionStore(control, "after-restart")
				if err != nil {
					t.Fatal(err)
				}
			}
			store.mu.Lock()
			store.resetControlCachesLocked()
			store.mu.Unlock()
			fresh := newFixtureSession(sessionConfig{ID: h.sess.ID(), HostedThreadID: h.sess.HostedThreadID(), Provider: "claude", Projection: h.projection, Turns: h.sess.PromptStore(), AgentExecutions: store}, h.sess.ThreadID())
			liveTextOf(h.sess).srv.startLiveText(fresh)
			stream := httptest.NewServer(http.HandlerFunc(fresh.HandleLiveStream))
			defer stream.Close()
			client := &http.Client{Timeout: 5 * time.Second}
			response, err := client.Get(stream.URL + "?view=enhanced&limit=40")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			lines := bufio.NewScanner(response.Body)
			for lines.Scan() {
				data, ok := strings.CutPrefix(lines.Text(), "data: ")
				if !ok {
					continue
				}
				var message struct {
					Type string                       `json:"type"`
					Live []map[string]json.RawMessage `json:"live"`
				}
				if err := json.Unmarshal([]byte(data), &message); err != nil {
					t.Fatal(err)
				}
				if message.Type != "tail" {
					continue
				}
				if len(message.Live) != len(want) {
					t.Fatalf("live entries = %d, want %d", len(message.Live), len(want))
				}
				for _, entry := range message.Live {
					var id, stamp string
					if err := json.Unmarshal(entry["id"], &id); err != nil {
						t.Fatal(err)
					}
					expected, ok := want[id]
					if !ok {
						t.Fatalf("unexpected live entry %q", id)
					}
					if raw, present := entry["senderSubmittedAt"]; present {
						if err := json.Unmarshal(raw, &stamp); err != nil {
							t.Fatal(err)
						}
						if expected == "" {
							t.Fatalf("unattributed prompt %q carries senderSubmittedAt", id)
						}
					}
					if stamp != expected {
						t.Errorf("prompt %q senderSubmittedAt = %q, want admission time %q", id, stamp, expected)
					}
				}
				return
			}
			t.Fatalf("no tail: %v", lines.Err())
		})
	}
}

// A file-only pane takes its status (Working, the turn, approval cards) from
// its own stream, where the stream handler alone writes it: first, before any
// rows, then each change in revision order. On the shared page feed, a Stop's
// status was lost when the pane's hidden rows stalled and it read Working.
func TestLiveTextStreamCarriesStatusFirst(t *testing.T) {
	h := newLiveTextHarness(t)
	h.save(fixtureLines(t, "claude-turn.saved.jsonl"))
	h.waitSaved(fixtureReply)
	stream := httptest.NewServer(http.HandlerFunc(h.sess.HandleLiveStream))
	defer stream.Close()
	response, err := http.Get(stream.URL + "?view=enhanced&limit=40")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	lines := bufio.NewScanner(response.Body)
	next := func() liveTextMessage {
		for lines.Scan() {
			if data, ok := strings.CutPrefix(lines.Text(), "data: "); ok {
				var message liveTextMessage
				if err := json.Unmarshal([]byte(data), &message); err != nil {
					t.Fatal(err)
				}
				return message
			}
		}
		t.Fatal("the stream ended")
		return liveTextMessage{}
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
	h.projection.UpdateRuntime(liveProjectionRuntime{SessionStatus: "ready", TurnActive: true, TurnID: "turn-2"})
	expect("working=true cards=0")
	h.projection.UpsertInteraction(pendingInteraction{ID: "ask-1", Kind: "question", CreatedAt: "2026-10-03T00:00:00Z"})
	expect("working=true cards=1")
	// Live rows change the projection too, with compact row records that
	// carry no status; they must not read as a turn ending or a card leaving.
	h.feed(fixtureLines(t, "claude-turn.wire.jsonl"))
	h.projection.RemoveInteraction("ask-1")
	expect("working=true cards=0")
	h.projection.UpdateRuntime(liveProjectionRuntime{SessionStatus: "ready"})
	expect("working=false cards=0")
}

// The projection drops a subscriber that falls behind; the stream must then
// end, so the browser reconnects to a fresh status instead of keeping a stale
// one.
func TestLiveTextStreamEndsWhenStatusLags(t *testing.T) {
	h := newLiveTextHarness(t)
	writer, done := h.blockedStream()
	for i := range 300 {
		h.projection.UpdateError(fmt.Sprintf("status %d", i))
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
	h.projection.UpdateRuntime(liveProjectionRuntime{SessionStatus: "ready", TurnActive: true, TurnID: "turn-2"})
	h.projection.UpdateRuntime(liveProjectionRuntime{SessionStatus: "ready"})
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
		h.sess.HandleLiveStream(writer, httptest.NewRequest("GET", "/live?view=enhanced&limit=40", nil).WithContext(ctx))
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

func (w *streamRecorder) statuses() []liveTextStatus {
	w.mu.Lock()
	body := w.body.String()
	w.mu.Unlock()
	var statuses []liveTextStatus
	for _, line := range strings.Split(body, "\n") {
		var message liveTextMessage
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
	h.expect("saved rows", func(liveTextMessage) bool { return len(h.view.rows) >= 2 })
	bound := prompts.OutboxEntry{HostedThreadID: h.sess.HostedThreadID(), ActionID: "bound", Sequence: 1,
		PresentationText: "Reply with two short sentences about rain.", State: prompts.Accepted,
		Binding: &prompts.Binding{EventID: liveTextFixturePrompt}}
	h.view.expectSaved["prompt:bound"] = bound.PresentationText
	h.sess.LiveTextService().OutboxChanged(bound)
	h.sess.LiveTextService().OutboxChanged(prompts.OutboxEntry{HostedThreadID: h.sess.HostedThreadID(), ActionID: "next",
		Sequence: 2, PresentationText: "next prompt", State: prompts.Queued})
	h.expect("prompts", func(liveTextMessage) bool { return len(h.view.live) == 2 })
	// Rewriting the file in place (new inode) is a new source generation: the
	// catalog rebuilds the projection under a new key.
	if err := os.Remove(h.file); err != nil {
		t.Fatal(err)
	}
	h.save(fixtureLines(t, "claude-turn.saved.jsonl"))
	var saved []string
	h.expect("fresh tail", func(m liveTextMessage) bool {
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
	h.sess.LiveTextService().OutboxChanged(prompts.OutboxEntry{HostedThreadID: h.sess.HostedThreadID(), ActionID: "large",
		Sequence: 3, PresentationText: strings.Repeat("x", liveTextPaneBacklog+1), State: prompts.Queued})
	reopened := h.open(transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40})
	if !containsEntry(reopened.view.live, "prompt:large") {
		t.Fatalf("reopened pane live = %d entries, want the large queued prompt", len(reopened.view.live))
	}
	// A failed read is shown, and the stream ends instead of retrying.
	h.store.Close()
	h.sess.LiveTextService().Applied()
	reopened.expect("read error", func(m liveTextMessage) bool { return m.Type == "error" })
	if !reopened.pane.isClosed() {
		t.Fatal("the pane stream stayed open after its read failed")
	}
}

// One drain applies producer events in arrival order: a reply that began
// before the next prompt was delivered shows first, and its newer full text
// replaces its pending update without moving it.
func TestLiveTextKeepsArrivalOrderInOneDrain(t *testing.T) {
	var in liveTextInbox
	in.addReply(liveReplyUpdate{key: "msg_1:0", text: "Ra", turn: "t1", started: true})
	next := prompts.OutboxEntry{ActionID: "next", Sequence: 2, PresentationText: "next", State: prompts.Delivering}
	in.events = append(in.events, liveTextEvent{outbox: &next})
	in.addReply(liveReplyUpdate{key: "msg_1:0", text: "Rain falls.", turn: "t1", done: true, started: true})
	l := newLiveTextLoop(&server{}, nil)
	l.reset()
	l.process(in)
	live := l.visible()
	if len(live) != 2 || live[0].ID != "reply:msg_1:0" || live[0].Text != "Rain falls." || live[0].State != "done" || live[1].ID != "prompt:next" {
		t.Fatalf("live = %+v, want the reply's latest text, then the prompt", live)
	}
}

// Queued prompts show in the order delivery claims them: a human prompt
// queued after an agent's goes first.
func TestLiveTextShowsQueueInDeliveryOrder(t *testing.T) {
	h := newLiveTextHarness(t)
	for _, origin := range []prompts.Origin{prompts.AgentOrigin, prompts.HumanOrigin} {
		record, _, err := h.sess.PromptStore().Submit(context.Background(), prompts.Submission{
			HostedThreadID: h.sess.HostedThreadID(), Provider: "claude", ActionID: string(origin),
			ProviderText: string(origin), PresentationText: string(origin), TextHash: string(origin),
			RequestHash: string(origin), Origin: origin,
		})
		if err != nil {
			t.Fatal(err)
		}
		h.sess.LiveTextService().OutboxChanged(record.Outbox)
	}
	h.expect("queue", func(liveTextMessage) bool { return len(h.view.live) == 2 })
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
	h.sess.LiveTextService().OutboxChanged(uncertain)
	h.expect("uncertain prompt", func(liveTextMessage) bool {
		for _, entry := range h.view.live {
			if entry.ID == "prompt:action-uncertain" {
				return entry.State == string(prompts.Uncertain)
			}
		}
		return false
	})
	h.save(fixtureLines(t, "claude-turn.saved.jsonl"))
	h.expect("uncertain prompt saved", func(liveTextMessage) bool { return !containsEntry(h.view.live, "prompt:action-uncertain") })
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
			h := newLiveTextHarnessWith(t, transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40, HiddenSpecified: true,
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
				rows := h.page(transcriptListQuery{View: devThreadTranscriptView, HiddenSpecified: true, Limit: 40})
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
					rows := h.page(transcriptListQuery{View: devThreadTranscriptView, HiddenSpecified: true, Limit: 40})
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
			h.expect("error row", func(liveTextMessage) bool {
				if ids := errorRows(); len(ids) > 0 {
					errorRow = ids[0]
				}
				return errorRow != ""
			})
			h.save(saved[ended+1:])
			h.expect("turn answered", func(liveTextMessage) bool {
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
			query := transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40}
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
				h.sess.LiveTextService().OutboxChanged(entry)
				entry.State = prompts.Accepted
				entry.Binding = &prompts.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: "codex", NativeThreadID: &thread, NativeTurnID: &turn}}
				h.sess.LiveTextService().OutboxChanged(entry)
			}
			ended := func(v *livePaneView) func(liveTextMessage) bool {
				return func(liveTextMessage) bool { return v.reply() != nil && v.reply().State == "done" }
			}
			submit("action-1", "Reply with two short sentences about rain.", codexFixtureTurn1)
			h.feed(turns[0])
			h.expect("finished reply", ended(h.view))
			h.save(saved[:lineWith(saved, `"task_complete"`)+1])
			h.expect("turn 1 saved", func(liveTextMessage) bool { return len(h.view.live) == 0 })
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
			h.expect("prompt 2 saved", func(liveTextMessage) bool { return !containsEntry(h.view.live, "prompt:action-2") })
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
			early.expect("stopped reply cleared", func(liveTextMessage) bool { return early.view.reply() == nil })

			submit("action-3", "Reply with exactly: CODEX_AFTER_STOP", codexFixtureTurn3)
			h.feed(turns[2])
			h.save(saved[aborted+1:])
			h.expect("turn 3 saved", func(liveTextMessage) bool { return len(h.view.live) == 0 && h.view.hasNative(codexFixtureReply3) })
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
			query := transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40}
			h := newLiveTextHarnessFor(t, "codex", query)
			thread, turn := codexFixtureThread, "01a0f63e-0000-7000-8000-00000000a001"
			accept := func(action, text, event string) prompts.OutboxEntry {
				if _, _, err := h.sess.PromptStore().Submit(ctx, prompts.Submission{HostedThreadID: h.sess.HostedThreadID(), Provider: model.ProviderCodex, NativeThreadID: thread,
					ActionID: action, ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
					t.Fatal(err)
				}
				if _, claimed, err := h.sess.PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: h.sess.HostedThreadID(), Token: "claim-" + action, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
					t.Fatalf("claim %s: %v %v", action, claimed, err)
				}
				record, err := h.sess.PromptStore().Accepted(ctx, prompts.Acceptance{ActionID: action, ClaimToken: "claim-" + action, AttachmentEpoch: 1,
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
			h.sess.LiveTextService().OutboxChanged(legacy)
			h.sess.LiveTextService().OutboxChanged(second)
			if restarted {
				// Confirmed before this attach, so it is not live and only its
				// stored binding still claims its row.
				if _, _, err := h.sess.PromptStore().Settle(ctx, "correction"); err != nil {
					t.Fatal(err)
				}
			} else {
				h.view.expectSaved["prompt:correction"] = correction.PresentationText
				h.sess.LiveTextService().OutboxChanged(correction)
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
				h.expect("correction row saved", func(liveTextMessage) bool {
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
			h.expect("every prompt saved", func(liveTextMessage) bool {
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
	query := transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40}
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
	h.sess.LiveTextService().OutboxChanged(bound(h, "action-1", firstText, "", prompts.Delivering))
	h.expect("unbound prompt", func(liveTextMessage) bool { return containsEntry(h.view.live, "prompt:action-1") })
	h.save(saved[:complete+1])
	h.waitSaved(codexFixtureReply1)
	h.barrier()
	// Unbound, the prompt cannot be matched yet; its binding hands it over.
	h.view.expectSaved["prompt:action-1"] = firstText + "\n\n[from nano]"
	h.sess.LiveTextService().OutboxChanged(bound(h, "action-1", firstText, codexFixtureTurn1, prompts.Accepted))
	h.expect("bound prompt handed over", func(liveTextMessage) bool { return !containsEntry(h.view.live, "prompt:action-1") })

	h.view.expectSaved["prompt:action-2"] = stopText + "\n\n[from nano]"
	h.view.neverSaved[stopped] = true
	h.sess.LiveTextService().OutboxChanged(bound(h, "action-2", stopText, codexFixtureTurn2, prompts.Accepted))
	h.feed(without(turns[1], `"turn/completed"`))
	h.expect("streaming stopped reply", func(liveTextMessage) bool { return h.view.reply() != nil })
	// The file without task_started leaves the turn's end unknown.
	h.save(without(saved[complete+1:aborted], `"task_started"`))
	h.expect("prompt 2 saved", func(liveTextMessage) bool { return !containsEntry(h.view.live, "prompt:action-2") })
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
	unknown.expect("cleared in every pane", func(liveTextMessage) bool { return unknown.view.reply() == nil })
	h.feed(turns[1][len(turns[1])-1:]) // the live end arrives late
	h.barrier()

	h.sess.LiveTextService().OutboxChanged(bound(h, "action-3", "Reply with exactly: CODEX_AFTER_STOP", codexFixtureTurn3, prompts.Accepted))
	h.view.expectSaved["prompt:action-3"] = "Reply with exactly: CODEX_AFTER_STOP\n\n[from nano]"
	h.feed(without(turns[2], `"item/started","params":{"item":{"type":"agentMessage"`))
	h.save(saved[aborted+1:])
	h.expect("turn 3 saved", func(liveTextMessage) bool { return len(h.view.live) == 0 && h.view.hasNative(codexFixtureReply3) })
	if h.view.shown["reply:"+codexFixtureReply3] {
		t.Fatal("a reply first seen mid-stream showed as live text")
	}

	// A rebuild's fresh tail is a reload too.
	r := newLiveTextHarnessFor(t, "codex", query)
	r.view.neverSaved[stopped] = true
	r.view.expectSaved["prompt:action-2"] = stopText + "\n\n[from nano]"
	r.sess.LiveTextService().OutboxChanged(bound(r, "action-2", stopText, codexFixtureTurn2, prompts.Accepted))
	r.feed(turns[1])
	r.expect("stopped reply", func(liveTextMessage) bool { return r.view.reply() != nil && r.view.reply().State == "done" })
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
	r.expect("rebuild tail clears the stopped reply", func(m liveTextMessage) bool { return m.Type == "tail" && r.view.reply() == nil })
}

// An expired prompt (an old delivered prompt exact identity never settled) is
// no longer shown. A long-lived thread's outbox held 23, and file-only showed
// every one as live text below the saved rows. The loop drops an expired
// prompt as it drops a refused one, whether a fresh loop seeds it or a shown
// prompt expires later.
func TestLiveTextShowsNoExpiredPrompt(t *testing.T) {
	old := prompts.OutboxEntry{ActionID: "agent-old", Sequence: 1, PresentationText: "an old delivered prompt", State: prompts.Expired}
	pending := prompts.OutboxEntry{ActionID: "human-now", Sequence: 2, PresentationText: "a prompt being delivered", State: prompts.Accepted}
	var in liveTextInbox
	in.events = append(in.events, liveTextEvent{outbox: &old}, liveTextEvent{outbox: &pending})
	l := newLiveTextLoop(&server{}, nil)
	l.reset()
	l.process(in)
	if live := l.visible(); len(live) != 1 || live[0].ID != "prompt:human-now" {
		t.Fatalf("live = %+v, want only the prompt being delivered", live)
	}
	expired := pending
	expired.State = prompts.Expired
	l.process(liveTextInbox{events: []liveTextEvent{{outbox: &expired}}})
	if live := l.visible(); len(live) != 0 {
		t.Fatalf("live = %+v after the prompt expired, want none", live)
	}
}

// Codex drops a person's prompt when its turn is interrupted before Codex
// records it: one steered into a running turn waits for the turn's next step,
// and one that starts a turn waits until the turn begins work, after any
// context compaction (here the captured compaction start). The live attachment
// that delivered it, having seen that turn end without it, retires it and
// sends its text again as the next turn. The pane shows the prompt pending
// throughout, then once, as the next turn's saved row, and the resend is
// accepted like any prompt. The live turn end and the saved commit of the
// abort arrive in either order, with no further commit to prompt the recovery.
//
// Stop means stop: when the operator stopped that turn (here twice before it
// ended, mid-compaction for the second variant), nothing is sent again, nor
// is the replacement the person sent into the stopping turn. Each stays shown
// once, held for the person's Resend or Discard.
//
// Boundary: Nano's Stop route, wire ingress, catalog and settlement, and a
// mounted live-text pane, on the captured Codex turns: the second was cut by
// Stop, and the third answers the resent prompt.
func TestLiveTextCodexPromptAnInterruptedTurnDroppedIsResentUnlessStopped(t *testing.T) {
	saved := fixtureLines(t, "codex-turns.saved.jsonl")
	wire := fixtureLines(t, "codex-turns.wire.jsonl")
	const dropped = "893574cb-2552-53b8-bbd1-667ea32ac097" // a turn Codex aborted before recording anything
	retarget := func(lines [][]byte, from, to string) [][]byte {
		out := make([][]byte, len(lines))
		for i, line := range lines {
			out[i] = bytes.ReplaceAll(line, []byte(from), []byte(to))
		}
		return out
	}
	const text = "Reply with exactly: CODEX_AFTER_STOP\n\n[from nano]"
	const replacement = "Stop asking the coordinator"
	compactionStarts := compactionFixture(t, "codex")[0].Line // carries the dropped turn's id
	for _, variant := range []struct {
		name  string
		turn  string   // the turn the prompt was handed to
		saved [][]byte // what Codex saved of that turn
		wire  [][]byte // what Nano received of that turn
	}{
		{name: "steered into a running turn", turn: codexFixtureTurn2, saved: saved[8:13], wire: wire[18:35]},
		{name: "started a turn still compacting", turn: dropped,
			saved: retarget([][]byte{saved[8], saved[12]}, codexFixtureTurn2, dropped),
			wire:  append(retarget([][]byte{wire[18]}, codexFixtureTurn2, dropped), append([][]byte{compactionStarts}, retarget([][]byte{wire[34]}, codexFixtureTurn2, dropped)...)...)},
	} {
		for _, test := range []struct {
			order   string
			stopped bool // the operator stopped the turn before it ended
		}{{order: "saved first"}, {order: "live first"}, {order: "saved first", stopped: true}, {order: "live first", stopped: true}} {
			order := test.order
			name := variant.name + "/" + order
			if test.stopped {
				name += "/after Stop"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				h := newLiveTextHarnessFor(t, "codex", transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40})
				sess := h.sess
				hosted, native := sess.HostedThreadID(), sess.ThreadID()
				var interrupts atomic.Int32

				seedTestSessionRegistry(&h.srv.Registry, map[string]*session{sess.ID(): sess})
				sess.ConfigureSavedReaders(nil, h.srv.savedPromptsFor(sess), nil, nil, nil)
				var settledCommits atomic.Int32
				h.service.SetTranscriptChanged(func(ctx context.Context, change transcriptChange) {
					sess.LiveTextService().Applied()
					h.srv.confirmDurableThread(ctx, change)
					settledCommits.Add(1)
				})
				sent := make(chan string, 4)
				var resendEnabled atomic.Bool
				runtime := connectProviderRuntimeFixture(t, sess, h.projection, native, "", func(f *providerRuntimeFixture) {
					f.onInterrupt = func() { interrupts.Add(1) }
					f.send = func(_ context.Context, text string) (json.RawMessage, error) {
						if !resendEnabled.Load() {
							return json.RawMessage(`{"turn":{"id":"` + variant.turn + `"}}`), nil
						}
						sent <- text
						return json.RawMessage(`{"turn":{"id":"` + codexFixtureTurn3 + `"}}`), nil
					}
				})
				h.view.neverSaved["reply:"+codexFixtureStopped] = true // Stop cut it, so Codex never saved it
				h.save(saved[:8])
				for _, line := range wire[:18] {
					runtime.emitAndWait(t, line)
				}
				h.waitTurnEnded(codexFixtureTurn1)

				// Exercise a real steer for the running-turn case and a real
				// start for the still-compacting case. Both register the saved-row wait.
				steered := variant.name == "steered into a running turn"
				if steered {
					runtime.emitAndWait(t, variant.wire[0])
				}
				if response := postTurn(t, sess, ctx, "lost", text); response.Code != http.StatusOK {
					t.Fatalf("lost prompt acceptance = %d %s", response.Code, response.Body.String())
				}
				turn := variant.turn
				if record, _, err := sess.PromptStore().Get(ctx, "lost"); err != nil || record.ProviderTurnID != turn {
					t.Fatalf("lost prompt bound to %+v, %v; want %s", record, err, turn)
				}
				h.expect("pending prompt", func(liveTextMessage) bool { return containsEntry(h.view.live, "prompt:lost") })
				// From here on the pane shows the prompt's text exactly once in
				// every message: the dropped prompt, then its resend in its
				// place, then the resend's saved row.
				shownOnce := func(m liveTextMessage) {
					t.Helper()
					shown := 0
					for _, entry := range h.view.live {
						if entry.Kind == "prompt" && entry.Text == text {
							shown++
						}
					}
					for _, row := range h.view.rows {
						if row.Type == "user_message" && row.Text == text {
							shown++
						}
					}
					if shown != 1 {
						t.Fatalf("the prompt shows %d times after a %s message; live %+v", shown, m.Type, h.view.live)
					}
				}

				// The turn's frames arrive as it runs; only its end, turn/completed,
				// races the saved commit of its abort.
				body, end := variant.wire[:len(variant.wire)-1], variant.wire[len(variant.wire)-1]
				for index, line := range body {
					if steered && index == 0 {
						continue
					}
					runtime.emitAndWait(t, line)
				}
				if test.stopped {
					for range 2 {
						rec := httptest.NewRecorder()
						sess.HandleInterrupt(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/x/interrupt", nil))
						if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"interrupted":true`) {
							t.Fatalf("Stop = %d %s", rec.Code, rec.Body.String())
						}
					}
					if got := interrupts.Load(); got != 2 {
						t.Fatalf("Stop reached the provider %d times, want 2", got)
					}
					// The replacement joins the same turn while its stop is pending.
					if response := postTurn(t, sess, ctx, "replacement", replacement); response.Code != http.StatusOK {
						t.Fatalf("replacement acceptance = %d %s", response.Code, response.Body.String())
					}
				}
				resendEnabled.Store(true)

				if order == "saved first" {
					// The abort's commit is applied and settled before live
					// reports the turn's end; no other commit follows it.
					before := settledCommits.Load()
					h.save(variant.saved)
					waitForCatalogWithin(t, 20*time.Second, func() bool { return settledCommits.Load() > before })
				}
				runtime.emitAndWait(t, end)
				if order == "live first" {
					h.save(variant.saved)
				}
				if test.stopped {
					for action, want := range map[string]string{"lost": text, "replacement": replacement} {
						waitForCatalogWithin(t, 20*time.Second, func() bool {
							record, _, err := sess.PromptStore().Get(ctx, action)
							return err == nil && record.Outbox.State == prompts.Uncertain
						})
						if record, _, _ := sess.PromptStore().Get(ctx, action); record.ProviderText != want {
							t.Fatalf("held %s text = %q, want it kept", action, record.ProviderText)
						}
						if view, code := readPrompt(t, sess, action); code != http.StatusOK || view.State != string(prompts.Uncertain) || !slices.Equal(view.AllowedActions, []string{promptResend, promptDiscard}) {
							t.Fatalf("stopped %s read = %d %+v; want held for the person", action, code, view)
						}
					}
					// Whatever the pane is sent meanwhile, it keeps showing the
					// prompt, once.
					for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
						select {
						case <-h.pane.ready:
						case <-time.After(50 * time.Millisecond):
						}
						for _, raw := range h.pane.take() {
							var message liveTextMessage
							if err := json.Unmarshal(raw, &message); err != nil {
								t.Fatal(err)
							}
							h.view.apply(t, message)
							shownOnce(message)
							replacements := 0
							for _, entry := range h.view.live {
								if entry.Kind == "prompt" && entry.Text == replacement {
									replacements++
								}
							}
							if replacements > 1 {
								t.Fatalf("the replacement shows %d times; live %+v", replacements, h.view.live)
							}
						}
					}
					if !containsEntry(h.view.live, "prompt:lost") || !containsEntry(h.view.live, "prompt:replacement") {
						t.Fatalf("a stopped prompt left the pane; live %+v", h.view.live)
					}
					select {
					case got := <-sent:
						t.Fatalf("a prompt caught by Stop was sent again: %q", got)
					case <-time.After(500 * time.Millisecond):
					}
					return
				}
				select {
				case got := <-sent:
					if got != text {
						t.Fatalf("resent %q, want the dropped prompt", got)
					}
				case <-time.After(20 * time.Second):
					t.Fatal("the prompt the interrupted turn dropped was never sent again")
				}

				var resent prompts.Record
				records, err := sess.PromptStore().List(ctx, hosted)
				if err != nil {
					t.Fatal(err)
				}
				for _, record := range records {
					if record.Outbox.ActionID != "lost" {
						resent = record
					}
				}
				waitTurnSendDone(t, sess, resent.Outbox.ActionID)
				if view, code := readPrompt(t, sess, resent.Outbox.ActionID); code != http.StatusOK || view.State != string(prompts.Accepted) {
					t.Fatalf("resend read = %d %+v; want accepted", code, view)
				}
				h.view.expectSaved["prompt:"+resent.Outbox.ActionID] = text
				// A Nano-submitted prompt carries its client identity on both
				// provider surfaces; the captured prompt was typed directly.
				clientID := submissionEventID(hosted, resent.Outbox.ActionID)
				savedResend := make([][]byte, 0, 7)
				for _, line := range saved[13:20] {
					savedResend = append(savedResend, bytes.ReplaceAll(line, []byte(`"type":"UserMessage",`), []byte(`"type":"UserMessage","client_id":"`+clientID+`",`)))
				}
				h.save(savedResend)
				for _, line := range wire[35:] {
					runtime.emitAndWait(t, bytes.ReplaceAll(line, []byte(`"clientId":null`), []byte(`"clientId":"`+clientID+`"`)))
				}
				h.expect("the resent prompt saved once", func(m liveTextMessage) bool {
					shownOnce(m)
					rows := 0
					for _, row := range h.view.rows {
						if row.Type == "user_message" && row.Text == text {
							rows++
						}
					}
					return rows == 1 && len(h.view.live) == 0
				})
				if old, _, err := sess.PromptStore().Get(ctx, "lost"); err != nil || old.Outbox.State != prompts.Expired || old.ProviderText != text {
					t.Fatalf("dropped prompt = %s %q, %v; want retired with its text kept", old.Outbox.State, old.ProviderText, err)
				}
				waitForCatalogWithin(t, 10*time.Second, func() bool {
					record, _, err := sess.PromptStore().Get(ctx, resent.Outbox.ActionID)
					return err == nil && record.Outbox.State == prompts.Confirmed
				})
				select {
				case extra := <-sent:
					t.Fatalf("sent a second time: %q", extra)
				default:
				}
			})
		}
	}
}

// A delivered prompt Nano holds because no saved row settled it before its
// turn ended is the person's to discard. Discard hides it for good, keeping its text in the store: an open pane,
// whose live text is the only place the prompt shows, drops it, and a reload
// does not bring it back. Boundary: the HTTP delete route and mounted
// live-text panes.
func TestLiveTextDiscardRemovesAnUnsavedDeliveredPrompt(t *testing.T) {
	ctx := context.Background()
	executions := newTestAgentExecutionStore(t, "run")
	h := newLiveTextHarnessForThread(t, "codex", transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40}, grokFixtureThread, func(cfg *sessionConfig) { cfg.AgentExecutions = executions })
	sess := h.sess // a Discard ends any operation the prompt carries
	hosted, native := sess.HostedThreadID(), sess.ThreadID()
	const text = "no, budget requests go to me"
	if _, _, err := sess.PromptStore().Submit(ctx, prompts.Submission{HostedThreadID: hosted, Provider: model.ProviderCodex, NativeThreadID: native, ActionID: "dropped",
		ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: prompts.HumanOrigin}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := sess.PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: "dropped", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	turn := codexFixtureTurn2
	accepted, err := sess.PromptStore().Accepted(ctx, prompts.Acceptance{ActionID: "dropped", ClaimToken: "dropped", AttachmentEpoch: 1, Binding: prompts.Binding{
		EventID: submissionEventID(hosted, "dropped"), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}}})
	if err != nil {
		t.Fatal(err)
	}
	held, ok, err := sess.PromptStore().HoldUntaken(ctx, "dropped", accepted.Revision, prompts.ReasonTurnEnded)
	if err != nil || !ok {
		t.Fatalf("hold = %v, %v", ok, err)
	}
	sess.PublishLiveOutbox(held.Outbox)
	h.expect("unsaved prompt shown", func(liveTextMessage) bool { return containsEntry(h.view.live, "prompt:dropped") })

	discard := httptest.NewRecorder()
	sess.HandleTurnDeliveryCancel(discard, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sess.ID()+"/turn-delivery/dropped", nil), "dropped")
	if discard.Code != http.StatusOK {
		t.Fatalf("discard status = %d, body = %s", discard.Code, discard.Body.String())
	}
	h.expect("discarded prompt gone", func(liveTextMessage) bool { return !containsEntry(h.view.live, "prompt:dropped") })
	if reloaded := h.open(transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40}); containsEntry(reloaded.view.live, "prompt:dropped") {
		t.Fatal("a reload brought the discarded prompt back")
	}
	if record, _, err := sess.PromptStore().Get(ctx, "dropped"); err != nil || record.Outbox.State != prompts.Expired || record.ProviderText != text {
		t.Fatalf("discarded prompt = %s %q, %v; want expired with its text kept", record.Outbox.State, record.ProviderText, err)
	}
}

// deliveryJournal reads the thread's delivery journal as a reloading browser
// does, by action id.
func (h *liveTextHarness) deliveryJournal() map[string]promptView {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.sess.HandleTurnDeliveryList(rec, httptest.NewRequest(http.MethodGet, "/turn-delivery?thread="+h.sess.ThreadID(), nil))
	var body struct {
		Operations []promptView `json:"operations"`
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
	rows := map[string]promptView{}
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
	hosted, native := h.sess.HostedThreadID(), h.sess.ThreadID()
	if _, _, err := h.sess.PromptStore().Submit(ctx, prompts.Submission{HostedThreadID: hosted, Provider: model.ProviderCodex, NativeThreadID: native, ActionID: action,
		ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: origin, CreatedAt: createdAt}); err != nil {
		h.t.Fatal(err)
	}
	if _, claimed, err := h.sess.PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: action, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		h.t.Fatalf("claim %s = %v, %v", action, claimed, err)
	}
	accepted, err := h.sess.PromptStore().Accepted(ctx, prompts.Acceptance{ActionID: action, ClaimToken: action, AttachmentEpoch: 1, Binding: prompts.Binding{
		EventID: submissionEventID(hosted, action), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}}})
	if err != nil {
		h.t.Fatal(err)
	}
	h.sess.PublishLiveOutbox(accepted.Outbox)
}

// An accepted prompt shows no dot, so one its provider never took must not
// stay accepted: once the saved transcript shows its turn ended without its
// row, the server holds it as uncertain, keeping its text, and the pane and
// a reload's journal both offer Resend and Discard. A prompt in a turn the
// file still shows running (a steer whose row Codex has not written yet, as
// in the operator's report of a dot above the agent's answer) stays accepted.
// So does one this process found already accepted at boot: history is never
// swept into Resend and Discard rows, some for prompts the provider took.
// Boundary: the settlement pass, the live stream and the journal route.
func TestLiveTextAcceptedPromptWhoseSavedTurnEndedWithoutItIsHeld(t *testing.T) {
	ctx := context.Background()
	h := newLiveTextHarnessFor(t, "codex", transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40})
	sess := h.sess
	sess.ConfigureSavedReaders(func(ctx context.Context, turnID string) (durableTurnEvidence, bool) {
		return h.srv.savedTurnEvidence(ctx, sess, turnID)
	}, h.srv.savedPromptsFor(sess), nil, nil, nil)
	const running = "01a0f650-0000-7000-8000-0000000000aa"
	saved := fixtureLines(t, "codex-turns.saved.jsonl")
	h.save(append(saved[:8:8], []byte(`{"timestamp":"2026-10-01T07:00:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"`+running+`"}}`)))
	h.waitTurnEnded(codexFixtureTurn1)

	const lost, steered, old = "an addendum the turn never took", "a steer Codex has not saved yet", "accepted before this process booted"
	h.acceptCodexPrompt("lost", lost, codexFixtureTurn1, prompts.AgentOrigin, "")
	h.acceptCodexPrompt("steered", steered, running, prompts.HumanOrigin, "")
	h.acceptCodexPrompt("old", old, codexFixtureTurn1, prompts.HumanOrigin, processStarted.Add(-time.Hour).UTC().Format(time.RFC3339Nano))
	h.expect("both prompts shown", func(liveTextMessage) bool {
		return containsEntry(h.view.live, "prompt:lost") && containsEntry(h.view.live, "prompt:steered")
	})
	if _, err := sess.SettlePrompts(ctx); err != nil {
		t.Fatal(err)
	}

	record, attempt, err := sess.PromptStore().Get(ctx, "lost")
	if err != nil || record.Outbox.State != prompts.Uncertain || attempt.Reason != prompts.ReasonTurnEnded || record.ProviderText != lost {
		t.Fatalf("lost prompt = %s %q %q, %v; want held uncertain with its text", record.Outbox.State, attempt.Reason, record.ProviderText, err)
	}
	if record, _, err := sess.PromptStore().Get(ctx, "steered"); err != nil || record.Outbox.State != prompts.Accepted {
		t.Fatalf("steered prompt = %s, %v; want accepted while its turn runs", record.Outbox.State, err)
	}
	if record, _, err := sess.PromptStore().Get(ctx, "old"); err != nil || record.Outbox.State != prompts.Accepted {
		t.Fatalf("prompt from before boot = %s, %v; want accepted, untouched", record.Outbox.State, err)
	}
	h.expect("held prompt offers Resend and Discard", func(liveTextMessage) bool {
		entry := liveEntry(h.view.live, "prompt:lost")
		return entry != nil && entry.State == string(prompts.Uncertain)
	})
	held := liveEntry(h.view.live, "prompt:lost")
	if !slices.Equal(held.AllowedActions, []string{promptResend, promptDiscard}) || held.Label != "Codex's turn ended without it" {
		t.Fatalf("held entry = %+v", *held)
	}
	if entry := liveEntry(h.view.live, "prompt:steered"); entry == nil || entry.State != string(prompts.Accepted) || entry.AllowedActions != nil || entry.Label != "" {
		t.Fatalf("steered entry = %+v; want accepted with nothing to do", entry)
	}
	journal := h.deliveryJournal()
	for _, id := range []string{"lost", "steered"} {
		row, entry := journal[id], liveEntry(h.view.live, "prompt:"+id)
		if row.State != entry.State || !slices.Equal(row.AllowedActions, entry.AllowedActions) || row.Label != entry.Label {
			t.Fatalf("%s: journal %q %v %q, stream %q %v %q", id, row.State, row.AllowedActions, row.Label, entry.State, entry.AllowedActions, entry.Label)
		}
	}
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
	h := newLiveTextHarnessFor(t, "codex", transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40})
	sess := h.sess
	hosted, native := sess.HostedThreadID(), sess.ThreadID()
	submit := func(action string, nextTurn bool) {
		text := "prompt " + action
		if _, _, err := sess.PromptStore().Submit(ctx, prompts.Submission{HostedThreadID: hosted, Provider: model.ProviderCodex, NativeThreadID: native, ActionID: action,
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: turnRequestHash(text, nextTurn), Origin: prompts.AgentOrigin, NextTurn: nextTurn}); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(action string) prompts.Record {
		claimed, ok, err := sess.PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: action, AttachmentEpoch: 1, Ready: true})
		if err != nil || !ok || claimed.Outbox.ActionID != action {
			t.Fatalf("claim %s = %s %v, %v", action, claimed.Outbox.ActionID, ok, err)
		}
		return claimed
	}
	submit("timed-out", false)
	claim("timed-out")
	uncertain, err := sess.PromptStore().Uncertain(ctx, prompts.Uncertainty{ActionID: "timed-out", ClaimToken: "timed-out", AttachmentEpoch: 1, Reason: context.DeadlineExceeded.Error()})
	if err != nil {
		t.Fatal(err)
	}
	submit("parked", false)
	claim("parked")
	parked, err := sess.PromptStore().Refused(ctx, prompts.Refusal{ActionID: "parked", ClaimToken: "parked", AttachmentEpoch: 1, State: prompts.Parked, Reason: "rpc error -32000: provider is not accepting input"})
	if err != nil {
		t.Fatal(err)
	}
	// Delivery began shortly before the threshold: the provider has not taken
	// it yet, and nothing about it changes again.
	submit("slow", false)
	slow, ok, err := sess.PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: "slow", AttachmentEpoch: 1, Ready: true,
		At: time.Now().Add(2*time.Second - promptSlowDelivery).UTC().Format(time.RFC3339Nano)})
	if err != nil || !ok || slow.Outbox.ActionID != "slow" {
		t.Fatalf("claim slow = %s %v, %v", slow.Outbox.ActionID, ok, err)
	}
	submit("addendum", true)
	queued, _, err := sess.PromptStore().Get(ctx, "addendum")
	if err != nil {
		t.Fatal(err)
	}
	sess.LiveTextService().OutboxChanged(uncertain.Outbox, parked.Outbox, slow.Outbox, queued.Outbox)
	h.expect("four prompts shown", func(liveTextMessage) bool { return len(h.view.live) == 4 })
	if entry := liveEntry(h.view.live, "prompt:slow"); entry.State != string(prompts.Delivering) || entry.Label != "" || entry.AllowedActions != nil {
		t.Fatalf("slow entry before the threshold = %+v; want delivering with no label or action", entry)
	}
	const slowLabel = "Codex hasn't taken it yet; sending again will deliver twice"
	h.expect("slow prompt relabeled", func(liveTextMessage) bool {
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
	sess.HandleTurnDeliveryRead(rec, httptest.NewRequest(http.MethodGet, "/turn-delivery/slow", nil), "slow")
	var read promptView
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

// Constructed from the captured claude-turn saved/wire shapes: one prompt
// and three distinct assistant messages, with the turn still running after
// the first two saves. A boot reattachment must join its fresh saved tail to
// the same turn's next live message, without losing or duplicating a row.
func TestLiveTextClaudeRestartJoinsSavedAndLiveMessages(t *testing.T) {
	h := newLiveTextHarness(t)
	saved := fixtureLines(t, "claude-multi-message-turn.constructed.saved.jsonl")
	wire := fixtureLines(t, "claude-multi-message-turn.constructed.wire.jsonl")
	const action = "restart-action"
	ctx := context.Background()
	hosted, native := h.sess.HostedThreadID(), h.sess.ThreadID()
	turn, promptID := "claude:"+hosted+":2", submissionEventID(hosted, action)
	for _, lines := range [][][]byte{saved, wire} {
		for index := range lines {
			lines[index] = bytes.ReplaceAll(lines[index], []byte(liveTextFixturePrompt), []byte(promptID))
		}
	}
	if _, _, err := h.sess.PromptStore().Submit(ctx, prompts.Submission{
		HostedThreadID: hosted, Provider: "claude", NativeThreadID: native, ActionID: action,
		ProviderText: "Continue the reply.", PresentationText: "Continue the reply.",
		TextHash: sendTextHash("Continue the reply."), RequestHash: sendTextHash("Continue the reply."), Origin: prompts.HumanOrigin,
	}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := h.sess.PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: "claim", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim: %v, %v", claimed, err)
	}
	accepted, err := h.sess.PromptStore().Accepted(ctx, prompts.Acceptance{ActionID: action, ClaimToken: "claim", AttachmentEpoch: 1,
		Binding: prompts.Binding{EventID: promptID, Scope: model.Scope{Kind: model.ScopeTurn, Provider: "claude", NativeThreadID: &native, NativeTurnID: &turn}}})
	if err != nil {
		t.Fatal(err)
	}
	h.view.expectSaved["prompt:"+action] = "Continue the reply."
	h.sess.LiveTextService().OutboxChanged(accepted.Outbox)
	h.projection.SetClaudeProviderTurnID(turn)
	h.feed(wire[:15]) // init and A1/A2, without a turn result
	h.save(saved[:3])
	h.expect("first two messages saved", func(liveTextMessage) bool {
		return len(h.view.rows) == 3 && len(h.view.live) == 0
	})
	before := append([]transcriptItem(nil), h.view.rows...)

	// Lose the old pane/session, as process exit does, but keep the provider
	// file, history store and transactional delivery journal. Do not close a
	// provider child: the restarted Nano attaches to the surviving shim.
	liveTextOf(h.sess).unsubscribe(h.pane)
	spool := []claudeTestFrame{recvFrame(1, string(wire[0])), sendFrame(2, string(saved[0])).WithTurn(turn)}
	for index, line := range wire[1:15] {
		spool = append(spool, recvFrame(int64(index+3), string(line)).WithTurn(turn))
	}
	attached := make(chan *shimConn, 1)
	shim := newFakeShim(t, resumeScript(func(c *shimConn, call shimCall) {
		c.Reply(call, map[string]any{"shimSessionId": "shim-1", "nativeSessionId": native, "state": "running", "turnActive": true, "lastSeq": 16})
		var request struct {
			FromSeq int64 `json:"fromSeq"`
		}
		if err := json.Unmarshal(call.Params, &request); err != nil {
			t.Error(err)
		}
		for _, frame := range spool {
			if frame.Seq > request.FromSeq {
				c.LineForTurn(frame.Seq, frame.Dir, frame.TurnID, frame.Line)
			}
		}
		c.Notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 16})
		attached <- c
	}))
	srv, attachments := claudeResumeServer(t, shim.WSURL())
	srv.catalog = h.service
	srv.controlMetadata = h.metadata
	srv.queuedTurns, srv.sendAttempts, srv.turns = h.queue.(*queuedTurnStore), h.journal.(*sendAttemptStore), h.sess.PromptStore()
	srv.agentExecutions, err = newControlAgentExecutionStore(srv.controlMetadata, "restart-test")
	if err != nil {
		t.Fatal(err)
	}
	attachments.remember(attachmentRecord{Provider: "claude", NativeThreadID: native, HostedThreadID: hosted,
		Cwd: h.sess.CWD(), Model: "sonnet", Bin: "/fixture/claude", ShimSessionID: "shim-1"})
	seedBootAttachments(t, srv)
	writeClaudeTestSpool(t, srv.defaults.claudeShimSpools, hosted, spool...)
	h.service.SetTranscriptChanged(srv.confirmDurableThread)
	srv.prepareBootReattach()
	srv.reattachWaitingHosted(hosted) // reattachStored -> spawnSession -> startLiveText -> attachClaudeSession
	fresh := srv.SessionForThread("claude", native)
	if fresh == nil || fresh == h.sess || fresh.LiveTextService() == h.sess.LiveTextService() {
		t.Fatal("boot did not construct a fresh session and live-text loop")
	}
	waitForClaude(t, "running turn reattached", func() bool { return fresh.Summary().TurnActive })
	restored, attempt, err := fresh.PromptStore().Get(ctx, action)
	if err != nil || restored.Outbox.Binding == nil || restored.Outbox.Binding.EventID != promptID || attempt.ProviderTurnID != turn {
		t.Fatalf("restored delivery binding: %+v, %+v, %v", restored, attempt, err)
	}
	h.sess = fresh
	h.liveTextViewer = h.open(transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40})
	assertRows := func(want int) {
		t.Helper()
		if len(h.view.rows) != want || len(h.view.live) != 0 {
			t.Fatalf("saved/live join: rows=%+v live=%+v, want %d saved rows only", h.view.rows, h.view.live, want)
		}
		for index, row := range h.view.rows {
			text := "Continue the reply."
			if index > 0 {
				text = fmt.Sprintf("Reply part %d.", index)
			}
			if row.Text != text || index > 0 && row.NativeItemID != fmt.Sprintf("msg_restart_%d:1", index) || index < len(before) && row.ID != before[index].ID {
				t.Fatalf("saved row %d lost identity or order: %+v", index, row)
			}
		}
	}
	// The pane opens with its default newest-tail query; there is no scroll
	// or paging assertion.
	assertRows(3)
	connection := <-attached
	for index, line := range wire[15:19] {
		connection.LineForTurn(int64(17+index), "recv", turn, string(line))
	}
	h.expect("third message streaming after the saved tail", func(liveTextMessage) bool {
		return h.view.reply() != nil && h.view.reply().Text == "Reply part 3."
	})
	if len(h.view.rows) != 3 || len(h.view.live) != 1 || h.view.live[0].ID != "reply:msg_restart_3:1" {
		t.Fatalf("third message did not follow A2: rows=%+v live=%+v", h.view.rows, h.view.live)
	}
	for index, line := range wire[19:22] {
		connection.LineForTurn(int64(21+index), "recv", turn, string(line))
	}
	h.expect("third message finished, still unsaved", func(liveTextMessage) bool { return h.view.reply() != nil && h.view.reply().State == "done" })
	h.save(saved[3:])
	h.expect("third message saved in the same update", func(m liveTextMessage) bool {
		if len(h.view.live) != 0 {
			return false
		}
		if m.Type != "update" || !slices.Contains(m.Saved, "reply:msg_restart_3:1") {
			t.Fatalf("handoff = %s, saved=%v; want one update retiring A3", m.Type, m.Saved)
		}
		return true
	}) // livePaneView.apply checks never-both and never-neither on every update
	assertRows(4)
	connection.LineForTurn(24, "recv", turn, string(wire[22]))
	waitForClaude(t, "same turn ended", func() bool { return !fresh.Summary().TurnActive })
	h.barrier()
	assertRows(4)
	if len(shim.CallsOf(shimMethodStart)) != 0 {
		t.Fatal("restart started a replacement Claude child")
	}
}

// An Enhanced pane's rows and turns carry only what the Enhanced view draws;
// the developer view's raw provider copy, tags and turn config were over a
// quarter of every tail. A dev pane keeps them.
func TestLiveTextEnhancedPaneOmitsDeveloperFields(t *testing.T) {
	h := newLiveTextHarness(t)
	h.save(fixtureLines(t, "claude-tool-error.saved.jsonl"))
	h.expect("saved rows", func(liveTextMessage) bool { return len(h.view.rows) >= 2 })
	rawTail := func(query transcriptListQuery) string {
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
	dev := rawTail(transcriptListQuery{View: devThreadTranscriptView, HiddenSpecified: true, Limit: 40})
	for _, field := range []string{`"detail":`, `"filterTags":`, `"config":`} {
		if !strings.Contains(dev, field) {
			t.Fatalf("the dev tail lacks %s, so this test cannot show the Enhanced tail dropping it", field)
		}
	}
	enhanced := rawTail(transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40})
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
	sessionMetadata
	db *sql.DB
}

func (m journalReadMetadata) ReadPool() *sql.DB { return m.db }

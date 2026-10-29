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
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/testsupport/claudeshim"
	"github.com/dkroshow/nano/internal/threadindex"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
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
	sess       *sessions.Session
	projection *sessions.LiveProjection
	runtime    *providerRuntimeFixture
	turn       string
	queue      sessions.Queue
	journal    sessions.Journal
	metadata   *hostedstore.MetadataStore
	service    *sidebar.CatalogSource
	store      *history.Store
	file       string
	// held keeps catalog apply notices from the loop.
	held         atomic.Bool
	barriers     int
	catalogReads atomic.Int64
	failRead     atomic.Bool
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

func newLiveTextHarnessForThread(t *testing.T, provider string, query threadindex.TranscriptQuery, grokThread string, configure ...func(*sessions.Config)) *liveTextHarness {
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
	probe := registerLiveReadCounter(t, &h.catalogReads, &h.failRead)
	store, err := history.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h.store = store
	installLiveReadCounter(t, databasePath, probe)
	service, err := newThreadCatalogService(context.Background(), store, threadindex.Options{
		Roots: []history.SourceRoot{{Provider: history.Provider(provider), Kind: rootKind, Path: root}}, DisableWatcher: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	srv := &server{catalog: service, browserDiagnostics: newBrowserDiagnosticStore(64)}
	cfg, generation, _ := providerSessionConfig(t, provider)
	for _, apply := range configure {
		apply(&cfg)
	}
	cfg.Projection = srv.newSessionProjection(provider, cfg.HostedThreadID)
	h.metadata, _ = cfg.Metadata.(*hostedstore.MetadataStore)
	cfg.Metadata = journalReadMetadata{Metadata: cfg.Metadata, db: cfg.QueuedTurns.Database()}
	sess := newSession(cfg)
	if err := sess.InitializeBinding(generation); err != nil {
		t.Fatal(err)
	}
	srv.startLiveText(sess)
	sess.ReserveNativeID(nativeThread)
	h.srv, h.sess, h.service = srv, sess, service
	h.projection = cfg.Projection
	h.turn = "claude:" + sess.HostedThreadID() + ":1"
	h.queue = cfg.QueuedTurns
	h.journal = cfg.SendAttempts
	service.SetTranscriptChanged(func(context.Context, threadindex.TranscriptChange) {
		if !h.held.Load() {
			sess.NotifySavedTranscript()
		}
	})
	h.liveTextViewer = h.open(query)
	return h
}

// open subscribes another pane and waits for its tail.
func (h *liveTextHarness) open(query threadindex.TranscriptQuery) *liveTextViewer {
	v := &liveTextViewer{t: h.t, pane: newLiveTextPane(query), view: &livePaneView{expectSaved: map[string]string{}, neverSaved: map[string]bool{}}}
	liveTextOf(h.sess).subscribe(v.pane)
	h.t.Cleanup(v.pane.Stop)
	v.expect("tail", func(m liveTextMessage) bool { return m.Type == "tail" })
	return v
}

// hold keeps catalog apply notices from the loop, so a test can act between
// a committed save and the loop learning of it; release delivers one.
func (h *liveTextHarness) hold() { h.held.Store(true) }

func (h *liveTextHarness) release() {
	h.held.Store(false)
	h.sess.NotifySavedTranscript()
}

// page reads the catalog directly: the test's own view of what is saved.
func (h *liveTextHarness) page(query threadindex.TranscriptQuery) []threadindex.TranscriptItem {
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
	f, err := os.Open(filepath.Join("..", "sessions", "testdata", "live_text", name))
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

// feed supplies captured evidence through the actual provider transport.
func (h *liveTextHarness) feed(lines [][]byte) {
	if h.runtime == nil {
		h.runtime = connectProviderRuntimeFixture(h.t, h.sess, h.projection, h.sess.ThreadID(), "fixture-model")
	}
	for _, line := range lines {
		h.runtime.emitAtAndWait(h.t, line, time.Now(), h.turn)
	}
}

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

// barrier waits until the loop has handled everything posted before it: the
// loop applies its inbox in arrival order, so a queued prompt posted now shows
// only after every earlier event. The prompt is then refused and gone.
func (h *liveTextHarness) barrier() {
	h.t.Helper()
	h.barriers++
	id := fmt.Sprintf("barrier-%d", h.barriers)
	entry := prompts.OutboxEntry{HostedThreadID: h.sess.HostedThreadID(), ActionID: id,
		Sequence: 99, PresentationText: "barrier", State: prompts.Queued}
	h.sess.PublishLiveOutbox(entry)
	h.expect("barrier", func(liveTextMessage) bool { return containsEntry(h.view.live, "prompt:"+id) })
	entry.State = prompts.Refused
	h.sess.PublishLiveOutbox(entry)
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
		for _, view := range []liveview.View{liveview.Messages, liveview.Full} {
			rec := httptest.NewRecorder()
			h.sess.HandleConversationView(rec, httptest.NewRequest("GET", "/conversation?view="+string(view), nil), view)
			out["conversation "+string(view)] = rec.Body.String()
		}
		rec := httptest.NewRecorder()
		threadindex.NewHandlers(h.srv.catalog).HandleConversationHistory(rec, httptest.NewRequest("GET", "/conversation/history", nil), h.sess.ThreadID())
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
	cursor := liveStatusForTest(t, h.sess)
	rec := httptest.NewRecorder()
	h.sess.HandleConversationView(rec, httptest.NewRequest("GET", "/conversation?view=messages", nil), liveview.Messages)
	var snapshot liveview.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil || snapshot.Epoch != cursor.Epoch || snapshot.Revision != cursor.Revision {
		t.Fatalf("snapshot cursor = %s/%d (%v), want the status cursor %s/%d", snapshot.Epoch, snapshot.Revision, err, cursor.Epoch, cursor.Revision)
	}
	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.sess.HandleConversationEventsView(w, r, liveview.Messages)
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
	var first liveview.Update
	json.Unmarshal(nextData(), &first)
	h.runtime.activate(t)
	var next liveview.Update
	for next.Op == "" || next.Op == "rows.changed" {
		next = liveview.Update{}
		json.Unmarshal(nextData(), &next)
	}
	if first.Epoch != next.Epoch || next.Revision <= first.Snapshot.Revision || next.Op != projectionTurnUpsert {
		t.Fatalf("stream = %s/%d then %s %s/%d, want a later status update in the snapshot's epoch",
			first.Epoch, first.Snapshot.Revision, next.Op, next.Epoch, next.Revision)
	}
	// A failed read is an error to agents, never an empty transcript.
	h.store.Close()
	rec = httptest.NewRecorder()
	h.sess.HandleConversationView(rec, httptest.NewRequest("GET", "/conversation?view=messages", nil), liveview.Messages)
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
			h := newLiveTextHarnessForThread(t, "claude", threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40}, grokFixtureThread, func(cfg *sessions.Config) { cfg.AgentExecutions = store })
			if _, err := fixtureEnsureSession(store, h.sess.HostedThreadID(), "claude", presentationShared); err != nil {
				t.Fatal(err)
			}
			// Open the reader before admission so its cache has no operation rows.
			readerRun := "before-restart"
			if tc.restart {
				readerRun = "after-restart"
			}
			emptyReader, err := newControlAgentExecutionStore(control, readerRun)
			if err != nil {
				t.Fatal(err)
			}
			want := make(map[string]string)
			for index, stamp := range tc.times {
				action := fmt.Sprintf("prompt-%d", index)
				if !tc.missing {
					op, _, err := fixtureHistoricalTurn(store, h.sess.HostedThreadID(), action, "hash", hostedstore.TurnAttribution{
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
				if _, _, err := h.sess.Delivery().PromptStore().Submit(context.Background(), prompts.Submission{
					HostedThreadID: h.sess.HostedThreadID(), Provider: "claude", NativeThreadID: h.sess.ThreadID(),
					ActionID: action, ProviderText: "The same prompt.", PresentationText: "The same prompt.",
					TextHash: sendTextHash("The same prompt."), RequestHash: sendTextHash("The same prompt."), Origin: prompts.HumanOrigin,
				}); err != nil {
					t.Fatal(err)
				}
			}
			// A fresh loop restores the committed outbox instead of receiving
			// fabricated live entries. The reader opened before admission has
			// no cached operations, requiring the scoped committed lookup.
			liveTextOf(h.sess).unsubscribe(h.pane)
			store = emptyReader
			fresh := newFixtureSession(sessions.Config{ID: h.sess.ID(), HostedThreadID: h.sess.HostedThreadID(), Provider: "claude", Projection: h.projection, Turns: h.sess.Delivery().PromptStore(), AgentExecutions: store}, h.sess.ThreadID())
			h.srv.startLiveText(fresh)
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
				h := newLiveTextHarnessFor(t, "codex", threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40})
				sess := h.sess
				hosted, native := sess.HostedThreadID(), sess.ThreadID()
				var interrupts atomic.Int32

				seedTestSessionRegistry(&h.srv.Registry, map[string]*sessions.Session{sess.ID(): sess})
				sess.ConfigureSavedReaders(nil, h.srv.savedPromptsFor(sess), nil, nil, nil)
				var settledCommits atomic.Int32
				h.service.SetTranscriptChanged(func(ctx context.Context, change threadindex.TranscriptChange) {
					sess.NotifySavedTranscript()
					fixtureOwners(h.srv).attachmentCoordinator().ConfirmDurableThread(ctx, change)
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
				h.runtime = runtime
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
				if response := postTurn(t, sess.Delivery(), ctx, "lost", text); response.Code != http.StatusOK {
					t.Fatalf("lost prompt acceptance = %d %s", response.Code, response.Body.String())
				}
				turn := variant.turn
				if record, _, err := sess.Delivery().PromptStore().Get(ctx, "lost"); err != nil || record.ProviderTurnID != turn {
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
						sess.Delivery().HandleInterrupt(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/x/interrupt", nil))
						if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"interrupted":true`) {
							t.Fatalf("Stop = %d %s", rec.Code, rec.Body.String())
						}
					}
					if got := interrupts.Load(); got != 2 {
						t.Fatalf("Stop reached the provider %d times, want 2", got)
					}
					// The replacement joins the same turn while its stop is pending.
					if response := postTurn(t, sess.Delivery(), ctx, "replacement", replacement); response.Code != http.StatusOK {
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
							record, _, err := sess.Delivery().PromptStore().Get(ctx, action)
							return err == nil && record.Outbox.State == prompts.Uncertain
						})
						if record, _, _ := sess.Delivery().PromptStore().Get(ctx, action); record.ProviderText != want {
							t.Fatalf("held %s text = %q, want it kept", action, record.ProviderText)
						}
						if view, code := readPrompt(t, sess.Delivery(), action); code != http.StatusOK || view.State != string(prompts.Uncertain) || !slices.Equal(view.AllowedActions, []string{promptResend, promptDiscard}) {
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
				records, err := sess.Delivery().PromptStore().List(ctx, hosted)
				if err != nil {
					t.Fatal(err)
				}
				for _, record := range records {
					if record.Outbox.ActionID != "lost" {
						resent = record
					}
				}
				waitTurnSendDone(t, sess, resent.Outbox.ActionID)
				if view, code := readPrompt(t, sess.Delivery(), resent.Outbox.ActionID); code != http.StatusOK || view.State != string(prompts.Accepted) {
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
				if old, _, err := sess.Delivery().PromptStore().Get(ctx, "lost"); err != nil || old.Outbox.State != prompts.Expired || old.ProviderText != text {
					t.Fatalf("dropped prompt = %s %q, %v; want retired with its text kept", old.Outbox.State, old.ProviderText, err)
				}
				waitForCatalogWithin(t, 10*time.Second, func() bool {
					record, _, err := sess.Delivery().PromptStore().Get(ctx, resent.Outbox.ActionID)
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
	h := newLiveTextHarnessForThread(t, "codex", threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40}, grokFixtureThread, func(cfg *sessions.Config) { cfg.AgentExecutions = executions })
	sess := h.sess // a Discard ends any operation the prompt carries
	hosted, native := sess.HostedThreadID(), sess.ThreadID()
	const text = "no, budget requests go to me"
	if _, _, err := sess.Delivery().PromptStore().Submit(ctx, prompts.Submission{HostedThreadID: hosted, Provider: model.ProviderCodex, NativeThreadID: native, ActionID: "dropped",
		ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: prompts.HumanOrigin}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := sess.Delivery().PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: "dropped", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	turn := codexFixtureTurn2
	accepted, err := sess.Delivery().PromptStore().Accepted(ctx, prompts.Acceptance{ActionID: "dropped", ClaimToken: "dropped", AttachmentEpoch: 1, Binding: prompts.Binding{
		EventID: submissionEventID(hosted, "dropped"), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}}})
	if err != nil {
		t.Fatal(err)
	}
	held, ok, err := sess.Delivery().PromptStore().HoldUntaken(ctx, "dropped", accepted.Revision, prompts.ReasonTurnEnded)
	if err != nil || !ok {
		t.Fatalf("hold = %v, %v", ok, err)
	}
	sess.PublishLiveOutbox(held.Outbox)
	h.expect("unsaved prompt shown", func(liveTextMessage) bool { return containsEntry(h.view.live, "prompt:dropped") })

	discard := httptest.NewRecorder()
	sess.Delivery().HandleTurnDeliveryCancel(discard, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sess.ID()+"/turn-delivery/dropped", nil), "dropped")
	if discard.Code != http.StatusOK {
		t.Fatalf("discard status = %d, body = %s", discard.Code, discard.Body.String())
	}
	h.expect("discarded prompt gone", func(liveTextMessage) bool { return !containsEntry(h.view.live, "prompt:dropped") })
	if reloaded := h.open(threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40}); containsEntry(reloaded.view.live, "prompt:dropped") {
		t.Fatal("a reload brought the discarded prompt back")
	}
	if record, _, err := sess.Delivery().PromptStore().Get(ctx, "dropped"); err != nil || record.Outbox.State != prompts.Expired || record.ProviderText != text {
		t.Fatalf("discarded prompt = %s %q, %v; want expired with its text kept", record.Outbox.State, record.ProviderText, err)
	}
}

// deliveryJournal reads the thread's delivery journal as a reloading browser
// does, by action id.
func (h *liveTextHarness) deliveryJournal() map[string]prompts.View {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.sess.Delivery().HandleTurnDeliveryList(rec, httptest.NewRequest(http.MethodGet, "/turn-delivery?thread="+h.sess.ThreadID(), nil))
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
	hosted, native := h.sess.HostedThreadID(), h.sess.ThreadID()
	if _, _, err := h.sess.Delivery().PromptStore().Submit(ctx, prompts.Submission{HostedThreadID: hosted, Provider: model.ProviderCodex, NativeThreadID: native, ActionID: action,
		ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: origin, CreatedAt: createdAt}); err != nil {
		h.t.Fatal(err)
	}
	if _, claimed, err := h.sess.Delivery().PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: action, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		h.t.Fatalf("claim %s = %v, %v", action, claimed, err)
	}
	accepted, err := h.sess.Delivery().PromptStore().Accepted(ctx, prompts.Acceptance{ActionID: action, ClaimToken: action, AttachmentEpoch: 1, Binding: prompts.Binding{
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
	h := newLiveTextHarnessFor(t, "codex", threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40})
	sess := h.sess
	sess.ConfigureSavedReaders(func(ctx context.Context, turnID string) (liveview.DurableTurnEvidence, bool) {
		return h.srv.hostedCatalog().SavedTurnEvidence(ctx, sess.Provider(), sess.ThreadID(), turnID)
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
	if _, err := sess.Delivery().SettlePrompts(ctx); err != nil {
		t.Fatal(err)
	}

	record, attempt, err := sess.Delivery().PromptStore().Get(ctx, "lost")
	if err != nil || record.Outbox.State != prompts.Uncertain || attempt.Reason != prompts.ReasonTurnEnded || record.ProviderText != lost {
		t.Fatalf("lost prompt = %s %q %q, %v; want held uncertain with its text", record.Outbox.State, attempt.Reason, record.ProviderText, err)
	}
	if record, _, err := sess.Delivery().PromptStore().Get(ctx, "steered"); err != nil || record.Outbox.State != prompts.Accepted {
		t.Fatalf("steered prompt = %s, %v; want accepted while its turn runs", record.Outbox.State, err)
	}
	if record, _, err := sess.Delivery().PromptStore().Get(ctx, "old"); err != nil || record.Outbox.State != prompts.Accepted {
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
	if _, _, err := h.sess.Delivery().PromptStore().Submit(ctx, prompts.Submission{
		HostedThreadID: hosted, Provider: "claude", NativeThreadID: native, ActionID: action,
		ProviderText: "Continue the reply.", PresentationText: "Continue the reply.",
		TextHash: sendTextHash("Continue the reply."), RequestHash: sendTextHash("Continue the reply."), Origin: prompts.HumanOrigin,
	}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := h.sess.Delivery().PromptStore().Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: "claim", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim: %v, %v", claimed, err)
	}
	accepted, err := h.sess.Delivery().PromptStore().Accepted(ctx, prompts.Acceptance{ActionID: action, ClaimToken: "claim", AttachmentEpoch: 1,
		Binding: prompts.Binding{EventID: promptID, Scope: model.Scope{Kind: model.ScopeTurn, Provider: "claude", NativeThreadID: &native, NativeTurnID: &turn}}})
	if err != nil {
		t.Fatal(err)
	}
	h.view.expectSaved["prompt:"+action] = "Continue the reply."
	h.sess.PublishLiveOutbox(accepted.Outbox)
	h.turn = turn
	h.feed(wire[:15]) // init and A1/A2, without a turn result
	h.save(saved[:3])
	h.expect("first two messages saved", func(liveTextMessage) bool {
		return len(h.view.rows) == 3 && len(h.view.live) == 0
	})
	before := append([]threadindex.TranscriptItem(nil), h.view.rows...)

	// Lose the old pane/session, as process exit does, but keep the provider
	// file, history store and transactional delivery journal. Do not close a
	// provider child: the restarted Nano attaches to the surviving shim.
	liveTextOf(h.sess).unsubscribe(h.pane)
	spool := []claudeshim.Frame{recvFrame(1, string(wire[0])), sendFrame(2, string(saved[0])).WithTurn(turn)}
	for index, line := range wire[1:15] {
		spool = append(spool, recvFrame(int64(index+3), string(line)).WithTurn(turn))
	}
	attached := make(chan *claudeshim.Conn, 1)
	shim := newFakeShim(t, resumeScript(func(c *claudeshim.Conn, call claudeshim.Call) {
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
	srv.queuedTurns, srv.sendAttempts, srv.turns = h.queue.(*prompts.LegacyQueue), h.journal.(*prompts.LegacyJournal), h.sess.Delivery().PromptStore()
	srv.agentExecutions, err = newControlAgentExecutionStore(srv.controlMetadata, "restart-test")
	if err != nil {
		t.Fatal(err)
	}
	attachments.Remember(hostedstore.Attachment{Provider: "claude", NativeThreadID: native, HostedThreadID: hosted,
		Cwd: h.sess.CWD(), Model: "sonnet", Bin: "/fixture/claude", ShimSessionID: "shim-1"})
	seedBootAttachments(t, srv)
	writeClaudeTestSpool(t, srv.defaults.ClaudeShimSpools, hosted, spool...)
	h.service.SetTranscriptChanged(fixtureOwners(srv).attachmentCoordinator().ConfirmDurableThread)
	fixtureOwners(srv).attachmentCoordinator().PrepareBoot()
	fixtureReattach(srv, hosted) // reattachStored -> spawnSession -> startLiveText -> attachClaudeSession
	fresh := srv.SessionForThread("claude", native)
	if fresh == nil || fresh == h.sess || fresh.LiveTextService() == h.sess.LiveTextService() {
		t.Fatal("boot did not construct a fresh session and live-text loop")
	}
	waitForClaude(t, "running turn reattached", func() bool { return fresh.Summary().TurnActive })
	restored, attempt, err := fresh.Delivery().PromptStore().Get(ctx, action)
	if err != nil || restored.Outbox.Binding == nil || restored.Outbox.Binding.EventID != promptID || attempt.ProviderTurnID != turn {
		t.Fatalf("restored delivery binding: %+v, %+v, %v", restored, attempt, err)
	}
	h.sess = fresh
	h.liveTextViewer = h.open(threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, Limit: 40})
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

// journalReadMetadata keeps the harness's metadata owner while reading prompts
// from its separately seeded canonical prompt database.
type journalReadMetadata struct {
	sessions.Metadata
	db *sql.DB
}

func (m journalReadMetadata) ReadPool() *sql.DB { return m.db }

func (s *server) newSessionProjection(provider, hostedThreadID string) *sessions.LiveProjection {
	return sessions.NewLiveProjection(provider, hostedThreadID, s.dropLiveTextDeltas)
}

package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"slices"
	"sort"
	"strings"

	"time"

	"github.com/dkroshow/nano/internal/history"

	"github.com/dkroshow/nano/internal/threadindex"
	threadvalues "github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// proofs is what one read proves: saved names the entries whose saved rows
// it holds; gone adds the ended replies the saved row of a prompt delivered
// after them supersedes, since an unsaved reply's text stays until the next
// prompt. Only a prompt proves a later turn: a reply may be a replay. For
// Claude this is exact, because the file is written and indexed in order.
func (l *liveTextLoop) proofs(matched map[string]string, ended map[string]bool) (saved, gone map[string]bool) {
	saved, gone = make(map[string]bool), make(map[string]bool)
	var latest int64
	for _, entry := range l.entries {
		if entry.key != "" && matched[entry.key] != "" || l.sess.Provider() == "grok" && entry.Kind == "prompt" && entry.turn != "" && matched[entry.turn] != "" {
			saved[entry.ID], gone[entry.ID] = true, true
			if entry.Kind == "prompt" {
				latest = max(latest, entry.order)
			}
		}
	}
	for _, entry := range l.entries {
		if entry.Kind == "reply" && (entry.endOrder > 0 && entry.endOrder < latest || ended[entry.turn]) {
			gone[entry.ID] = true
		}
	}
	return saved, gone
}

// reloadEnds names the Codex turns whose end the file records, when this
// read reloads a browser pane: a new pane, or a fresh tail after a rebuild. A
// Codex Stop leaves no saved reply, so a reload clears the stopped reply's
// text once the file proves its turn ended; Claude saves a stopped reply and
// needs none. An agent stream never reloads what the operator sees.
func (l *liveTextLoop) reloadEnds(panes []*liveTextPane, joining map[*liveTextPane]bool, responses []threadindex.TranscriptResponse) map[string]bool {
	ended := map[string]bool{}
	if l.sess.Provider() != "codex" {
		return ended
	}
	reloading := false
	for i, pane := range panes {
		// A pane's first tail once the thread is indexed replaces nothing.
		reloading = reloading || !pane.rowsOnly && (joining[pane] || pane.key != "" && pane.needsTail(responses[i]))
	}
	if !reloading {
		return ended
	}
	for _, response := range responses {
		for _, turn := range response.Turns {
			// A page reads a turn in progress until the file records its end.
			if turn.NativeTurnID != "" && turn.Status != string(history.TurnInProgress) {
				ended[turn.NativeTurnID] = true
			}
		}
	}
	return ended
}

// replyMatch and promptMatch look up the exact identities a provider's saved
// rows carry: a Claude reply's message block and prompt's stamped UUID; a
// Codex reply's item id in its turn, and prompt's turn; Grok's newest chunk
// event id and the Nano submission id. Grok prompts also prove their saved turn
// in tick. replyMatch takes each reply's key with its turn.
func (l *liveTextLoop) replyMatch(turns map[string]string) history.TranscriptMatch {
	keys := make([]string, 0, len(turns))
	for key := range turns {
		keys = append(keys, key)
	}
	if l.sess.Provider() == "grok" {
		return history.TranscriptMatch{Namespace: "native.eventId", ItemType: "agent_message", Keys: keys}
	}
	if l.sess.Provider() == "codex" {
		return history.TranscriptMatch{Namespace: history.MatchTurnNativeItem, ItemType: "agent_message", Keys: keys, Turns: turns}
	}
	return history.TranscriptMatch{Namespace: l.sess.Provider() + ".messageBlock", ItemType: "agent_message", Keys: keys}
}

// claimedTurnEvents names, for each turn a Codex prompt without a client id
// waits on, the client ids of the other prompts bound to that turn. Their
// rows are theirs, so the turn's remaining row is this prompt's. It reads the
// durable bindings, not live entries: a prompt that already left, or was
// confirmed before a restart, still claims its row. A turn whose claims cannot
// be read is left out of this tick's keys: without them its row is unproven.
func (l *liveTextLoop) claimedTurnEvents(turns []string) (map[string][]string, []string) {
	if l.sess.PromptStore() == nil {
		return nil, nil
	}
	claimed := make(map[string][]string)
	readable := make(map[string]bool) // one read per distinct turn
	var proven []string
	for _, turn := range turns {
		ok, read := readable[turn]
		if !read {
			events, err := l.sess.PromptStore().TurnEvents(context.Background(), l.sess.HostedThreadID(), model.ProviderCodex, l.sess.ThreadID(), turn)
			if ok = err == nil; !ok {
				log.Printf("live text turn events %s: %v", turn, err)
			} else if len(events) > 0 {
				claimed[turn] = events
			}
			readable[turn] = ok
		}
		if ok {
			proven = append(proven, turn)
		}
	}
	return claimed, proven
}

func (l *liveTextLoop) promptMatch(keys []string) history.TranscriptMatch {
	if l.sess.Provider() == "grok" {
		return history.TranscriptMatch{Namespace: "nano.submissionId", ItemType: "user_message", Keys: keys}
	}
	if l.sess.Provider() == "codex" {
		return history.TranscriptMatch{Namespace: history.MatchTurnItem, ItemType: "user_message", Keys: keys}
	}
	return history.TranscriptMatch{Namespace: "native.eventId", ItemType: "user_message", Keys: keys}
}

// liveTextCatalog reads pages of a thread and checks exact keys in one
// catalog transaction.
type liveTextCatalog interface {
	LoadTranscripts(context.Context, string, []threadindex.TranscriptQuery, []history.TranscriptMatch) ([]threadindex.TranscriptResponse, map[string]string, error)
}

var _ liveTextCatalog = (*threadCatalogService)(nil)

func (l *liveTextLoop) read(threadID string, queries []threadindex.TranscriptQuery, match []history.TranscriptMatch) ([]threadindex.TranscriptResponse, map[string]string, error) {
	catalog, ok := l.srv.catalog.(liveTextCatalog)
	if !ok {
		return nil, nil, errors.New("catalog unavailable")
	}
	return catalog.LoadTranscripts(context.Background(), threadID, queries, match)
}

var errLiveTextNotIndexed = errors.New("thread not in the catalog yet")

func notIndexed(err error) bool {
	return errors.Is(err, errLiveTextNotIndexed) || errors.Is(err, threadindex.ErrCatalogThreadNotFound)
}

func (l *liveTextLoop) catalogThreadID() (string, error) {
	if l.sess == nil {
		return "", errLiveTextNotIndexed
	}
	native := l.sess.ThreadID()
	resolver, ok := l.srv.catalog.(threadindex.ReferenceResolver)
	if native == "" || !ok {
		return "", errLiveTextNotIndexed
	}
	id, err := resolver.ResolveThreadReference(context.Background(), native)
	if errors.Is(err, threadindex.ErrCatalogThreadNotFound) {
		return "", errLiveTextNotIndexed
	}
	return id, err
}

// tick reads every due pane's newest page and checks every held key in one
// catalog transaction, so one answer holds for every pane. Text the read
// proves saved leaves every pane in the message that brings its row, and
// retires. Joining panes get their tail from the same read, and panes asking
// for the same page share one read. Warm panes are due when they join and
// when their coalesced read comes (warm), never while a pane on screen shows
// the thread.
func (l *liveTextLoop) tick(joining map[*liveTextPane]bool, warm bool) {
	shown := l.shownOnScreen()
	var panes []*liveTextPane
	for pane := range l.panes {
		switch {
		case !pane.warm:
			if !warm {
				panes = append(panes, pane)
			}
		case warm || joining[pane] || shown:
			// A warm pane rides a pane on screen's reads (they ask for the
			// same page), so the browser's cached rows stay current while
			// the thread is shown, and no second read is made.
			panes = append(panes, pane)
		}
	}
	if len(panes) == 0 {
		return // held text is checked when a pane opens
	}
	var queries []threadindex.TranscriptQuery
	page := make([]int, len(panes)) // each pane's index in queries
	asked := make(map[string]int)
	for i, pane := range panes {
		key := pageQueryKey(pane.query)
		index, ok := asked[key]
		if !ok {
			index, asked[key] = len(queries), len(queries)
			queries = append(queries, pane.query)
		}
		page[i] = index
	}
	var promptKeys, promptTurns []string
	replyTurns := make(map[string]string)
	// A Codex prompt with a client id shares its turn with others, so it is
	// found by that id within its turn; one without keeps its turn key.
	eventTurns := make(map[string]string)
	for _, entry := range l.entries {
		if l.sess.Provider() == "grok" && entry.Kind == "prompt" && entry.turn != "" {
			promptTurns = append(promptTurns, entry.turn)
		}
		switch {
		case entry.key == "":
		case entry.Kind == "prompt" && l.sess.Provider() == "codex" && entry.turn != "" && entry.key != entry.turn:
			eventTurns[entry.key] = entry.turn
		case entry.Kind == "prompt":
			promptKeys = append(promptKeys, entry.key)
		default:
			replyTurns[entry.key] = entry.turn
		}
	}
	var match []history.TranscriptMatch
	if len(promptTurns) > 0 {
		match = append(match, history.TranscriptMatch{Namespace: history.MatchTurnItem, ItemType: "user_message", Keys: promptTurns})
	}
	if l.sess.Provider() == "codex" && len(promptKeys) > 0 {
		var claimed map[string][]string
		claimed, promptKeys = l.claimedTurnEvents(promptKeys)
		if len(promptKeys) > 0 {
			prompt := l.promptMatch(promptKeys)
			prompt.Exclude = claimed
			match = append(match, prompt)
		}
	} else if len(promptKeys) > 0 {
		match = append(match, l.promptMatch(promptKeys))
	}
	if len(eventTurns) > 0 {
		keys := make([]string, 0, len(eventTurns))
		for key := range eventTurns {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		match = append(match, history.TranscriptMatch{Namespace: history.MatchTurnEvent, ItemType: "user_message", Keys: keys, Turns: eventTurns})
	}
	if len(replyTurns) > 0 {
		match = append(match, l.replyMatch(replyTurns))
	}

	threadID, err := l.catalogThreadID()
	var responses []threadindex.TranscriptResponse
	var matched map[string]string
	if err == nil {
		responses, matched, err = l.read(threadID, queries, match)
	}
	if notIndexed(err) {
		// Nothing is saved yet: joining panes start empty, the rest wait.
		for pane := range joining {
			if !pane.warm {
				l.tail(pane, livePage{}, nil)
			}
		}
		return
	}
	if err != nil {
		// A failed read is a bug: every pane shows it instead of retrying.
		log.Printf("live text read %s: %v", l.hostedID(), err)
		for _, pane := range panes {
			pane.fail("the saved transcript could not be read")
			delete(l.panes, pane)
		}
		return
	}
	pages := make([]livePage, len(responses))
	for i, response := range responses {
		pages[i] = newLivePage(response)
	}
	paneResponses := make([]threadindex.TranscriptResponse, len(panes))
	for i := range panes {
		paneResponses[i] = responses[page[i]]
	}
	saved, gone := l.proofs(matched, l.reloadEnds(panes, joining, paneResponses))
	for _, entry := range append([]*liveTextEntry(nil), l.entries...) {
		if gone[entry.ID] {
			l.drop(entry)
		}
	}
	for i, pane := range panes {
		read := pages[page[i]]
		if pane.reads != nil {
			pane.reads.Add(1)
		}
		switch {
		case pane.warm && !settledProjection(read.Projection.State):
			// A warm pane sends nothing until the projection settles, so a
			// rebuild reaches it as one fresh tail.
		case joining[pane]:
			l.tail(pane, read, saved)
		default:
			l.updatePane(pane, read, saved)
		}
	}
}

// livePage is one page read and its rows' digests, computed once per read and
// shared by every pane that asked for that page.
type livePage struct {
	threadindex.TranscriptResponse
	digests [][sha256.Size]byte
}

func newLivePage(response threadindex.TranscriptResponse) livePage {
	page := livePage{TranscriptResponse: response, digests: make([][sha256.Size]byte, len(response.Items))}
	for i, item := range response.Items {
		raw, _ := json.Marshal(item)
		page.digests[i] = sha256.Sum256(raw)
	}
	return page
}

// pageQueryKey names one page query: panes with the same key read the same
// page.
func pageQueryKey(query threadindex.TranscriptQuery) string {
	return fmt.Sprintf("%s\x00%s\x00%t\x00%d", query.View, strings.Join(query.HiddenCategories, ","), query.HiddenSpecified, query.Limit)
}

func settledProjection(state string) bool {
	return state == string(history.TranscriptReady) || state == string(history.TranscriptEmpty) || state == string(history.TranscriptRetained)
}

// promptsSavedIn names, as live-text ids, the prompts whose saved rows are
// among these page rows: the store lists the candidates (TailCandidates, read
// after the page, so every prompt whose row the page holds is among them) and
// one catalog read matches their exact keys to rows. Only a row id in the page
// names a prompt, so a row saved after the page was read names nothing. A
// Codex prompt with only a turn key is left out: its turn may hold several
// rows, and the browser's record read covers it.
func (l *liveTextLoop) promptsSavedIn(rows []threadindex.TranscriptItem) ([]string, error) {
	if l.sess == nil || l.sess.PromptStore() == nil {
		return nil, nil
	}
	var oldest time.Time
	page := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.Type != "user_message" {
			continue
		}
		page[row.ID] = true
		if row.CreatedAt == nil {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, *row.CreatedAt); err == nil && (oldest.IsZero() || at.Before(oldest)) {
			oldest = at
		}
	}
	if len(page) == 0 || oldest.IsZero() {
		return nil, nil
	}
	candidates, err := l.sess.PromptStore().TailCandidates(context.Background(), l.hostedID(), oldest)
	if err != nil {
		return nil, err
	}
	actions := make(map[string][]string) // key -> action ids
	eventTurns := make(map[string]string)
	var keys []string
	for _, entry := range candidates {
		key := l.promptKey(entry)
		if key == "" || l.sess.Provider() == "codex" && (entry.Binding == nil || entry.Binding.EventID == "") {
			continue
		}
		if l.sess.Provider() == "codex" && entry.Binding.Scope.NativeTurnID != nil {
			eventTurns[key] = *entry.Binding.Scope.NativeTurnID
		} else if len(actions[key]) == 0 {
			keys = append(keys, key)
		}
		actions[key] = append(actions[key], entry.ActionID)
	}
	var match []history.TranscriptMatch
	if len(keys) > 0 {
		slices.Sort(keys)
		match = append(match, l.promptMatch(keys))
	}
	if len(eventTurns) > 0 {
		events := make([]string, 0, len(eventTurns))
		for key := range eventTurns {
			events = append(events, key)
		}
		slices.Sort(events)
		match = append(match, history.TranscriptMatch{Namespace: history.MatchTurnEvent, ItemType: "user_message", Keys: events, Turns: eventTurns})
	}
	if len(match) == 0 {
		return nil, nil
	}
	threadID, err := l.catalogThreadID()
	if err != nil {
		return nil, err
	}
	_, matched, err := l.read(threadID, nil, match)
	if err != nil {
		return nil, err
	}
	var ids []string
	for key, itemID := range matched {
		if page[itemID] {
			for _, action := range actions[key] {
				ids = append(ids, "prompt:"+action)
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func savedConversationItem(row threadindex.TranscriptItem) liveProjectionItem {
	itemType := row.Type
	switch row.Type {
	case "user_message":
		itemType = "userMessage"
	case "agent_message":
		itemType = "agentMessage"
	}
	createdAt := ""
	if row.CreatedAt != nil {
		createdAt = *row.CreatedAt
	}
	return liveProjectionItem{ID: row.ID, DurableItemID: row.ID, NativeItemID: row.NativeItemID, ProjectedSeq: row.ProjectedSeq,
		TurnID: row.TurnID, Type: itemType, Category: row.Category, Role: row.Role, Phase: row.Phase, Text: row.Text,
		Status: threadvalues.FirstNonEmpty(row.Status, "completed"), Done: true, CreatedAt: createdAt, ToolName: row.ToolName, CallID: row.CallID,
		ContentRef: row.ContentRef, ContentTruncated: row.ContentTruncated, OriginalBytes: int(row.OriginalBytes)}
}

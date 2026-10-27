package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/protocol"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// claudeHandoff drives one real thread catalog over a Claude session file and
// one live Claude projection: the saved file and the wire.

const handoffNative = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

type claudeHandoff struct {
	catalog    *gateCatalog
	path       string
	threadID   string
	session    *session
	projection *liveProjection
	at         time.Time
	sequence   uint64
}

// newClaudeHandoff starts the catalog over a Claude session that already has
// one completed saved turn, then attaches a live projection the way a hosted
// session does.
func newClaudeHandoff(t *testing.T) *claudeHandoff {
	t.Helper()
	h := &claudeHandoff{at: time.Now().UTC().Add(-time.Hour)}
	var file strings.Builder
	h.writeSavedTurn(&file, "saved-prompt-0", "earlier prompt", "earlier answer")
	h.catalog = newGateCatalog(t, nil, nil)
	h.path = filepath.Join(h.catalog.roots[history.ProviderClaude], handoffNative+".jsonl")
	writeGateFile(t, h.path, []byte(file.String()))
	h.catalog.service.RequestPathRefresh([]string{h.path})
	h.threadID = h.catalog.threadID(t, handoffNative, 2)

	h.projection = newLiveProjection("claude", "hosted-handoff")
	h.projection.setNativeThreadID(handoffNative)
	h.session = &session{id: "session-handoff", threadID: handoffNative, provider: "claude",
		hostedThreadID: "hosted-handoff", projection: h.projection}
	server := h.catalog.server
	server.sessions = map[string]*session{h.session.id: h.session}
	h.session.savedTurn = func(ctx context.Context, turnID string) (durableTurnEvidence, bool) {
		return server.savedTurnEvidence(ctx, h.session, turnID)
	}
	return h
}

func (h *claudeHandoff) stamp() string {
	h.at = h.at.Add(time.Second)
	return h.at.Format(time.RFC3339Nano)
}

func (h *claudeHandoff) writeSavedTurn(file *strings.Builder, eventID, prompt, answer string) {
	fmt.Fprintf(file, `{"type":"user","uuid":%q,"sessionId":%q,"timestamp":%q,"message":{"role":"user","content":[{"type":"text","text":%q}]}}`+"\n",
		eventID, handoffNative, h.stamp(), prompt)
	fmt.Fprintf(file, `{"type":"assistant","uuid":%q,"sessionId":%q,"timestamp":%q,"message":{"id":%q,"role":"assistant","content":[{"type":"text","text":%q}],"stop_reason":"end_turn"}}`+"\n",
		eventID+"-answer", handoffNative, h.stamp(), "msg-"+eventID, answer)
	fmt.Fprintf(file, `{"type":"system","subtype":"turn_duration","durationMs":1000,"uuid":%q,"sessionId":%q,"timestamp":%q}`+"\n",
		eventID+"-duration", handoffNative, h.stamp())
}

// appendSaved writes rows to the Claude session file and waits until the
// catalog has committed them.
func (h *claudeHandoff) appendSaved(t *testing.T, rows string, wantText string) {
	t.Helper()
	file, err := os.OpenFile(h.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(rows); err != nil {
		t.Fatal(err)
	}
	file.Close()
	h.catalog.service.RequestPathRefresh([]string{h.path})
	waitForCatalogWithin(t, time.Minute, func() bool {
		page, err := h.catalog.service.LoadTranscript(context.Background(), h.threadID, transcriptListQuery{
			Limit: maxThreadTranscriptLimit, View: "dev", HiddenSpecified: true,
		})
		if err != nil {
			return false
		}
		return slices.ContainsFunc(page.Items, func(item transcriptItem) bool { return item.Text == wantText })
	})
}

// submit records one accepted Nano prompt for providerTurn and sends it over
// the Claude wire with its exact submission UUID. It returns that UUID.
func (h *claudeHandoff) submit(t *testing.T, action, providerTurn, text string) string {
	t.Helper()
	h.sequence++
	native := handoffNative
	entry := prompts.OutboxEntry{
		HostedThreadID: "hosted-handoff", ActionID: action, Sequence: h.sequence,
		State: prompts.Accepted, PresentationText: text,
		Binding: &prompts.Binding{Scope: model.Scope{
			Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &providerTurn,
		}},
	}
	h.session.publishTransactionalOutbox(entry)
	h.projection.setClaudeProviderTurnID(providerTurn)
	eventID := submissionEventID("hosted-handoff", action)
	h.projection.applyWire(protocol.Send, []byte(fmt.Sprintf(`{"type":"user","uuid":%q,"message":{"role":"user","content":[{"type":"text","text":%q}]}}`, eventID, text)), time.Now())
	return eventID
}

func (h *claudeHandoff) reply(eventID, text string) {
	h.projection.applyWire(protocol.Recv, []byte(fmt.Sprintf(`{"type":"assistant","session_id":%q,"uuid":%q,"message":{"id":%q,"role":"assistant","content":[{"type":"text","text":%q}],"stop_reason":"end_turn"}}`,
		handoffNative, eventID+"-answer", "msg-"+eventID, text)), time.Now())
}

// stream sends an unsaved reply as the CLI streams it: a message start, then
// one text block still streaming, which is what live text shows.
func (h *claudeHandoff) stream(id, text string) {
	for _, line := range []string{messageStart(id),
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
		textDelta(0, text)} {
		h.projection.applyWire(protocol.Recv, []byte(line), time.Now())
	}
}

func (h *claudeHandoff) finish(text string) {
	h.projection.applyWire(protocol.Recv, []byte(fmt.Sprintf(`{"type":"result","subtype":"success","is_error":false,"result":%q,"session_id":%q,"duration_ms":1000}`, text, handoffNative)), time.Now())
}

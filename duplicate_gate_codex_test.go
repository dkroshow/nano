package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// Duplicate gate for Codex serialization twins: Codex writes each item twice,
// and saved history must serve it once. Helpers live in duplicate_gate_test.go.

const (
	gateShortPrompt  = "is the build finished?"
	gateShortAnswer  = "Yes—it finished."
	gateOrdersPrompt = "let's go through the four open orders"
	gateOrdersReply  = "I’ll pull the durable state, recent commentary, owner, and execution status for all four, then we can decide what to keep, revive, close, or drop."
)

// TestDuplicateGateCodexSerializationTwinsServedOnce is the twin gate.
// Boundary: saved Codex history as the transcript and messages endpoints serve
// it. Codex writes each item twice (an event copy and a response copy). It
// catches the three pairs reader v36 joins being served as two rows:
//  1. a completed AgentMessage with a local id (item-16) and its response message;
//  2. a legacy agent_reasoning event and its response reasoning;
//  3. a completed Reasoning with a local id (item-2, item-4) and its response reasoning.
//
// Red on reader v35; the v36 pairing rule turns it green.
func TestDuplicateGateCodexSerializationTwinsServedOnce(t *testing.T) {
	catalog := newGateCatalog(t, []string{
		"codex-agent-message-local-id.jsonl",
		"codex-agent-reasoning-event.jsonl",
		"codex-completed-reasoning-local-id.jsonl",
	}, nil)
	checkerPrompt := gateFixtureText(t, "codex-completed-reasoning-local-id.jsonl", "message", "You are the independent checker.")
	checkerFirst := gateFixtureText(t, "codex-completed-reasoning-local-id.jsonl", "message", "I’m starting the independent read-only check.")
	checkerSecond := gateFixtureText(t, "codex-completed-reasoning-local-id.jsonl", "message", "The checkout matches the required commit")
	ordersFinal := gateFixtureText(t, "codex-agent-reasoning-event.jsonl", "message", "All four are open.")
	cases := []struct {
		name, nativeID string
		expected       []gateRow
	}{
		{"completed AgentMessage with local id", "40b7284b-c7dd-56b1-a8d1-f5f536dbe04c", []gateRow{
			gateUser(gateShortPrompt), gateAgent(gateShortAnswer),
		}},
		{"legacy agent_reasoning event", "397cb5c8-3ab7-5c8f-a6b1-566ced0e6b00", []gateRow{
			gateUser(gateOrdersPrompt),
			gateReasoning("**Planning parallel task inspection**"), gateAgent(gateOrdersReply),
			gateReasoning("**Verifying hosted-threads endpoint usage**"), gateAgent(ordersFinal),
		}},
		{"completed Reasoning with local id", "c6f79940-ea48-5a90-98eb-cf99c8578e04", []gateRow{
			gateUser(checkerPrompt),
			gateReasoning("**Planning repository inspection and role review**\n\n**Optimizing file reading for commit checks**"),
			gateAgent(checkerFirst),
			gateReasoning("**Planning heartbeat feature inspection**"), gateAgent(checkerSecond),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := catalog.threadID(t, tc.nativeID, 2)
			assertGateRows(t, "transcript", tc.expected, catalog.servedTranscript(t, id))
			assertGateRows(t, "messages", gateMessageRows(tc.expected), catalog.servedMessages(t, id))
		})
	}
}

// TestDuplicateGateCodexTwinSplitAcrossAppendServedOnce catches a twin join
// that works on a whole-file parse but not when the file grows between the two
// copies. Boundary: the catalog's append path, which resumes the parser from
// its saved checkpoint, as served by the transcript endpoint after each write.
// Red on reader v35 after the second write; v36 turns it green.
func TestDuplicateGateCodexTwinSplitAcrossAppendServedOnce(t *testing.T) {
	raw := string(readGateFixture(t, "codex-agent-message-local-id.jsonl"))
	split := strings.Index(raw, `"type":"response_item","payload":{"type":"message","id":"msg_06704d457e13504cb653b37f41964dde"`)
	split = strings.LastIndex(raw[:split], "\n") + 1
	if split <= 0 {
		t.Fatal("fixture has no response copy of the answer")
	}
	catalog := newGateCatalog(t, nil, nil)
	path := filepath.Join(catalog.roots[history.ProviderCodex], "rollout-40b7284b-c7dd-56b1-a8d1-f5f536dbe04c.jsonl")
	writeGateFile(t, path, []byte(raw[:split]))
	catalog.service.RequestPathRefresh([]string{path})
	expected := []gateRow{gateUser(gateShortPrompt), gateAgent(gateShortAnswer)}
	id := catalog.threadID(t, "40b7284b-c7dd-56b1-a8d1-f5f536dbe04c", 2)
	assertGateRows(t, "transcript after the event copy", expected, catalog.servedTranscript(t, id))

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(raw[split:]); err != nil {
		t.Fatal(err)
	}
	file.Close()
	catalog.service.RequestPathRefresh([]string{path})
	waitForCatalogWithin(t, time.Minute, func() bool {
		page, err := catalog.service.LoadTranscript(context.Background(), id, transcriptListQuery{Limit: 10, HiddenSpecified: true, View: defaultThreadTranscriptView})
		return err == nil && len(page.Turns) == 1 && page.Turns[0].Status == "completed"
	})
	assertGateRows(t, "transcript after the response copy", expected, catalog.servedTranscript(t, id))
}

// TestDuplicateGateCodexUserTwinKeepsItsFirstNativeID catches a prompt's two
// Codex serializations, each carrying its own native id, failing the saved
// transcript with conflicting_item_identity: the first copy is committed under
// its id, and the joined item must not later claim the second. Boundary: the
// real catalog's SQLite applies, when the twin is split across appends, split
// across a full rebuild's 4 MiB read batches, or read in one batch. Each case
// saves the prompt once, under the first id, with no transcript error.
func TestDuplicateGateCodexUserTwinKeepsItsFirstNativeID(t *testing.T) {
	const firstID = "msg_user-response-copy"
	raw := string(readGateFixture(t, "codex-agent-message-local-id.jsonl"))
	lines := strings.SplitAfter(raw, "\n")
	// The fixture's response copy of the prompt (line 4) carries no id; Codex
	// also writes one with an id, as here, before the completed event (line 5).
	lines[3] = strings.Replace(lines[3], `"payload":{"type":"message",`, `"payload":{"type":"message","id":"`+firstID+`",`, 1)
	if !strings.Contains(lines[3], firstID) || !strings.Contains(lines[4], `"type":"UserMessage"`) {
		t.Fatal("fixture lines moved")
	}
	head, twin, tail := strings.Join(lines[:4], ""), lines[4], strings.Join(lines[5:], "")
	// One world_state record, which the parser keeps as a non-content
	// observation, sized so the response copy ends 100 bytes before the
	// 4 MiB read boundary and its twin crosses it.
	prefix := strings.Join(lines[:3], "")
	filler := `{"timestamp":"2026-09-15T20:13:08.100Z","type":"world_state","payload":{"pad":""}}` + "\n"
	pad := strings.Repeat("x", 4<<20-100-len(prefix)-len(filler)-len(lines[3]))
	rebuild := prefix + strings.Replace(filler, `"pad":""`, `"pad":"`+pad+`"`, 1) + lines[3]
	if end := len(rebuild); end > 4<<20 || end+len(twin) <= 4<<20 {
		t.Fatalf("response copy ends at %d; its twin must cross %d", end, 4<<20)
	}

	expected := []gateRow{gateUser(gateShortPrompt), gateAgent(gateShortAnswer)}
	native := "40b7284b-c7dd-56b1-a8d1-f5f536dbe04c"
	for _, tc := range []struct {
		name   string
		writes []string
	}{
		{"split across appends", []string{head, twin + tail}},
		{"split across rebuild batches", []string{rebuild + twin + tail}},
		{"one batch", []string{head + twin + tail}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := newGateCatalog(t, nil, nil)
			path := filepath.Join(catalog.roots[history.ProviderCodex], "rollout-"+native+".jsonl")
			var id string
			var written int64
			for index, chunk := range tc.writes {
				file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString(chunk); err != nil {
					t.Fatal(err)
				}
				file.Close()
				written += int64(len(chunk))
				catalog.service.RequestPathRefresh([]string{path})
				if index == 0 {
					id = catalog.threadID(t, native, 0)
				}
				if index < len(tc.writes)-1 {
					// The first copy must be committed before its twin arrives.
					waitForCatalogWithin(t, time.Minute, func() bool {
						checkpoint, exists, err := catalog.service.store.LoadTranscriptCheckpointHeader(context.Background(), id)
						return err == nil && exists && checkpoint.ReadOffset == written
					})
				}
			}
			size := int64(len(strings.Join(tc.writes, "")))
			var checkpoint history.TranscriptCheckpoint
			waitForCatalogWithin(t, time.Minute, func() bool {
				var exists bool
				var err error
				checkpoint, exists, err = catalog.service.store.LoadTranscriptCheckpointHeader(context.Background(), id)
				return err == nil && exists && (checkpoint.ErrorSummary != nil || checkpoint.ReadOffset == size)
			})
			if checkpoint.ErrorSummary != nil {
				t.Fatalf("saved transcript failed: %s", *checkpoint.ErrorSummary)
			}
			assertGateRows(t, "transcript", expected, catalog.servedTranscript(t, id))
			page, err := catalog.service.store.ReadTranscript(context.Background(), history.TranscriptReadRequest{ThreadID: id})
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range page.Items {
				if item.Type == history.ItemUserMessage && item.NativeItemID != firstID {
					t.Fatalf("prompt saved under native id %q, want the first copy's %q", item.NativeItemID, firstID)
				}
			}
		})
	}
}

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	featuremail "github.com/dkroshow/nano/internal/features/mail"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threads"
)

func TestAgentSenderUsesResolvedIdentity(t *testing.T) {
	for _, route := range []string{"attribution", "turn", "inbox"} {
		for _, scenario := range []string{"source name only", "source name overrides catalog", "catalog fallback", "no hosted record", "missing native", "provisional native", "missing provider", "no name", "provider mismatch", "native mismatch"} {
			t.Run(route+"/"+scenario, func(t *testing.T) {
				s, store, workerSession, sender, worker, prompts, _ := stage0Server(t, func(store *hostedstore.ExecutionStore, sender threads.SessionRecord) {
					native, provider := sender.NativeThreadID, sender.Provider
					switch scenario {
					case "missing native":
						native = ""
					case "provisional native":
						native = "cs_provisional"
					case "missing provider":
						provider = ""
					}
					if _, err := store.Control().CanonicalDatabase().Exec(`UPDATE hosted_threads SET current_native_id=?,provider=? WHERE hosted_thread_id=?`, native, provider, sender.ID); err != nil {
						t.Fatal(err)
					}
				})
				t.Cleanup(func() {
					if op, ok := fixtureTurnForAction(store, worker.ID, "identity-check"); ok {
						waitForAgentTurnStatus(t, store, op.ID, agentTurnRunning)
						waitForTurnSendDone(t, workerSession, op.TransportActionID)
					}
				})
				s.catalog = nil
				liveConfig := sessions.Config{ID: "nano-sender", HostedThreadID: sender.ID, Provider: sender.Provider, DisplayName: "Source sender", CreatedAt: time.Now()}
				live := newFixtureSession(liveConfig, sender.NativeThreadID)
				s.RegisterSession(live)
				wantName, wantResolvedName, wantCode := "Source sender", "Source sender", ""
				switch scenario {
				case "source name overrides catalog":
					s.catalog = fixtureNamedCatalog(t, sender.Provider, sender.NativeThreadID, "Old catalog name")
				case "catalog fallback":
					s.RemoveSession(live.ID())
					s.catalog = fixtureNamedCatalog(t, sender.Provider, sender.NativeThreadID, "Catalog sender")
					wantName, wantResolvedName = "Catalog sender", "Catalog sender"
				case "no hosted record":
					sender.ID = "nht_00000000000000000000000000000000"
					wantCode = "sender_not_hosted"
				case "missing native":
					sender.NativeThreadID = ""
					wantCode = "sender_identity_incomplete"
				case "provisional native":
					sender.NativeThreadID = "cs_provisional"
					wantCode = "sender_identity_incomplete"
				case "missing provider":
					sender.Provider = ""
					wantCode = "sender_identity_incomplete"
				case "no name":
					live.SetDisplayName("")
					prefix := sender.NativeThreadID
					if len(prefix) > 8 {
						prefix = prefix[:8]
					}
					wantName = sender.Provider + " " + prefix + " (unnamed)"
					wantResolvedName = ""
				case "provider mismatch":
					liveConfig.Provider = "codex"
					live = newFixtureSession(liveConfig, sender.NativeThreadID)
					s.RegisterSession(live)
					s.catalog = fixtureNamedCatalog(t, sender.Provider, sender.NativeThreadID, "Catalog sender")
					wantCode = "sender_identity_incomplete"
				case "native mismatch":
					live = newFixtureSession(liveConfig, identityNativeNew)
					s.RegisterSession(live)
					s.catalog = fixtureNamedCatalog(t, sender.Provider, sender.NativeThreadID, "Catalog sender")
					wantCode = "sender_identity_incomplete"
				}
				if scenario == "source name only" {
					var names int
					if err := s.controlMetadata.CanonicalDatabase().QueryRow(`SELECT count(*) FROM thread_metadata_current WHERE hosted_thread_id=? AND field='name'`, sender.ID).Scan(&names); err != nil || names != 0 {
						t.Fatalf("fixture metadata names=%d, err=%v", names, err)
					}
					if name := s.hostedIdentity().DisplayNameForHosted(context.Background(), sender.Provider, sender.ID, sender.NativeThreadID); name != "" {
						t.Fatalf("fixture unexpectedly has a catalog name: %q", name)
					}
				}
				if wantCode == "" {
					code, resolved, raw := resolveIdentity(t, s, sender.ID)
					if code != http.StatusOK || resolved.DisplayName != wantResolvedName {
						t.Fatalf("identity endpoint = %d %s", code, raw)
					}
				}
				persistedName := wantName
				if scenario == "no name" {
					persistedName = ""
				}
				if route == "attribution" {
					got, err := fixtureOwners(s).hostedController().TurnAttributionFor(sender)
					if wantCode != "" {
						if err == nil {
							t.Fatalf("incomplete identity accepted: %+v", got)
						}
						return
					}
					if err != nil || got.SenderName != persistedName || got.SenderHostedThreadID != sender.ID || got.SenderNativeThreadID != sender.NativeThreadID || got.SenderProvider != sender.Provider {
						t.Fatalf("attribution=%+v, err=%v", got, err)
					}
					if footer := senderFooter(got); !strings.Contains(footer, "[from "+wantName+" · "+sender.NativeThreadID+" · submitted ") {
						t.Fatalf("footer=%q", footer)
					}
					return
				}
				var response *httptest.ResponseRecorder
				if route == "turn" {
					body, _ := json.Marshal(map[string]string{"clientActionId": "identity-check", "prompt": "Review", "senderThreadRef": sender.ID})
					response = httptest.NewRecorder()
					fixtureOwners(s).hostedController().HandleTurnSubmit(response, httptest.NewRequest(http.MethodPost, "/turns", bytes.NewReader(body)), worker.ID)
				} else {
					var message featuremail.Message
					response, message = sendAgentMessageForTest(t, s, worker, sender.ID, "identity-check", "Review", "Review", nil)
					if wantCode == "" && response.Code == http.StatusCreated {
						if message.Sender.Name != persistedName || message.Sender.HostedThreadID != sender.ID || message.Sender.NativeThreadID != sender.NativeThreadID || message.Sender.Provider != sender.Provider {
							t.Fatalf("inbox attribution=%+v", message.Sender)
						}
						return
					}
				}
				if wantCode != "" {
					var body struct {
						Error struct {
							Code string `json:"code"`
						} `json:"error"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusBadRequest || body.Error.Code != wantCode {
						t.Fatalf("refusal=%d %s, want %s", response.Code, response.Body, wantCode)
					}
					if _, exists := fixtureTurnForAction(store, worker.ID, "identity-check"); exists {
						t.Fatal("refused sender created a turn")
					}
					if s.queuedTurns.QueuedCount(worker.Provider, worker.NativeThreadID, worker.ID) != 0 || len(prompts) != 0 {
						t.Fatal("refused sender queued or delivered a turn")
					}
					var messages int
					if err := s.controlMetadata.CanonicalDatabase().QueryRow(`SELECT count(*) FROM agent_messages`).Scan(&messages); err != nil || messages != 0 {
						t.Fatalf("refused sender persisted mail: count=%d err=%v", messages, err)
					}
					return
				}
				if response.Code != http.StatusAccepted {
					t.Fatalf("submit=%d %s", response.Code, response.Body)
				}
				op, exists := fixtureTurnForAction(store, worker.ID, "identity-check")
				if !exists || op.SenderName != persistedName || op.SenderNativeThreadID != sender.NativeThreadID || op.SenderProvider != sender.Provider {
					t.Fatalf("turn attribution=%+v", op)
				}
				select {
				case prompt := <-prompts:
					if !strings.Contains(prompt, "[from "+wantName+" · "+sender.NativeThreadID+" · submitted ") {
						t.Fatalf("provider prompt=%q", prompt)
					}
				case <-time.After(time.Second):
					t.Fatal("no provider delivery")
				}
			})
		}
	}
}

func TestAgentSenderCannotOverrideResolvedName(t *testing.T) {
	for _, route := range []string{"turn", "inbox"} {
		t.Run(route, func(t *testing.T) {
			s, store, _, sender, recipient, prompts, _ := stage0Server(t)
			body := map[string]string{"senderThreadRef": sender.ID, "senderName": "Caller supplied name"}
			response := httptest.NewRecorder()
			if route == "turn" {
				body["clientActionId"], body["prompt"] = "caller-name", "Review"
				encoded, _ := json.Marshal(body)
				fixtureOwners(s).hostedController().HandleTurnSubmit(response, httptest.NewRequest(http.MethodPost, "/turns", bytes.NewReader(encoded)), recipient.ID)
				if response.Code != http.StatusAccepted {
					t.Fatalf("caller name admission=%d %s", response.Code, response.Body)
				}
				op, exists := fixtureTurnForAction(store, recipient.ID, "caller-name")
				if !exists || op.SenderName != "Claude sender" {
					t.Fatalf("caller name replaced canonical attribution: %+v", op)
				}
				return
			} else {
				body["clientMessageId"], body["body"] = "caller-name", "Review"
				encoded, _ := json.Marshal(body)
				fixtureOwners(s).agentController().HandleHostedThreadMessageSend(response, httptest.NewRequest(http.MethodPost, "/messages", bytes.NewReader(encoded)), recipient)
			}
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_message_request") {
				t.Fatalf("caller name admission=%d %s", response.Code, response.Body)
			}
			if _, exists := fixtureTurnForAction(store, recipient.ID, "caller-name"); exists || len(prompts) != 0 {
				t.Fatal("caller supplied name admitted a turn")
			}
			var messages int
			if err := s.controlMetadata.CanonicalDatabase().QueryRow(`SELECT count(*) FROM agent_messages`).Scan(&messages); err != nil || messages != 0 {
				t.Fatalf("caller supplied name persisted mail: count=%d err=%v", messages, err)
			}
		})
	}
}

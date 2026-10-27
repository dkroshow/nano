package subscriptions

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/interact"
)

type recordingClient struct {
	messages []interact.SendMessageRequest
	prompts  []interact.SubmitPromptRequest
}

func (c *recordingClient) SendMessage(_ context.Context, request interact.SendMessageRequest) (json.RawMessage, error) {
	c.messages = append(c.messages, request)
	return json.RawMessage(`{"messageId":"message"}`), nil
}

func (c *recordingClient) SubmitPrompt(_ context.Context, request interact.SubmitPromptRequest) (interact.Turn, error) {
	c.prompts = append(c.prompts, request)
	return interact.Turn{ID: "turn"}, nil
}

func (c *recordingClient) SubscribeCompletedTurns(context.Context, interact.PageRequest) (interact.CompletedTurnPage, error) {
	return interact.CompletedTurnPage{}, nil
}

func TestChoicesDefaultToInboxOnly(t *testing.T) {
	got, err := NormalizeChoices(Choices{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Inbox != Send || got.Prompt != None {
		t.Fatalf("choices = %+v, want inbox send and prompt none", got)
	}
}

func TestDeliveryChannelsAreIndependent(t *testing.T) {
	for _, tc := range []struct {
		name         string
		choices      Choices
		wantMessages int
		wantPrompts  int
	}{
		{"inbox", Choices{Inbox: Send, Prompt: None}, 1, 0},
		{"prompt", Choices{Inbox: None, Prompt: Send}, 0, 1},
		{"both", Choices{Inbox: Send, Prompt: Send}, 1, 1},
		{"neither", Choices{Inbox: None, Prompt: None}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &recordingClient{}
			event := Event{ChildThreadRef: "child", InvokerThreadRef: "parent", OperationID: "aop_1", WorkerDisplay: "worker (uuid)", Status: "completed"}
			if err := Deliver(context.Background(), client, tc.choices, event); err != nil {
				t.Fatal(err)
			}
			if len(client.messages) != tc.wantMessages || len(client.prompts) != tc.wantPrompts {
				t.Fatalf("deliveries = %d messages, %d prompts", len(client.messages), len(client.prompts))
			}
			if len(client.prompts) == 1 {
				prompt := client.prompts[0]
				if prompt.Origin != interact.SystemOrigin || prompt.ThreadRef != "parent" || prompt.ClientID != "nano:subscription:aop_1" {
					t.Fatalf("prompt = %+v", prompt)
				}
			}
			if len(client.messages) == 1 {
				message := client.messages[0]
				if message.RecipientThreadRef != "parent" || message.SenderThreadRef != "child" || message.ClientMessageID != "subscription:aop_1" || message.DispositionFeedback {
					t.Fatalf("message = %+v", message)
				}
			}
		})
	}
}

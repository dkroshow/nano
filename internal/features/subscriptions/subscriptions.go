package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dkroshow/nano/internal/interact"
)

const (
	Send = "send"
	None = "none"
)

type Choices struct {
	Inbox  string `json:"inbox,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}

func FromStored(policy string, legacyPrompt bool) Choices {
	switch policy {
	case "inbox":
		return Choices{Inbox: Send, Prompt: None}
	case "prompt", "individual", "coalesced":
		return Choices{Inbox: None, Prompt: Send}
	case "inbox+prompt":
		return Choices{Inbox: Send, Prompt: Send}
	case "none":
		return Choices{Inbox: None, Prompt: None}
	default:
		if legacyPrompt {
			return Choices{Inbox: None, Prompt: Send}
		}
		return Choices{Inbox: None, Prompt: None}
	}
}

func StoredPolicy(value Choices) string {
	switch {
	case value.Inbox == Send && value.Prompt == Send:
		return "inbox+prompt"
	case value.Inbox == Send:
		return "inbox"
	case value.Prompt == Send:
		return "prompt"
	default:
		return "none"
	}
}

func NormalizeChoices(value Choices) (Choices, error) {
	value.Inbox, value.Prompt = strings.TrimSpace(value.Inbox), strings.TrimSpace(value.Prompt)
	if value.Inbox == "" {
		value.Inbox = Send
	}
	if value.Prompt == "" {
		value.Prompt = None
	}
	if value.Inbox != Send && value.Inbox != None {
		return Choices{}, fmt.Errorf("subscriptions.inbox must be %q or %q", Send, None)
	}
	if value.Prompt != Send && value.Prompt != None {
		return Choices{}, fmt.Errorf("subscriptions.prompt must be %q or %q", Send, None)
	}
	return value, nil
}

type Event struct {
	ChildThreadRef   string
	InvokerThreadRef string
	OperationID      string
	TurnID           string
	WorkerDisplay    string
	Status           string
}

type Client interface {
	interact.CompletedTurnSubscriber
	interact.MessageSender
	interact.PromptSubmitter
}

func Deliver(ctx context.Context, client Client, choices Choices, event Event) error {
	body := completionText(event)
	deliveryID := event.OperationID
	if deliveryID == "" {
		deliveryID = event.ChildThreadRef + ":" + event.TurnID
	}
	refs := []interact.MessageRef{}
	if event.OperationID != "" {
		refs = append(refs, interact.MessageRef{Kind: "nano.operation", ID: event.OperationID})
	}
	var errs []error
	if choices.Inbox == Send {
		_, err := client.SendMessage(ctx, interact.SendMessageRequest{
			RecipientThreadRef: event.InvokerThreadRef,
			ClientMessageID:    "subscription:" + deliveryID,
			SenderThreadRef:    event.ChildThreadRef,
			Subject:            "Hosted thread turn finished",
			Body:               body,
			Delivery:           "inbox",
			Refs:               refs,
		})
		errs = append(errs, err)
	}
	if choices.Prompt == Send {
		_, err := client.SubmitPrompt(ctx, interact.SubmitPromptRequest{
			ThreadRef: event.InvokerThreadRef,
			Prompt:    body,
			ClientID:  "nano:subscription:" + deliveryID,
			Origin:    interact.SystemOrigin,
		})
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func completionText(event Event) string {
	result := fmt.Sprintf("GET /api/v1/hosted-threads/%s/completed-turns", event.ChildThreadRef)
	identity := "turnId: " + event.TurnID
	if event.OperationID != "" {
		identity = "operationId: " + event.OperationID
		result = "GET /api/v1/turn-operations/" + event.OperationID
	}
	return fmt.Sprintf("Nano subscription: a hosted thread you launched finished a turn.\nworker: %s\n%s\noutcome: %s\nFetch the result: %s\nThis is an automated delivery, not a user message.", event.WorkerDisplay, identity, event.Status, result)
}

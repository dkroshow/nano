package sessions

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestRestoredCodexMessagesKeepCanonicalTypes(t *testing.T) {
	for _, tc := range []struct {
		native string
		typed  model.ItemType
		want   string
	}{
		{"message", model.ItemUserMessage, "userMessage"},
		{"UserMessage", model.ItemUserMessage, "userMessage"},
		{"message", model.ItemAgentMessage, "agentMessage"},
		{"AgentMessage", model.ItemAgentMessage, "agentMessage"},
	} {
		t.Run(tc.native+"/"+string(tc.typed), func(t *testing.T) {
			item := parsedLiveItem(model.Item{ID: "saved-message", Type: tc.typed,
				Text: "earlier message", Category: model.CategoryMessages,
				Scope:  model.Scope{Provider: model.ProviderCodex},
				Detail: json.RawMessage(`{"type":"` + tc.native + `"}`)}, time.Now())
			if item.Type != tc.want || item.Item["type"] != tc.want {
				t.Fatalf("restored type = %s; want %s", item.Type, tc.want)
			}
		})
	}
}

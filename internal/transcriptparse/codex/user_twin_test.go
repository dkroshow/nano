package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func userTwinResponse(id, turn string) string {
	return fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","id":%q,"role":"user","content":[{"type":"input_text","text":"Again"}],"internal_chat_message_metadata_passthrough":{"turn_id":%q}}}`+"\n", id, turn)
}
func userTwinCompleted(id, turn string) string {
	return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"item_completed","turn_id":%q,"item":{"type":"UserMessage","id":%q,"content":[{"type":"text","text":"Again"}]}}}`+"\n", turn, id)
}

func TestCodexCompletedUserSerializationTwins(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			first, second := userTwinResponse("response-1", "turn-1"), userTwinCompleted("completed-1", "turn-1")
			if reverse {
				first, second = second, first
			}
			view, _, err := parseFileView(parsedView{}, inputFor([]byte(first)))
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, _ := json.Marshal(view)
			var restored parsedView
			if err := json.Unmarshal(checkpoint, &restored); err != nil {
				t.Fatal(err)
			}
			view, _, err = parseFileView(restored, inputFor([]byte(second+userTwinResponse("response-2", "turn-2")+userTwinCompleted("completed-2", "turn-2"))))
			if err != nil {
				t.Fatal(err)
			}
			items := orderedItems(view)
			if len(items) != 2 {
				t.Fatalf("got %d messages; want two real prompts", len(items))
			}
			for _, item := range items {
				if len(item.Observations) != 2 || len(item.Relations) != 1 || item.Relations[0].Kind != model.RelationSerializationTwin {
					t.Fatal("both native records must survive as twin evidence")
				}
				if len(item.Aliases) != 2 {
					t.Fatalf("native aliases = %v", item.Aliases)
				}
			}
		})
	}
}

func TestCodexCompletedUserAmbiguityAndRepeats(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      int
	}{
		{"same representation", userTwinResponse("a", "t") + userTwinResponse("b", "t"), 2},
		{"ambiguous", userTwinResponse("a", "t") + userTwinResponse("b", "t") + userTwinCompleted("c", "t"), 3},
		{"two pairs same turn", userTwinResponse("a", "t") + userTwinCompleted("b", "t") + userTwinResponse("c", "t") + userTwinCompleted("d", "t"), 2},
		{"different attachment evidence", strings.Replace(userTwinResponse("a", "t"), `"content":[`, `"content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"},`, 1) + userTwinCompleted("b", "t"), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, _, err := parseFileView(parsedView{}, inputFor([]byte(tc.raw)))
			if err != nil {
				t.Fatal(err)
			}
			if got := len(orderedItems(view)); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}

func TestCodexOperatorUserTwins(t *testing.T) {
	path := os.Getenv("NANO_CODEX_TWIN_TRANSCRIPT")
	if path == "" {
		t.Skip("opt-in read-only operator transcript")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	view, _, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	users, paired, selected := 0, 0, 0
	for _, item := range orderedItems(view) {
		if item.Type != model.ItemUserMessage {
			continue
		}
		users++
		if len(item.Relations) > 0 {
			paired++
		}
		if item.Text == "spinning for about 5 minutes now" {
			selected++
		}
	}
	t.Logf("user messages=%d paired=%d selected prompt=%d", users, paired, selected)
	if selected != 1 {
		t.Fatalf("affected prompt appears %d times", selected)
	}
}

// A prompt steered into a running turn shares the turn with the prompt that
// started it, so only its clientUserMessageId tells their saved rows apart.
// Codex saves it on either copy form it writes for a prompt: the completed
// UserMessage item (item.client_id) and, in legacy mode, the user_message
// event (payload.client_id). It must become that row's native event id, never
// replace a row's own event_id, and leave rows saved before any client id
// (an older checkpoint) as they were.
func TestCodexPromptClientIDsBecomeTheirRowsEventIDs(t *testing.T) {
	response := func(id, turn, text string) string {
		return fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","id":%q,"role":"user","content":[{"type":"input_text","text":%q}],"internal_chat_message_metadata_passthrough":{"turn_id":%q}}}`+"\n", id, text, turn)
	}
	completed := func(id, turn, text, extra string) string {
		return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"item_completed","turn_id":%q%s,"item":{"type":"UserMessage","id":%q,"client_id":"client-%s","content":[{"type":"text","text":%q}]}}}`+"\n", turn, extra, id, id, text)
	}
	events := func(item model.Item) []string {
		var ids []string
		for _, obs := range item.Observations {
			if obs.NativeEventID != "" {
				ids = append(ids, obs.NativeEventID)
			}
		}
		return ids
	}

	// Saved before any client id, then checkpointed.
	view, _, err := parseFileView(parsedView{}, inputFor([]byte(userTwinResponse("response-old", "turn-old")+userTwinCompleted("completed-old", "turn-old"))))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ := json.Marshal(view)
	var restored parsedView
	if err := json.Unmarshal(checkpoint, &restored); err != nil {
		t.Fatal(err)
	}
	// A starter and a steered correction in one turn, then a row with its own event_id.
	view, _, err = parseFileView(restored, inputFor([]byte(
		response("r-start", "turn-1", "Start the task.")+completed("start", "turn-1", "Start the task.", "")+
			response("r-fix", "turn-1", "Correction.")+completed("fix", "turn-1", "Correction.", "")+
			response("r-own", "turn-2", "Own event.")+completed("own", "turn-2", "Own event.", `,"event_id":"row-event"`))))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(view)
	got := make([][]string, len(items))
	for index, item := range items {
		got[index] = events(item)
	}
	if want := [][]string{nil, {"client-start"}, {"client-fix"}, {"row-event"}}; len(items) != 4 || !reflect.DeepEqual(got, want) {
		t.Fatalf("event ids = %v; want %v", got, want)
	}

	legacy, _, err := parseFileView(parsedView{}, inputFor([]byte(
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Legacy prompt"}]}}`+"\n"+
			`{"type":"event_msg","payload":{"type":"user_message","message":"Legacy prompt","client_id":"client-legacy"}}`+"\n")))
	if err != nil {
		t.Fatal(err)
	}
	if items := orderedItems(legacy); len(items) != 1 || !reflect.DeepEqual(events(items[0]), []string{"client-legacy"}) {
		t.Fatalf("legacy prompt items = %+v; want one with event client-legacy", items)
	}

	// The live item carries it as clientId.
	wire := wireState(t, []byte(`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"userMessage","id":"fix","clientId":"client-fix","content":[{"type":"text","text":"Correction."}]}}}`))
	if items := orderedItems(wire); len(items) != 1 || !reflect.DeepEqual(events(items[0]), []string{"client-fix"}) {
		t.Fatalf("live prompt items = %+v; want one with event client-fix", items)
	}
}

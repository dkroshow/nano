package codex

import (
	"encoding/json"
	"testing"
)

func object(t *testing.T, text string) map[string]json.RawMessage {
	t.Helper()
	var value map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestExtractedCommandDisplay(t *testing.T) {
	for _, raw := range []string{
		`{"name":"exec_command","arguments":{"command":["bash","-lc","echo hello"],"workdir":"/example"}}`,
		`{"name":"exec_command","arguments":"{\"cmd\":\"sh -lc 'echo hello'\",\"cwd\":\"/example\"}"}`,
	} {
		command, detail := codexCommandDisplay(object(t, raw))
		if command != "echo hello" || detail["workdir"] != "/example" {
			t.Fatalf("command %q detail %+v", command, detail)
		}
	}
}

func TestExtractedStructuredOutputAndSettings(t *testing.T) {
	row := object(t, `{"output":{"ok":true,"items":[1,2]}}`)
	if got := codexTextOrJSON(row, "output"); got != `{"ok":true,"items":[1,2]}` {
		t.Fatalf("output %q", got)
	}
	config := object(t, `{"collaboration_mode":{"settings":{"model":"model-example","reasoning_effort":"high","service_tier":"priority"}}}`)
	var fields map[string]string
	if err := json.Unmarshal(codexTurnConfig(config), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["model"] != "model-example" || fields["effort"] != "high" || fields["serviceTier"] != "priority" {
		t.Fatalf("config %+v", fields)
	}
}

func TestExtractedUserTextSeparatesHarnessBlocks(t *testing.T) {
	row := object(t, `{"content":[{"type":"input_text","text":"<recommended_plugins>example</recommended_plugins>"},{"type":"input_text","text":"# AGENTS.md instructions for /example"},{"type":"input_text","text":"<environment_context>example</environment_context>"},{"type":"input_text","text":"Read the example."}]}`)
	text, ok := codexResponseUserText(row)
	if !ok || text != "Read the example." {
		t.Fatalf("text=%q conversation=%v", text, ok)
	}
	if _, ok := codexResponseUserText(object(t, `{"content":"<environment_context>example</environment_context>"}`)); ok {
		t.Fatal("harness-only envelope became human input")
	}
}

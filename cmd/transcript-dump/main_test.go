package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const usageFixture = "../../internal/transcriptparse/codex/testdata/research/edge-usage-three-grains/input.jsonl"

func TestOptionsTerminatorPreservesDashPath(t *testing.T) {
	var stderr bytes.Buffer
	o, err := parseOptions([]string{"--provider", "claude", "--", "-capture.jsonl"}, &stderr)
	if err != nil || o.path != "-capture.jsonl" {
		t.Fatalf("path=%q err=%v", o.path, err)
	}
}

func TestDumpPathBeforeFlagsAndNativeUsage(t *testing.T) {
	var out, err bytes.Buffer
	code := run([]string{usageFixture, "--provider", "auto", "--source", "file"}, strings.NewReader(""), &out, &err)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, err.String())
	}
	var dump struct {
		Provider string `json:"provider"`
		Turns    []struct {
			Usage struct {
				Input *int64 `json:"input"`
			} `json:"usage"`
		} `json:"turns"`
	}
	if failure := json.Unmarshal(out.Bytes(), &dump); failure != nil {
		t.Fatal(failure)
	}
	if dump.Provider != "codex" || len(dump.Turns) != 1 || dump.Turns[0].Usage.Input == nil || *dump.Turns[0].Usage.Input != 10 {
		t.Fatalf("wrong native summary: %s", out.String())
	}
}
func TestDumpStrictReportsMalformedSourceWithoutEcho(t *testing.T) {
	var out, err bytes.Buffer
	code := run([]string{"-", "--provider", "claude", "--strict"}, strings.NewReader("private-invalid-row\n"), &out, &err)
	if code != 1 || !json.Valid(out.Bytes()) || !strings.Contains(err.String(), "malformed_record") || strings.Contains(err.String(), "private-invalid-row") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, out.String(), err.String())
	}
}
func TestDumpAmbiguousProviderFails(t *testing.T) {
	var out, err bytes.Buffer
	code := run([]string{"-"}, strings.NewReader(`{"id":1,"result":{}}`), &out, &err)
	if code != 2 || out.Len() != 0 || !strings.Contains(err.String(), "--provider") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, out.String(), err.String())
	}
}

func TestDumpDetectsClaudeSpoolAndShimFrames(t *testing.T) {
	path := "../../internal/transcriptparse/claude/testdata/research/shim-envelope-replay-exit/input.jsonl"
	var out, stderr bytes.Buffer
	if code := run([]string{path, "--strict"}, strings.NewReader(""), &out, &stderr); code != 0 {
		t.Fatalf("spool exit=%d stderr=%s", code, stderr.String())
	}
	var dump struct {
		Provider string            `json:"provider"`
		Items    []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(out.Bytes(), &dump); err != nil {
		t.Fatal(err)
	}
	if dump.Provider != "claude" || len(dump.Items) != 1 {
		t.Fatal("spool detection lost native content")
	}
	provider, err := detectProvider([]byte(`{"jsonrpc":"2.0","method":"session/line","params":{"shimSessionId":"s","seq":1,"dir":"recv","line":"{\"type\":\"result\",\"subtype\":\"success\"}"}}`))
	if err != nil || provider != "claude" {
		t.Fatal("Claude shim notification misidentified as Grok")
	}
}
func TestDumpWireJSONLContainsTypedChanges(t *testing.T) {
	var out, err bytes.Buffer
	input := `{"method":"item/completed","params":{"threadId":"th","turnId":"t","item":{"type":"agentMessage","id":"a","text":"Done","phase":"final_answer"}}}` + "\n"
	code := run([]string{"-", "--source", "wire", "--format", "jsonl"}, strings.NewReader(input), &out, &err)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, err.String())
	}
	found := false
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte{'\n'}) {
		var record struct {
			Type   string `json:"type"`
			Change struct {
				Item struct {
					Text string `json:"text"`
				} `json:"item"`
			} `json:"change"`
		}
		if failure := json.Unmarshal(line, &record); failure != nil {
			t.Fatal(failure)
		}
		if record.Type == "change" && record.Change.Item.Text == "Done" {
			found = true
		}
	}
	if !found {
		t.Fatal("wire typed changes absent")
	}
}
func TestDumpGrokCompanionSummaryDoesNotRestampTurn(t *testing.T) {
	var out, err bytes.Buffer
	path := "../../internal/transcriptparse/grok/testdata/research/paired-segments-tools-late-prompt/updates.jsonl"
	summary := "../../internal/transcriptparse/grok/testdata/research/capture-summary/input.json"
	code := run([]string{path, "--provider", "grok", "--summary", summary}, strings.NewReader(""), &out, &err)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, err.String())
	}
	var dump struct {
		Turns        []map[string]json.RawMessage `json:"turns"`
		Observations []map[string]json.RawMessage `json:"observations"`
	}
	if failure := json.Unmarshal(out.Bytes(), &dump); failure != nil {
		t.Fatal(failure)
	}
	if len(dump.Turns) != 1 || len(dump.Observations) == 0 {
		t.Fatal("companion metadata or transcript missing")
	}
	var modelName string
	_ = json.Unmarshal(dump.Turns[0]["model"], &modelName)
	if modelName != "" {
		t.Fatal("summary restamped an older turn model")
	}
}

func TestDumpRawFlagAndStdinWireLimits(t *testing.T) {
	native := `{"type":"assistant","uuid":"u","message":{"id":"m","content":[{"type":"tool_use","id":"c","name":"Read","input":{"path":"/example"}}]}}` + "\n"
	for _, raw := range []bool{false, true} {
		args := []string{"-", "--provider", "claude"}
		if raw {
			args = append(args, "--raw")
		}
		var out, err bytes.Buffer
		if code := run(args, strings.NewReader(native), &out, &err); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, err.String())
		}
		var dump struct {
			Items []struct {
				Parts []struct {
					Detail json.RawMessage `json:"detail"`
				} `json:"parts"`
			} `json:"items"`
		}
		if failure := json.Unmarshal(out.Bytes(), &dump); failure != nil {
			t.Fatal(failure)
		}
		if len(dump.Items) != 1 || len(dump.Items[0].Parts) != 1 || (len(dump.Items[0].Parts[0].Detail) > 0) != raw {
			t.Fatal("raw flag did not control retained native part detail")
		}
	}
	var out, err bytes.Buffer
	input := strings.Repeat("x", 101) + "\n" + `{"type":"result","subtype":"success"}` + "\n"
	code := run([]string{"-", "--provider", "claude", "--source", "wire", "--max-row-bytes", "100", "--strict"}, strings.NewReader(input), &out, &err)
	var dump struct {
		Records []struct {
			Source struct {
				End int64 `json:"end"`
			} `json:"source"`
			Disposition string `json:"disposition"`
		} `json:"records"`
	}
	if failure := json.Unmarshal(out.Bytes(), &dump); failure != nil {
		t.Fatal(failure)
	}
	if code != 1 || len(dump.Records) != 2 || dump.Records[0].Source.End != 101 || dump.Records[0].Disposition != "malformed" || dump.Records[1].Disposition != "semantic" {
		t.Fatalf("oversized frame did not drain/recover: exit=%d records=%+v", code, dump.Records)
	}
}

func TestDumpRawFlagCoversModelUsageBuckets(t *testing.T) {
	path := "../../internal/transcriptparse/claude/testdata/research/wire-result-usage-grain/input.jsonl"
	for _, raw := range []bool{false, true} {
		args := []string{path, "--provider", "claude", "--source", "wire"}
		if raw {
			args = append(args, "--raw")
		}
		var out, stderr bytes.Buffer
		if code := run(args, strings.NewReader(""), &out, &stderr); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, stderr.String())
		}
		var dump struct {
			Turns []struct {
				UsageObservations []struct {
					ModelUsage map[string]struct {
						Native json.RawMessage `json:"native"`
					} `json:"modelUsage"`
				} `json:"usageObservations"`
			} `json:"turns"`
		}
		if err := json.Unmarshal(out.Bytes(), &dump); err != nil {
			t.Fatal(err)
		}
		if len(dump.Turns) != 1 || len(dump.Turns[0].UsageObservations) != 1 {
			t.Fatal("result usage absent")
		}
		bucket := dump.Turns[0].UsageObservations[0].ModelUsage["claude-example"]
		if (len(bucket.Native) > 0) != raw {
			t.Fatal("raw flag did not control model-bucket detail")
		}
	}
}

func TestDumpMissingNativeInputReportsUnavailable(t *testing.T) {
	var out, stderr bytes.Buffer
	code := run([]string{filepath.Join(t.TempDir(), "missing.jsonl"), "--provider", "claude"}, strings.NewReader(""), &out, &stderr)
	if code != 2 || out.Len() != 0 || !strings.Contains(stderr.String(), "evidence_unavailable") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
}

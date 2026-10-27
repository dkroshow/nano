package heartbeat

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	PrePingWake                 = "wake"
	PrePingSkip                 = "skip"
	PrePingFail                 = "fail"
	heartbeatPrePingTimeout     = 5 * time.Second
	heartbeatPrePingOutputLimit = 32 << 10
	heartbeatPrePingDedupeLimit = 4 << 10
	InboxTemplateID             = "inbox_match"
)

type PrePingInput struct {
	Version                int             `json:"version"`
	DueEpochMinute         int64           `json:"dueEpochMinute"`
	TargetHostedThreadID   string          `json:"targetHostedThreadId"`
	Prompt                 string          `json:"prompt"`
	Settings               json.RawMessage `json:"settings"`
	LastDeliveredDedupeKey string          `json:"lastDeliveredDedupeKey,omitempty"`
}

type PrePingResult struct {
	Decision  string `json:"decision"`
	Prompt    string `json:"prompt,omitempty"`
	Reason    string `json:"reason,omitempty"`
	DedupeKey string `json:"dedupeKey,omitempty"`
}

type PrePingAudit struct {
	ScriptID       string
	Revision       int64
	Result         string
	Reason         string
	DurationMS     int64
	PromptChanged  bool
	FailureLatched bool
	DedupeKey      string
}

type PrePingRunner func(context.Context, ScriptRevision, PrePingInput) (PrePingResult, PrePingAudit, error)

var heartbeatPrePingReasonPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,119}$`)

type cappedBuffer struct {
	bytes.Buffer
	limit int
	over  bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Buffer.Len()+len(p) > b.limit {
		remaining := b.limit - b.Buffer.Len()
		if remaining > 0 {
			_, _ = b.Buffer.Write(p[:remaining])
		}
		b.over = true
		return len(p), errors.New("processor output limit exceeded")
	}
	return b.Buffer.Write(p)
}

func NewPrePingRunner(nodeBinary, apiOrigin string) PrePingRunner {
	if strings.TrimSpace(nodeBinary) == "" {
		nodeBinary = "node"
	}
	if strings.TrimSpace(apiOrigin) == "" {
		apiOrigin = "http://127.0.0.1:7690"
	}
	return func(parent context.Context, revision ScriptRevision, input PrePingInput) (PrePingResult, PrePingAudit, error) {
		audit := PrePingAudit{ScriptID: revision.ScriptID, Revision: revision.Revision}
		started := time.Now()
		finish := func(result, reason string, err error) (PrePingResult, PrePingAudit, error) {
			audit.Result, audit.Reason, audit.DurationMS = result, reason, time.Since(started).Milliseconds()
			return PrePingResult{}, audit, err
		}
		encoded, err := json.Marshal(input)
		if err != nil {
			return finish(PrePingFail, "input_encoding", err)
		}
		ctx, cancel := context.WithTimeout(parent, heartbeatPrePingTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, nodeBinary, "-e", revision.Source)
		cmd.Stdin = bytes.NewReader(encoded)
		cmd.Env = []string{
			"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
			"LANG=C.UTF-8",
			"NANO_API_ORIGIN=" + apiOrigin,
		}
		var stdout, stderr cappedBuffer
		stdout.limit, stderr.limit = heartbeatPrePingOutputLimit, heartbeatPrePingOutputLimit
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err = cmd.Run()
		if ctx.Err() == context.DeadlineExceeded {
			return finish(PrePingFail, "timeout", errors.New("pre-ping script timed out"))
		}
		if stdout.over || stderr.over {
			return finish(PrePingFail, "output_too_large", errors.New("pre-ping script output exceeded the limit"))
		}
		if err != nil {
			return finish(PrePingFail, "script_exit", errors.New("pre-ping script exited unsuccessfully"))
		}
		decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
		decoder.DisallowUnknownFields()
		var result PrePingResult
		if err := decoder.Decode(&result); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
			return finish(PrePingFail, "invalid_output", errors.New("pre-ping script returned invalid output"))
		}
		result.Decision, result.Reason = strings.TrimSpace(result.Decision), strings.TrimSpace(result.Reason)
		result.DedupeKey = strings.TrimSpace(result.DedupeKey)
		if len(result.DedupeKey) > heartbeatPrePingDedupeLimit {
			return finish(PrePingFail, "dedupe_key_too_large", errors.New("pre-ping script returned an oversized dedupe key"))
		}
		if result.Reason != "" && !heartbeatPrePingReasonPattern.MatchString(result.Reason) {
			return finish(PrePingFail, "invalid_reason", errors.New("pre-ping script returned an invalid reason"))
		}
		switch result.Decision {
		case PrePingWake:
			if len(result.Prompt) > 64<<10 {
				return finish(PrePingFail, "prompt_too_large", errors.New("pre-ping script returned an oversized prompt"))
			}
			if result.Prompt == "" {
				result.Prompt = input.Prompt
			}
			audit.Result, audit.Reason = PrePingWake, result.Reason
			audit.PromptChanged = result.Prompt != input.Prompt
			audit.DedupeKey = result.DedupeKey
		case PrePingSkip:
			if result.Prompt != "" || result.DedupeKey != "" {
				return finish(PrePingFail, "invalid_skip_output", errors.New("skip must not return a prompt or dedupe key"))
			}
			audit.Result, audit.Reason = PrePingSkip, result.Reason
		case PrePingFail:
			if result.Prompt != "" || result.DedupeKey != "" {
				return finish(PrePingFail, "invalid_fail_output", errors.New("fail must not return a prompt or dedupe key"))
			}
			if result.Reason == "" {
				result.Reason = "script_reported_failure"
			}
			audit.Result, audit.Reason = PrePingFail, result.Reason
		default:
			return finish(PrePingFail, "unknown_decision", errors.New("pre-ping script returned an unknown decision"))
		}
		audit.DurationMS = time.Since(started).Milliseconds()
		return result, audit, nil
	}
}

type ScriptTemplate struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	Language        string          `json:"language"`
	DefaultSettings json.RawMessage `json:"defaultSettings"`
	DefaultPrompt   string          `json:"defaultPrompt"`
	Revision        string          `json:"revision"`
	Source          string          `json:"-"`
}

//go:embed heartbeat_templates/*/manifest.json heartbeat_templates/*/processor.js
var heartbeatTemplateFiles embed.FS

func LoadScriptTemplates() ([]ScriptTemplate, error) {
	dirs, err := heartbeatTemplateFiles.ReadDir("heartbeat_templates")
	if err != nil {
		return nil, err
	}
	items := make([]ScriptTemplate, 0, len(dirs))
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		base := "heartbeat_templates/" + dir.Name()
		manifest, err := heartbeatTemplateFiles.ReadFile(base + "/manifest.json")
		if err != nil {
			return nil, err
		}
		var value ScriptTemplate
		decoder := json.NewDecoder(bytes.NewReader(manifest))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&value); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
			return nil, fmt.Errorf("invalid heartbeat template %s manifest", dir.Name())
		}
		source, err := heartbeatTemplateFiles.ReadFile(base + "/processor.js")
		if err != nil {
			return nil, err
		}
		value.Source = string(source)
		value.ID = strings.TrimSpace(value.ID)
		if value.ID == "" || value.ID != dir.Name() || value.Language != "javascript" {
			return nil, fmt.Errorf("invalid heartbeat template %s identity", dir.Name())
		}
		settings, err := NormalizeJSONObject(value.DefaultSettings, 16<<10)
		if err != nil {
			return nil, fmt.Errorf("invalid heartbeat template %s settings: %w", dir.Name(), err)
		}
		value.DefaultSettings = settings
		value.Revision = SourceSHA256(string(manifest) + "\x00" + value.Source)
		items = append(items, value)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func ScriptTemplateByID(id string) (ScriptTemplate, error) {
	items, err := LoadScriptTemplates()
	if err != nil {
		return ScriptTemplate{}, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return ScriptTemplate{}, errors.New("heartbeat script template not found")
}

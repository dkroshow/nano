package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/claude"
	"github.com/dkroshow/nano/internal/transcriptparse/codex"
	"github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

const commandVersion = "transcript-dump-v1"
const detectionBytes = 256 << 10

type options struct {
	provider, source, format, summary, lineage, path string
	raw, strict, version                             bool
	limit                                            int
}

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
func parseOptions(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("transcript-dump", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.provider, "provider", "auto", "auto|codex|claude|grok")
	fs.StringVar(&o.source, "source", "file", "file|wire")
	fs.StringVar(&o.format, "format", "json", "json|jsonl")
	fs.StringVar(&o.summary, "summary", "", "Grok summary JSON input")
	fs.StringVar(&o.lineage, "lineage", "", "Grok child metadata JSON input")
	fs.BoolVar(&o.raw, "raw", false, "include retained native item/part detail")
	fs.BoolVar(&o.strict, "strict", false, "exit 1 when a diagnostic is emitted")
	fs.BoolVar(&o.version, "version", false, "print command version")
	fs.IntVar(&o.limit, "max-row-bytes", 32<<20, "maximum native record or document bytes")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: transcript-dump PATH|- [--provider auto|codex|claude|grok] [options]")
		fs.PrintDefaults()
	}
	// The documented command places PATH first. flag stops at the first positional
	// argument, so move declared flag/value pairs ahead of positional arguments.
	var flags, paths []string
	takesValue := map[string]bool{"provider": true, "source": true, "format": true, "summary": true, "lineage": true, "max-row-bytes": true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			paths = append(paths, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			flags = append(flags, arg)
			name := strings.TrimLeft(arg, "-")
			if !strings.Contains(name, "=") && takesValue[name] {
				i++
				if i >= len(args) {
					return o, fmt.Errorf("missing value for %s", arg)
				}
				flags = append(flags, args[i])
			}
		} else {
			paths = append(paths, arg)
		}
	}
	if err := fs.Parse(append(append(flags, "--"), paths...)); err != nil {
		return o, err
	}
	if o.version {
		return o, nil
	}
	if fs.NArg() != 1 {
		return o, errors.New("one PATH or - is required")
	}
	o.path = fs.Arg(0)
	if o.source != "file" && o.source != "wire" {
		return o, errors.New("--source must be file or wire")
	}
	if o.format != "json" && o.format != "jsonl" {
		return o, errors.New("--format must be json or jsonl")
	}
	if o.provider != "auto" && o.provider != "codex" && o.provider != "claude" && o.provider != "grok" {
		return o, errors.New("--provider must be auto, codex, claude or grok")
	}
	if o.limit <= 0 || o.limit == int(^uint(0)>>1) {
		return o, errors.New("--max-row-bytes must be positive and leave room for an overflow sentinel")
	}
	return o, nil
}
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	o, err := parseOptions(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if o.version {
		fmt.Fprintln(stdout, commandVersion)
		return 0
	}
	input := stdin
	if o.path != "-" {
		file, err := os.Open(o.path)
		if err != nil {
			fmt.Fprintln(stderr, "evidence_unavailable:", err)
			return 2
		}
		defer file.Close()
		input = file
	}
	reader := bufio.NewReaderSize(input, detectionBytes)
	prefix, peekErr := reader.Peek(detectionBytes)
	if peekErr != nil && !errors.Is(peekErr, io.EOF) && !errors.Is(peekErr, bufio.ErrBufferFull) {
		fmt.Fprintln(stderr, peekErr)
		return 2
	}
	if o.provider == "auto" {
		o.provider, err = detectProvider(prefix)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	if (o.summary != "" || o.lineage != "") && o.provider != "grok" {
		fmt.Fprintln(stderr, "--summary and --lineage require --provider grok")
		return 2
	}
	sink := newCollector(o, stdout, stderr)
	scope := model.Scope{Provider: model.Provider(o.provider)}
	for _, path := range []string{o.summary, o.lineage} {
		if path == "" {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			fmt.Fprintln(stderr, "evidence_unavailable:", err)
			return 2
		}
		companion := model.Input{Source: model.SourceRef{Label: path, Incarnation: "dump-1"}, Format: "json", MaxRowBytes: o.limit}
		parse := newParser("grok", "file")
		err = feedFile(file, companion, parse, sink.add)
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	label := o.path
	if label == "-" {
		label = "stdin"
	}
	nativeFormat := "jsonl"
	if o.provider == "grok" && o.source == "file" && wholeDocument(prefix, o.path) {
		nativeFormat = "json"
	}
	frame := model.Input{Source: model.SourceRef{Label: label, Incarnation: "dump-1"}, Scope: scope, Format: nativeFormat, MaxRowBytes: o.limit}
	parse := newParser(o.provider, o.source)
	if o.source == "wire" {
		err = feedWire(reader, frame, parse, sink.add)
	} else {
		err = feedFile(reader, frame, parse, sink.add)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if o.provider == "grok" {
		for _, d := range grok.CheckLineage(sink.observations) {
			if err = sink.add(model.Batch{Diagnostics: []model.Diagnostic{d}}); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
		}
	}
	if err = sink.finish(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if o.strict && len(sink.diagnostics) > 0 {
		return 1
	}
	return 0
}

type parseFunc func(model.Input) (model.Batch, error)

func newParser(provider, source string) parseFunc {
	switch provider {
	case "codex":
		var state codex.State
		return func(input model.Input) (batch model.Batch, err error) {
			if source == "wire" {
				state, batch, err = codex.ParseWire(state, input)
			} else {
				state, batch, err = codex.ParseFile(state, input)
			}
			return
		}
	case "claude":
		var state claude.State
		return func(input model.Input) (batch model.Batch, err error) {
			if source == "wire" {
				state, batch, err = claude.ParseWire(state, input)
			} else {
				state, batch, err = claude.ParseFile(state, input)
			}
			return
		}
	default:
		var state grok.State
		return func(input model.Input) (batch model.Batch, err error) {
			if source == "wire" {
				state, batch, err = grok.ParseWire(state, input)
			} else {
				state, batch, err = grok.ParseFile(state, input)
			}
			return
		}
	}
}
func feedFile(reader io.Reader, input model.Input, parse parseFunc, accept func(model.Batch) error) error {
	buffer := make([]byte, 64<<10)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			input.Bytes = buffer[:n]
			batch, failure := parse(input)
			if failure != nil {
				return failure
			}
			if failure = accept(batch); failure != nil {
				return failure
			}
		}
		if errors.Is(err, io.EOF) {
			input.Bytes = nil
			input.Closed = true
			batch, failure := parse(input)
			if failure != nil {
				return failure
			}
			return accept(batch)
		}
		if err != nil {
			return err
		}
	}
}
func feedWire(reader io.Reader, input model.Input, parse parseFunc, accept func(model.Batch) error) error {
	buffered := bufio.NewReaderSize(reader, 64<<10)
	var row []byte
	var total int64
	var lastByte byte
	for {
		part, err := buffered.ReadSlice('\n')
		newline := len(part) > 0 && part[len(part)-1] == '\n'
		carriageReturn := newline && ((len(part) >= 2 && part[len(part)-2] == '\r') || (len(part) == 1 && lastByte == '\r'))
		if len(part) > 0 {
			lastByte = part[len(part)-1]
		}
		total += int64(len(part))
		room := input.MaxRowBytes + 1 - len(row)
		if room > len(part) {
			room = len(part)
		}
		if room > 0 {
			row = append(row, part[:room]...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if total > 0 {
			frameBytes := total
			input.Bytes = row
			if newline {
				frameBytes--
				input.Bytes = bytes.TrimSuffix(input.Bytes, []byte{'\n'})
				if carriageReturn {
					frameBytes--
					input.Bytes = bytes.TrimSuffix(input.Bytes, []byte{'\r'})
				}
			}
			// The decoder receives a bounded over-limit sentinel. Preserve the actual
			// frame length in its oversized-record evidence after draining the line.
			if frameBytes > int64(input.MaxRowBytes) {
				input.Bytes = make([]byte, input.MaxRowBytes+1)
			}
			batch, failure := parse(input)
			if failure != nil {
				return failure
			}
			if frameBytes > int64(input.MaxRowBytes) {
				for i := range batch.Records {
					batch.Records[i].Source.End = frameBytes
				}
				for i := range batch.Diagnostics {
					batch.Diagnostics[i].Source.End = frameBytes
				}
			}
			if failure = accept(batch); failure != nil {
				return failure
			}
		}
		row, total, lastByte = nil, 0, 0
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
func detectProvider(prefix []byte) (string, error) {
	seen := map[string]bool{}
	inspect := func(raw []byte) {
		var row map[string]json.RawMessage
		if json.Unmarshal(raw, &row) != nil {
			return
		}
		if inner := objectFields(row["msg"]); inner != nil {
			row = inner
		}
		method := stringField(row, "method")
		switch {
		case method == "session/line" || method == "session/replayDone" || method == "session/exited":
			seen["claude"] = true
		case row["type"] == nil && row["line"] != nil && (row["seq"] != nil || row["dir"] != nil):
			seen["claude"] = true
		case strings.HasPrefix(method, "session/") || strings.HasPrefix(method, "_x.ai/"):
			seen["grok"] = true
		case strings.HasPrefix(method, "item/") || strings.HasPrefix(method, "turn/") || strings.HasPrefix(method, "thread/") || strings.HasPrefix(method, "codex/"):
			seen["codex"] = true
		}
		switch stringField(row, "type") {
		case "session_meta", "turn_context", "response_item", "event_msg", "compacted", "token_usage_record":
			seen["codex"] = true
		case "stream_event":
			seen["claude"] = true
		case "assistant", "user", "system", "result":
			if row["message"] != nil || row["session_id"] != nil || row["sessionId"] != nil {
				seen["claude"] = true
			}
		}
		if row["info"] != nil || row["parent_session_id"] != nil || row["current_model_id"] != nil {
			seen["grok"] = true
		}
	}
	inspect(prefix)
	for _, line := range bytes.Split(prefix, []byte{'\n'}) {
		inspect(line)
	}
	if len(seen) != 1 {
		return "", errors.New("provider detection is ambiguous; specify --provider codex|claude|grok")
	}
	for provider := range seen {
		return provider, nil
	}
	return "", nil
}
func wholeDocument(prefix []byte, path string) bool {
	if strings.EqualFold(filepath.Ext(path), ".json") {
		return true
	}
	trimmed := bytes.TrimSpace(prefix)
	first := bytes.TrimSpace(bytes.SplitN(trimmed, []byte{'\n'}, 2)[0])
	if bytes.Equal(first, []byte{'{'}) || len(trimmed) > 0 && trimmed[0] == '[' {
		return true
	}
	var row map[string]json.RawMessage
	if json.Unmarshal(trimmed, &row) == nil && row["method"] == nil && row["jsonrpc"] == nil {
		return true
	}
	return false
}
func objectFields(raw json.RawMessage) map[string]json.RawMessage {
	var value map[string]json.RawMessage
	_ = json.Unmarshal(raw, &value)
	return value
}
func stringField(row map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(row[key], &value)
	return value
}

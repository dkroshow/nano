package sessions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/wire"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// errClaudeRecoveryUnproven: the durable facts or the spool cannot prove where
// a replay may start. The child may still be running, so this never falls back
// to a replacement process (resumeClaudeSession).
var errClaudeRecoveryUnproven = errors.New("claude recovery evidence is missing or unprovable")

// claudeReplayPlan is what the pre-attach scan proved.
type claudeReplayPlan struct {
	// FromSeq is the replay floor: the shim sends every frame after it.
	FromSeq int64
	// Observed is the highest complete sequence the scan read. The shim's
	// attach lastSeq can only be at or above it for the same spool.
	Observed int64
	// Missing means no spool file exists. Only a shim session that has
	// recorded nothing at all (attach lastSeq 0) is then positively empty.
	Missing bool
	// Resolved is the real native id a provisional native's own prompts prove
	// in the spool (claudeSpoolFacts.resolvedNative); empty when unproven.
	Resolved string
	// SpoolBytes is the spool's size when planned: the shim reads all of it
	// before it answers an attach.
	SpoolBytes int64
	// Err is a planning failure. It is raised only once the shim confirms the
	// child exists: a child the shim no longer holds needs no replay, and its
	// `--resume` rung stays available.
	Err error
}

// claudeSpoolFrame is one spooled line, from the file or from the replay.
type claudeSpoolFrame struct {
	Seq    int64
	Dir    wire.Direction
	Line   []byte
	TurnID string
}

// claudeSpoolFacts follows one spool in sequence order. The pre-attach scan
// uses it to choose the floor; the replay uses a fresh one to decide which
// replayed permission requests are still open.
type claudeSpoolFacts struct {
	// turnPrefix is "claude:<spool key>:". Empty skips the ownership check.
	turnPrefix string
	wanted     map[int64]*claudeSpoolTurn
	last       int64
	nativeID   string
	// open holds message_start sequences with no message_stop or result yet.
	// A later message_start does not close an earlier one: an interleaved
	// message keeps the earlier start, and a result clears them all.
	open     []int64
	requests map[string]claudeSpoolRequest
	// prompts are the spooled Nano prompts in order.
	prompts []claudeSpoolPrompt
	// sendTags holds each anchor's literal turn tags; initNatives holds, per
	// literal turn tag, the session ids of the root system/init lines the shim
	// tagged with it. With prompts, every send line tagged or not, they tie a
	// prompt the provider accepted under a provisional handle to the native id
	// its own child announced.
	sendTags    map[string][]string
	initNatives map[string][]string
}

// claudeSpoolPrompt is one spooled Nano prompt: its send line, the anchor
// Nano stamped on it, and the text of its one text block (empty for any
// other shape).
type claudeSpoolPrompt struct {
	Seq    int64
	Anchor string
	Text   string
}

// claudeSpoolTurn tracks one accepted prompt that has no durable terminal
// record. Its frames run from its own send line to the next result.
type claudeSpoolTurn struct {
	sent, active, ambiguous bool
	lastStart               int64
	// answered: a result named this prompt's turn, or an untagged one came
	// after it (the shim gives a legacy result the turn of the prompt before
	// it). over: a result named a later prompt's turn first, so no replayed
	// result can finish this one.
	answered, over bool
}

type claudeSpoolRequest struct {
	floor  int64
	turnID string
}

type claudeSpoolProbe struct {
	Type            string `json:"type"`
	UUID            string `json:"uuid"`
	Subtype         string `json:"subtype"`
	SessionID       string `json:"session_id"`
	ParentToolUseID string `json:"parent_tool_use_id"`
	RequestID       string `json:"request_id"`
	Response        struct {
		RequestID string `json:"request_id"`
	} `json:"response"`
	Event struct {
		Type string `json:"type"`
	} `json:"event"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// claudeSpoolCommandsChanged reports a system commands_changed notice: the
// one system line that carries no replay fact. Init names the conversation,
// and other subtypes carry compaction or retraction.
func claudeSpoolCommandsChanged(line []byte) bool {
	var probe claudeSpoolProbe
	return json.Unmarshal(line, &probe) == nil && probe.Type == "system" && probe.Subtype == "commands_changed"
}

// newClaudeSpoolFacts follows a spool whose frames must continue exactly from
// after (0 for a whole spool file, the replay floor for a replay).
func newClaudeSpoolFacts(turnPrefix string, after int64, wanted []int64) *claudeSpoolFacts {
	facts := &claudeSpoolFacts{turnPrefix: turnPrefix, last: after, wanted: map[int64]*claudeSpoolTurn{}, requests: map[string]claudeSpoolRequest{},
		sendTags: map[string][]string{}, initNatives: map[string][]string{}}
	for _, seq := range wanted {
		facts.wanted[seq] = &claudeSpoolTurn{}
	}
	return facts
}

// observe applies one frame. It returns the permission requests that frame
// retired without an answer (a cancel, or the result of the request's own
// turn), so a replay can drop them too.
func (f *claudeSpoolFacts) observe(frame claudeSpoolFrame) ([]string, error) {
	ownTurn := frame.TurnID == "" || f.turnPrefix == "" || strings.HasPrefix(frame.TurnID, f.turnPrefix)
	if frame.Seq > 0 && frame.Seq < f.last && frame.Dir == protocol.Recv && ownTurn && claudeSpoolCommandsChanged(frame.Line) {
		// A stopped earlier child's last notice, numbered by its own lower
		// sequence: not part of the running sequence, and a commands_changed
		// notice carries no replay fact.
		log.Printf("WARNING: claude spool %s: skipped an earlier child's system notice, sequence %d below %d", strings.TrimSuffix(strings.TrimPrefix(f.turnPrefix, "claude:"), ":"), frame.Seq, f.last)
		return nil, nil
	}
	// A gap could hide an open message or request, so evidence must be
	// contiguous: the shim numbers every line it records.
	if frame.Seq != f.last+1 {
		return nil, fmt.Errorf("%w: spool sequence %d does not follow %d", errClaudeRecoveryUnproven, frame.Seq, f.last)
	}
	if frame.Dir != protocol.Send && frame.Dir != protocol.Recv {
		return nil, fmt.Errorf("%w: spool sequence %d has direction %q", errClaudeRecoveryUnproven, frame.Seq, frame.Dir)
	}
	if !ownTurn {
		return nil, fmt.Errorf("%w: spool sequence %d names turn %s, not this spool's", errClaudeRecoveryUnproven, frame.Seq, frame.TurnID)
	}
	f.last = frame.Seq
	if frame.Line == nil {
		return nil, nil // the file scan positively recognised a fact-free frame
	}
	var probe claudeSpoolProbe
	if json.Unmarshal(frame.Line, &probe) != nil {
		return nil, nil // provider noise carries no replay fact
	}
	if frame.Dir == protocol.Send {
		switch probe.Type {
		case claudeUserLineType:
			prompt := claudeSpoolPrompt{Seq: frame.Seq, Anchor: probe.UUID}
			if content := probe.Message.Content; len(content) == 1 && content[0].Type == "text" {
				prompt.Text = content[0].Text
			}
			f.prompts = append(f.prompts, prompt)
			if probe.UUID != "" && frame.TurnID != "" {
				f.sendTags[probe.UUID] = append(f.sendTags[probe.UUID], frame.TurnID)
			}
			for seq, turn := range f.wanted {
				if seq == frame.Seq {
					turn.sent, turn.active = true, true
				} else if turn.active {
					turn.ambiguous = true // another prompt before this one's result
				}
			}
		case claudeControlResponseType:
			delete(f.requests, probe.Response.RequestID)
		}
		return nil, nil
	}
	switch probe.Type {
	case claudeSystemLineType:
		if probe.Subtype == claudeInitSubtype && probe.SessionID != "" {
			f.nativeID = probe.SessionID
			if frame.TurnID != "" && probe.ParentToolUseID == "" && !slices.Contains(f.initNatives[frame.TurnID], probe.SessionID) {
				f.initNatives[frame.TurnID] = append(f.initNatives[frame.TurnID], probe.SessionID)
			}
		}
	case "stream_event":
		switch probe.Event.Type {
		case "message_start":
			f.open = append(f.open, frame.Seq)
			for _, turn := range f.wanted {
				if turn.active {
					turn.lastStart = frame.Seq
				}
			}
		case "message_stop":
			if len(f.open) > 0 {
				f.open = f.open[:len(f.open)-1]
			}
		}
	case claudeControlRequestType:
		if probe.RequestID != "" {
			floor := frame.Seq - 1
			if len(f.open) > 0 {
				floor = f.open[0] - 1
			}
			f.requests[probe.RequestID] = claudeSpoolRequest{floor: floor, turnID: frame.TurnID}
		}
	case "control_cancel_request":
		if _, ok := f.requests[probe.RequestID]; ok {
			delete(f.requests, probe.RequestID)
			return []string{probe.RequestID}, nil
		}
	case claudeResultLineType:
		f.open = nil
		named, isPrompt := f.promptSeq(frame.TurnID)
		for seq, turn := range f.wanted {
			turn.active = false
			switch {
			case !turn.sent:
			case frame.TurnID == "":
				turn.answered = true
			case !isPrompt:
			case named == seq:
				turn.answered = true
			case named > seq && !turn.answered:
				turn.over = true
			}
		}
		var retired []string
		for id, request := range f.requests {
			if request.turnID == frame.TurnID {
				delete(f.requests, id)
				retired = append(retired, id)
			}
		}
		return retired, nil
	}
	return nil, nil
}

// resolvedNative is the one native id the given prompt anchors prove: an
// anchor on exactly one send line, tagged by the shim, whose tag is on root
// init lines naming exactly one session. An anchor with no send line, or one
// untagged legacy send line, proves nothing. Any competing evidence fails the
// whole proof: an anchor on two send lines, a tag on inits naming two
// sessions, or two anchors proving different natives. A reader-side tag is
// never the child's own.
func (f *claudeSpoolFacts) resolvedNative(anchors []string) (string, bool) {
	sends := map[string]int{}
	for _, prompt := range f.prompts {
		sends[prompt.Anchor]++
	}
	native := ""
	for _, anchor := range anchors {
		switch sends[anchor] {
		case 0:
			continue
		case 1:
		default:
			return "", false
		}
		tags := f.sendTags[anchor]
		if len(tags) == 0 {
			continue
		}
		natives := f.initNatives[tags[0]]
		switch {
		case len(natives) == 0:
			continue
		case len(natives) > 1:
			return "", false
		case native != "" && native != natives[0]:
			return "", false
		}
		native = natives[0]
	}
	return native, native != ""
}

// promptSeq is the spool sequence of the prompt a turn id names.
func (f *claudeSpoolFacts) promptSeq(turnID string) (int64, bool) {
	rest, ok := strings.CutPrefix(turnID, f.turnPrefix)
	if !ok || f.turnPrefix == "" {
		return 0, false
	}
	seq, err := strconv.ParseInt(rest, 10, 64)
	return seq, err == nil && seq > 0
}

// floor is the lowest sequence any recorded fact needs replayed.
func (f *claudeSpoolFacts) floor() (int64, error) {
	floor := f.last
	lower := func(seq int64) {
		if seq < floor {
			floor = seq
		}
	}
	for _, start := range f.open {
		lower(start - 1)
	}
	for _, request := range f.requests {
		lower(request.floor)
	}
	for seq, turn := range f.wanted {
		switch {
		case !turn.sent && seq <= f.last:
			return 0, fmt.Errorf("%w: accepted prompt sequence %d is not a prompt in the spool", errClaudeRecoveryUnproven, seq)
		case !turn.sent:
			// Sent after the scan's flushed tail: everything after it replays.
		case turn.over:
			// A result finishes only the turn it names, so replaying this
			// prompt settles nothing; holding the floor for it replayed every
			// frame since on each restart.
			log.Printf("WARNING: claude spool %s: accepted prompt %d ended without a result naming it; replay cannot settle it", strings.TrimSuffix(strings.TrimPrefix(f.turnPrefix, "claude:"), ":"), seq)
		case turn.ambiguous || turn.lastStart == 0:
			lower(seq - 1)
		default:
			lower(turn.lastStart - 1)
		}
	}
	return floor, nil
}

// scanClaudeSpool feeds every complete line of the spool file to facts. A
// trailing line without its newline is a write in progress; the shim replays
// it after the floor. It reports missing only for an absent file.
func scanClaudeSpool(path string, facts *claudeSpoolFacts) (bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: open spool: %v", errClaudeRecoveryUnproven, err)
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<20)
	for {
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			return false, nil // an unterminated tail is still being written
		}
		if err != nil {
			return false, fmt.Errorf("%w: read spool: %v", errClaudeRecoveryUnproven, err)
		}
		if frame, ok := claudeSpoolFactFreeFrame(bytes.TrimSpace(line)); ok {
			if _, err := facts.observe(frame); err != nil {
				return false, err
			}
			continue
		}
		var envelope struct {
			Seq    int64   `json:"seq"`
			Dir    string  `json:"dir"`
			Line   *string `json:"line"`
			TurnID string  `json:"turnId"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(line), &envelope); err != nil || envelope.Line == nil {
			return false, fmt.Errorf("%w: malformed spool line after sequence %d", errClaudeRecoveryUnproven, facts.last)
		}
		if _, err := facts.observe(claudeSpoolFrame{Seq: envelope.Seq, Dir: wire.Direction(envelope.Dir), Line: []byte(*envelope.Line), TurnID: envelope.TurnID}); err != nil {
			return false, err
		}
	}
}

// claudeSpoolFactFreeFrame borrows exact prefixes from the shim's compact
// envelope. The CLI uses JSON.stringify, so no later duplicate key overrides
// the prefix. Unrecognised envelopes and kinds use the original decoder.
func claudeSpoolFactFreeFrame(raw []byte) (frame claudeSpoolFrame, ok bool) {
	marker := []byte(`,"line":"`)
	head := bytes.Index(raw[:min(len(raw), 256)], marker)
	if head < 0 || json.Unmarshal(append(bytes.Clone(raw[:head]), '}'), &frame) != nil || frame.Dir != protocol.Recv {
		return frame, false
	}
	body := raw[head+len(marker):]
	for _, prefix := range []string{
		`{\"type\":\"assistant\",`, `{\"type\":\"user\",`,
		`{\"type\":\"stream_event\",\"event\":{\"type\":\"content_block_delta\",`,
		`{\"type\":\"stream_event\",\"event\":{\"type\":\"content_block_start\",`,
		`{\"type\":\"stream_event\",\"event\":{\"type\":\"content_block_stop\",`,
		`{\"type\":\"stream_event\",\"event\":{\"type\":\"message_delta\",`,
	} {
		if bytes.HasPrefix(body, []byte(prefix)) {
			ok = true
			break
		}
	}
	if !ok || !bytes.HasSuffix(raw, []byte(`"}`)) {
		return frame, false
	}
	// The body is an object. Its unescaped closing quote separates it from
	// the envelope tail; a long or unfamiliar tail falls back, never loses a tag.
	tail := raw[max(head+len(marker), len(raw)-256):]
	end := bytes.LastIndex(tail, []byte(`}"`))
	if end < 0 {
		return frame, false
	}
	tail = tail[end+2:]
	if !bytes.Equal(tail, []byte(`}`)) && (!bytes.HasPrefix(tail, []byte(`,"turnId":`)) || json.Unmarshal(tail[len(`,"turnId":`):len(tail)-1], &frame.TurnID) != nil) {
		return frame, false
	}
	frame.Line = nil
	return frame, true
}

// planClaudeReplay chooses the replay floor for attaching to native before
// any frame is requested. Durable facts come from the prompt and turn stores;
// the spool is found by the key the shim mints turn ids under: the hosted
// thread id, or the shim session id for a child started before that existed.
func (providerRuntime *Runtime) planClaudeReplay(ctx context.Context, spoolDir, native string, stored hostedstore.Attachment) (claudeReplayPlan, error) {
	if strings.TrimSpace(spoolDir) == "" {
		return claudeReplayPlan{}, fmt.Errorf("%w: the claude shim spool directory is not configured", errClaudeRecoveryUnproven)
	}
	var key, path string
	for _, candidate := range []string{providerRuntime.identity.hostedThreadID, stored.ShimSessionID} {
		if candidate == "" || strings.ContainsAny(candidate, `/\`) {
			continue
		}
		if key == "" {
			key = candidate // the key a missing spool would have had
		}
		if _, err := os.Stat(filepath.Join(spoolDir, candidate+".jsonl")); err == nil {
			key = candidate
			break
		}
	}
	if key == "" {
		return claudeReplayPlan{}, fmt.Errorf("%w: no hosted or shim identity names a spool", errClaudeRecoveryUnproven)
	}
	path = filepath.Join(spoolDir, key+".jsonl")
	// The shim reads the whole spool before it answers an attach, so every
	// plan carries its size for the attach's timeout (claudeShimAttachTimeout).
	var spoolBytes int64
	if info, err := os.Stat(path); err == nil {
		spoolBytes = info.Size()
	}
	planned := func(plan claudeReplayPlan, err error) (claudeReplayPlan, error) {
		plan.SpoolBytes = spoolBytes
		return plan, err
	}
	prefix := "claude:" + key + ":"
	wanted, sent, err := providerRuntime.unfinishedClaudePrompts(ctx, prefix, native, stored.ShimSessionID)
	if err != nil {
		return planned(claudeReplayPlan{}, err)
	}
	facts := newClaudeSpoolFacts(prefix, 0, wanted)
	missing, err := scanClaudeSpool(path, facts)
	if err != nil {
		return planned(claudeReplayPlan{}, err)
	}
	if missing {
		if len(wanted) > 0 {
			return planned(claudeReplayPlan{}, fmt.Errorf("%w: %d accepted prompt(s) have no terminal record and spool %s is missing", errClaudeRecoveryUnproven, len(wanted), filepath.Base(path)))
		}
		return planned(claudeReplayPlan{Missing: true}, nil)
	}
	var resolved string
	if ClaudeNativeIDProvisional(native) {
		var anchors []string
		for anchor, prompt := range sent {
			if prompt.native == native {
				anchors = append(anchors, anchor)
			}
		}
		resolved, _ = facts.resolvedNative(anchors)
	}
	// A proven pair is reported whatever else the plan decides: frames under
	// the handle stay unadmitted until the claim owner records it, and a later
	// child's init does not erase it.
	if facts.nativeID != "" && facts.nativeID != native {
		if resolved != "" {
			return planned(claudeReplayPlan{Resolved: resolved}, fmt.Errorf("%w: spool %s proves provisional %s is %s, not yet claimed (latest init %s)", errClaudeRecoveryUnproven, filepath.Base(path), native, resolved, facts.nativeID))
		}
		return planned(claudeReplayPlan{}, fmt.Errorf("%w: spool %s belongs to conversation %s, not %s", errClaudeRecoveryUnproven, filepath.Base(path), facts.nativeID, native))
	}
	floor, err := facts.floor()
	if err != nil {
		return planned(claudeReplayPlan{Resolved: resolved}, err)
	}
	return planned(claudeReplayPlan{FromSeq: floor, Observed: facts.last, Resolved: resolved}, nil)
}

// claudeSentPrompt is the native thread one accepted prompt's record names.
type claudeSentPrompt struct {
	native string
}

// unfinishedClaudePrompts returns the prompt sequences of this conversation's
// accepted prompts whose provider turn has no durable terminal record. A
// conversation's first prompt is bound under its provisional shim handle, so
// both identities count. A legacy Nano-local ordinal ("claude:<n>") was never
// minted by a shim, so no spool frame can carry it and no replay can finish
// it. Any other id this spool's key did not mint cannot be located, so it
// fails rather than being skipped. sent maps each accepted prompt's anchor to
// the native thread its record names; an anchor two records share is left
// out.
func (providerRuntime *Runtime) unfinishedClaudePrompts(ctx context.Context, prefix string, natives ...string) (wanted []int64, sent map[string]claudeSentPrompt, err error) {
	if providerRuntime.delivery.PromptStore() == nil || providerRuntime.completedTurns == nil {
		return nil, nil, fmt.Errorf("%w: prompt and turn stores are unavailable", errClaudeRecoveryUnproven)
	}
	turnStore, err := providerRuntime.completedTurns(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: turn store: %v", errClaudeRecoveryUnproven, err)
	}
	records, err := providerRuntime.delivery.PromptStore().List(ctx, providerRuntime.identity.hostedThreadID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: prompt store: %v", errClaudeRecoveryUnproven, err)
	}
	sent = map[string]claudeSentPrompt{}
	shared := map[string]bool{}
	for _, record := range records {
		if record.NativeThreadID == "" || !slices.Contains(natives, record.NativeThreadID) || record.ProviderText == "" || record.Outbox.PresentationText == "" {
			continue
		}
		anchor := SubmissionEventID(providerRuntime.identity.hostedThreadID, record.Outbox.ActionID)
		if _, seen := sent[anchor]; seen || anchor == "" {
			shared[anchor] = true
		}
		sent[anchor] = claudeSentPrompt{native: record.NativeThreadID}
	}
	for anchor := range shared {
		delete(sent, anchor)
	}
	ended, err := turnStore.Ended(ctx, providerRuntime.identity.hostedThreadID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: turn lookup: %v", errClaudeRecoveryUnproven, err)
	}
	for _, record := range records {
		if record.ProviderTurnID == "" || record.NativeThreadID == "" || !slices.Contains(natives, record.NativeThreadID) {
			continue
		}
		if ordinal, ok := strings.CutPrefix(record.ProviderTurnID, "claude:"); ok && isDigits(ordinal) {
			continue // legacy Nano-local ordinal
		}
		if ended[record.ProviderTurnID] {
			continue
		}
		seq, err := strconv.ParseInt(strings.TrimPrefix(record.ProviderTurnID, prefix), 10, 64)
		if !strings.HasPrefix(record.ProviderTurnID, prefix) || err != nil || seq <= 0 {
			return nil, nil, fmt.Errorf("%w: accepted turn %s is not locatable in this spool", errClaudeRecoveryUnproven, record.ProviderTurnID)
		}
		wanted = append(wanted, seq)
	}
	return wanted, sent, nil
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

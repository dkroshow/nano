package history

import (
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var uuidAtEnd = regexp.MustCompile(`(?i)([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

type projectionState struct {
	projection         ThreadProjection
	source             SourceCandidate
	diagnostics        []Diagnostic
	summary            DiagnosticSummary
	identityConflicted bool
	// Newest timestamp on any row, message or not. Only used as the fallback
	// for a thread that never held a message; UpdatedAt otherwise tracks
	// conversation, not file activity.
	lastActivity *time.Time
}

func initialProjection(provider Provider, source SourceCandidate, previous *ThreadProjection) ThreadProjection {
	if previous != nil {
		projection := *previous
		projection.Provider = provider
		if projection.Kind == "" {
			projection.Kind = source.Kind
		}
		return projection
	}
	metadata := Metadata{
		CWD: UnknownField(), Model: UnknownField(), Effort: UnknownField(),
		Priority: UnknownField(), Permission: UnknownField(), HistoricalBranch: UnknownField(),
	}
	if provider == ProviderClaude {
		metadata.Priority = UnsupportedField()
	}
	kind := source.Kind
	if kind == "" {
		kind = ThreadMain
	}
	return ThreadProjection{Provider: provider, Kind: kind, Metadata: metadata}
}

func applyFilenameFallback(state *projectionState, source SourceCandidate) {
	if state.projection.NativeThreadID != "" && state.projection.IdentityEvidence == EvidenceNativeDurable {
		return
	}
	base := strings.TrimSuffix(filepathBase(source.Path), filepathExt(source.Path))
	fallback := ""
	if state.projection.Kind == ThreadSubagent && strings.HasPrefix(base, "agent-") {
		fallback = strings.TrimSpace(strings.TrimPrefix(base, "agent-"))
	} else if match := uuidAtEnd.FindStringSubmatch(base); len(match) == 2 {
		fallback = match[1]
	}
	if fallback != "" {
		state.projection.NativeThreadID = fallback
		state.projection.IdentityEvidence = EvidenceFilenameFallback
	}
}

// These tiny wrappers keep projection helpers platform-path aware without
// leaking path manipulation into provider adapters.
func filepathBase(path string) string {
	path = strings.ReplaceAll(path, `\`, "/")
	if index := strings.LastIndex(path, "/"); index >= 0 {
		return path[index+1:]
	}
	return path
}

func filepathExt(path string) string {
	base := filepathBase(path)
	if index := strings.LastIndex(base, "."); index >= 0 {
		return base[index:]
	}
	return ""
}

func setKnown(field *MetadataField, value string, observedAt *time.Time) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	evidence := EvidenceNativeDurable
	field.State = MetadataKnown
	field.Value = stringPointer(value)
	field.Evidence = &evidence
	field.ObservedAt = cloneTime(observedAt)
}

func setNativeIdentity(state *projectionState, value string, line int) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	if state.projection.NativeThreadID != "" &&
		state.projection.IdentityEvidence == EvidenceNativeDurable &&
		state.projection.NativeThreadID != value {
		state.identityConflicted = true
		addDiagnostic(&state.diagnostics, &state.summary, Diagnostic{
			Kind: DiagnosticIdentityConflict, Line: line,
			Message: "source file claims more than one native thread identity",
		})
		return false
	}
	state.projection.NativeThreadID = value
	state.projection.IdentityEvidence = EvidenceNativeDurable
	return true
}

func setText(target **string, value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		*target = stringPointer(value)
	}
}

// observeTimestamp folds a timestamp into both ends of the thread's span. It
// is for providers whose file states the thread's own updated time (Grok's
// summary document); JSONL adapters use the activity/message pair below.
func observeTimestamp(projection *ThreadProjection, timestamp *time.Time) {
	if timestamp == nil {
		return
	}
	if projection.CreatedAt == nil || timestamp.Before(*projection.CreatedAt) {
		projection.CreatedAt = cloneTime(timestamp)
	}
	if projection.UpdatedAt == nil || timestamp.After(*projection.UpdatedAt) {
		projection.UpdatedAt = cloneTime(timestamp)
	}
}

// observeActivity records that the file was written at this timestamp. It
// moves CreatedAt earlier but deliberately leaves UpdatedAt alone: a provider
// writes plenty that is not conversation — attachments, queue operations,
// token counts, harness notifications — and a thread whose last message is an
// hour old should not read as active because bookkeeping landed a minute ago
// (the operator 2026-08-07).
func observeActivity(state *projectionState, timestamp *time.Time) {
	if timestamp == nil {
		return
	}
	if state.projection.CreatedAt == nil || timestamp.Before(*state.projection.CreatedAt) {
		state.projection.CreatedAt = cloneTime(timestamp)
	}
	if state.lastActivity == nil || timestamp.After(*state.lastActivity) {
		state.lastActivity = cloneTime(timestamp)
	}
}

// observeMessage records a row that is conversation — a person's prompt or the
// agent's reply. This is the only thing that advances UpdatedAt.
func observeMessage(state *projectionState, timestamp *time.Time) {
	observeActivity(state, timestamp)
	if timestamp == nil {
		return
	}
	if state.projection.UpdatedAt == nil || timestamp.After(*state.projection.UpdatedAt) {
		state.projection.UpdatedAt = cloneTime(timestamp)
	}
}

func stringPointer(value string) *string {
	copy := value
	return &copy
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// grokSessionDirName returns the session-id directory name that is the
// parent of a discovered summary.json or updates.jsonl path — the documented
// identity fallback when summary.info.id is absent.
func grokSessionDirName(sourcePath string) string {
	dir := filepath.Dir(sourcePath)
	base := filepath.Base(dir)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

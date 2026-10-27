package history

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrThreadNamesMalformed reports that an existing truth file is unreadable or
// violates the loader rules. The writer refuses to overwrite broken truth (it
// extends the reader's never-rewrite-on-malformed rule to the write path).
var ErrThreadNamesMalformed = errors.New("thread names file is malformed")

// ValidateThreadName applies the loader's name rules to a rename request: it
// trims surrounding whitespace and requires a non-empty name of at most
// maxThreadNameRunes runes. The trimmed value is what gets written. It shares
// the loader's constant so the two paths can never drift.
func ValidateThreadName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", errors.New("name is required and must be non-empty")
	}
	if utf8.RuneCountInString(trimmed) > maxThreadNameRunes {
		return "", fmt.Errorf("name exceeds %d runes", maxThreadNameRunes)
	}
	return trimmed, nil
}

// ThreadNameMutation is a single set-or-clear on the truth-file snapshot, keyed
// by the durable (Provider, NativeThreadID).
type ThreadNameMutation struct {
	Provider       Provider
	NativeThreadID string
	// Clear removes the record entirely regardless of provenance. Otherwise
	// Name (already validated and trimmed) is written with manual provenance and
	// UpdatedAt as the record timestamp.
	Clear     bool
	Name      string
	UpdatedAt time.Time
	// OnlyIfAbsent is the staged pre-bind rename path. It may fill a missing
	// name but never replace a later operator rename or an imported name.
	OnlyIfAbsent bool
}

// ApplyThreadNameSnapshot parses `existing` (nil/empty = a fresh version-1
// file), refuses malformed input with ErrThreadNamesMalformed, applies the
// single mutation, and re-serializes the whole snapshot. The output is
// byte-compatible with the established truth-file format: records sorted by
// (provider, nativeThreadId), canonical field order, unknown top-level and
// per-record fields preserved, 2-space indent, trailing newline. A mutation
// that changes nothing (an unchanged manual name, or clearing an absent record)
// returns changed=false and no bytes so the caller can skip the write.
func ApplyThreadNameSnapshot(existing []byte, mutation ThreadNameMutation) (result []byte, changed bool, err error) {
	top, records, err := parseSnapshotForWrite(existing)
	if err != nil {
		return nil, false, err
	}
	if !applyThreadNameMutation(&records, mutation) {
		return nil, false, nil
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].provider != records[j].provider {
			return records[i].provider < records[j].provider
		}
		return records[i].nativeThreadID < records[j].nativeThreadID
	})
	out, err := serializeThreadNames(top, records)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// objectMember is one key/value of a JSON object, kept in file order so unknown
// fields survive a rewrite in their original position.
type objectMember struct {
	key   string
	value json.RawMessage
}

// orderedObject marshals its members in slice order (Go maps would sort keys and
// drop order). It emits compact JSON; the caller's Encoder re-indents it.
type orderedObject []objectMember

func (o orderedObject) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(marshalNoHTML(m.key))
		buf.WriteByte(':')
		if len(m.value) == 0 {
			buf.WriteString("null")
		} else {
			buf.Write(m.value)
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalNoHTML encodes v to compact JSON without Go's default HTML escaping,
// matching Python json.dumps(ensure_ascii=False). Inputs are strings and
// []orderedObject, neither of which can fail to encode.
func marshalNoHTML(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}

// mutableNameRecord is a truth-file record parsed with field order and unknown
// fields preserved, so a single rename round-trips the rest of the file.
type mutableNameRecord struct {
	provider       string
	nativeThreadID string
	name           string
	provenance     string
	updatedAt      string
	hasName        bool
	hasProvenance  bool
	hasUpdatedAt   bool
	unknown        []objectMember // record fields outside the canonical five, in order
}

func (r mutableNameRecord) toOrderedObject() orderedObject {
	obj := orderedObject{
		{key: "provider", value: marshalNoHTML(r.provider)},
		{key: "nativeThreadId", value: marshalNoHTML(r.nativeThreadID)},
	}
	if r.hasName {
		obj = append(obj, objectMember{key: "name", value: marshalNoHTML(r.name)})
	}
	if r.hasProvenance {
		obj = append(obj, objectMember{key: "provenance", value: marshalNoHTML(r.provenance)})
	}
	if r.hasUpdatedAt {
		obj = append(obj, objectMember{key: "updatedAt", value: marshalNoHTML(r.updatedAt)})
	}
	return append(obj, r.unknown...)
}

// parseSnapshotForWrite returns the top-level members (with version normalized
// to 1 and the names member emptied for rebuild) and the parsed records. Empty
// input is a fresh document. Any structural or rule violation is malformed.
func parseSnapshotForWrite(existing []byte) ([]objectMember, []mutableNameRecord, error) {
	if len(bytes.TrimSpace(existing)) == 0 {
		return []objectMember{
			{key: "version", value: json.RawMessage("1")},
			{key: "names", value: nil},
		}, nil, nil
	}
	if _, err := parseThreadNamesBytes(existing); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrThreadNamesMalformed, err)
	}
	top, err := decodeOrderedObject(existing)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrThreadNamesMalformed, err)
	}
	var records []mutableNameRecord
	sawNames := false
	for i := range top {
		switch top[i].key {
		case "version":
			top[i].value = json.RawMessage("1")
		case "names":
			sawNames = true
			var elements []json.RawMessage
			if err := json.Unmarshal(top[i].value, &elements); err != nil {
				return nil, nil, fmt.Errorf("%w: %v", ErrThreadNamesMalformed, err)
			}
			for _, element := range elements {
				record, err := parseRecordForWrite(element)
				if err != nil {
					return nil, nil, fmt.Errorf("%w: %v", ErrThreadNamesMalformed, err)
				}
				records = append(records, record)
			}
			top[i].value = nil // rebuilt in serializeThreadNames
		}
	}
	if !sawNames {
		return nil, nil, fmt.Errorf("%w: names array is missing", ErrThreadNamesMalformed)
	}
	return top, records, nil
}

func parseRecordForWrite(raw json.RawMessage) (mutableNameRecord, error) {
	members, err := decodeOrderedObject(raw)
	if err != nil {
		return mutableNameRecord{}, err
	}
	var record mutableNameRecord
	for _, member := range members {
		switch member.key {
		case "provider":
			if err := json.Unmarshal(member.value, &record.provider); err != nil {
				return mutableNameRecord{}, err
			}
		case "nativeThreadId":
			if err := json.Unmarshal(member.value, &record.nativeThreadID); err != nil {
				return mutableNameRecord{}, err
			}
		case "name":
			if err := json.Unmarshal(member.value, &record.name); err != nil {
				return mutableNameRecord{}, err
			}
			record.hasName = true
		case "provenance":
			if err := json.Unmarshal(member.value, &record.provenance); err != nil {
				return mutableNameRecord{}, err
			}
			record.hasProvenance = true
		case "updatedAt":
			if err := json.Unmarshal(member.value, &record.updatedAt); err != nil {
				return mutableNameRecord{}, err
			}
			record.hasUpdatedAt = true
		default:
			record.unknown = append(record.unknown, member)
		}
	}
	return record, nil
}

// applyThreadNameMutation applies the set-or-clear in place and reports whether
// the record set actually changed. A PUT whose name and manual provenance
// already match is a no-op; a PUT over any other state (including an
// historical imported record) flips provenance to manual and restamps updatedAt.
func applyThreadNameMutation(records *[]mutableNameRecord, mutation ThreadNameMutation) bool {
	provider := string(mutation.Provider)
	index := -1
	for i := range *records {
		if (*records)[i].provider == provider && (*records)[i].nativeThreadID == mutation.NativeThreadID {
			index = i
			break
		}
	}
	if mutation.Clear {
		if index < 0 {
			return false
		}
		*records = append((*records)[:index], (*records)[index+1:]...)
		return true
	}
	if mutation.OnlyIfAbsent && index >= 0 {
		return false
	}
	stamp := mutation.UpdatedAt.UTC().Format(time.RFC3339)
	if index < 0 {
		*records = append(*records, mutableNameRecord{
			provider: provider, nativeThreadID: mutation.NativeThreadID,
			name: mutation.Name, hasName: true,
			provenance: string(ThreadNameManual), hasProvenance: true,
			updatedAt: stamp, hasUpdatedAt: true,
		})
		return true
	}
	record := &(*records)[index]
	if record.hasName && record.name == mutation.Name &&
		record.hasProvenance && record.provenance == string(ThreadNameManual) {
		return false
	}
	record.name, record.hasName = mutation.Name, true
	record.provenance, record.hasProvenance = string(ThreadNameManual), true
	record.updatedAt, record.hasUpdatedAt = stamp, true
	return true
}

func serializeThreadNames(top []objectMember, records []mutableNameRecord) ([]byte, error) {
	recordObjects := make([]orderedObject, 0, len(records))
	for _, record := range records {
		recordObjects = append(recordObjects, record.toOrderedObject())
	}
	names := marshalNoHTML(recordObjects) // non-nil slice → "[]" when empty
	out := make(orderedObject, 0, len(top))
	for _, member := range top {
		if member.key == "names" {
			out = append(out, objectMember{key: "names", value: names})
			continue
		}
		out = append(out, member)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeOrderedObject reads a JSON object preserving key order, capturing each
// value as raw bytes so unknown fields and nested structure survive verbatim.
func decodeOrderedObject(raw []byte) ([]objectMember, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("expected a JSON object")
	}
	var members []objectMember
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("expected a string object key")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		members = append(members, objectMember{key: key, value: value})
	}
	if _, err := decoder.Token(); err != nil { // consume the closing brace
		return nil, err
	}
	return members, nil
}

package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// ThreadNameProvenance records who assigned a name. Historical imported values
// remain readable so the truth file does not need a destructive rewrite.
type ThreadNameProvenance string

const (
	ThreadNameManual ThreadNameProvenance = "manual"
	// ThreadNameHistoricalImport identifies names imported from an earlier source.
	ThreadNameHistoricalImport ThreadNameProvenance = "imported"
	// ThreadNameCreation is derived from Nano's immutable hosted-thread
	// creation fact. It never appears in thread-names.json and ranks below both
	// operator and historical-import truth.
	ThreadNameCreation ThreadNameProvenance = "creation"
)

const maxThreadNameRunes = 200

// threadNamesFileVersion is the only snapshot layout nano reads. A different
// version means a newer writer changed the format; nano fails loudly rather
// than guessing, and never rewrites the file (see the flat-file-truth decision).
const threadNamesFileVersion = 1

// ThreadNameRecord is one durable name keyed by (Provider, NativeThreadID).
// Manual and imported records come from the truth-file loader; creation records
// come only from Nano's canonical hosted-thread creation facts. All three feed
// the derived thread_names table.
type ThreadNameRecord struct {
	Provider       Provider
	NativeThreadID string
	Name           string
	Provenance     ThreadNameProvenance
	UpdatedAt      *time.Time
}

// ThreadNameRef is the read-time name overlay joined onto a Thread. It is nil
// when no name record exists — never a fabricated value.
type ThreadNameRef struct {
	Value      string               `json:"value"`
	Provenance ThreadNameProvenance `json:"provenance"`
}

type threadNamesFile struct {
	Version int               `json:"version"`
	Names   []threadNameEntry `json:"names"`
}

type threadNameEntry struct {
	Provider       string `json:"provider"`
	NativeThreadID string `json:"nativeThreadId"`
	Name           string `json:"name"`
	Provenance     string `json:"provenance"`
	UpdatedAt      string `json:"updatedAt"`
}

// LoadThreadNames reads the nano-owned truth file. A missing file is an empty
// set, not an error. A malformed file is a loud error and the caller must
// neither rewrite the file nor overwrite the derived copy with partial data.
// Unknown top-level/record fields are tolerated for forward compatibility.
func LoadThreadNames(path string) ([]ThreadNameRecord, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read thread names file: %w", err)
	}
	return parseThreadNamesBytes(raw)
}

// parseThreadNamesBytes validates a truth-file snapshot already read into memory
// under the same rules as LoadThreadNames. It is the shared validation gate for
// both the reader and the writer's refuse-to-overwrite-malformed check.
func parseThreadNamesBytes(raw []byte) ([]ThreadNameRecord, error) {
	var file threadNamesFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse thread names file: %w", err)
	}
	if file.Version != threadNamesFileVersion {
		return nil, fmt.Errorf("thread names file version %d is not supported (want %d)", file.Version, threadNamesFileVersion)
	}
	records := make([]ThreadNameRecord, 0, len(file.Names))
	seen := make(map[string]bool, len(file.Names))
	for index, entry := range file.Names {
		record, err := parseThreadNameEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("thread names record %d: %w", index, err)
		}
		key := threadNameKey(record.Provider, record.NativeThreadID)
		if seen[key] {
			return nil, fmt.Errorf("thread names record %d: duplicate key (%s, %s)", index, record.Provider, record.NativeThreadID)
		}
		seen[key] = true
		records = append(records, record)
	}
	return records, nil
}

func parseThreadNameEntry(entry threadNameEntry) (ThreadNameRecord, error) {
	record := ThreadNameRecord{
		Provider:       Provider(strings.TrimSpace(entry.Provider)),
		NativeThreadID: strings.TrimSpace(entry.NativeThreadID),
		Name:           strings.TrimSpace(entry.Name),
		Provenance:     ThreadNameProvenance(strings.TrimSpace(entry.Provenance)),
	}
	// Earlier importers stored a source suffix. Normalize it only in memory;
	// reading a legacy names file never rewrites the operator's data.
	if strings.HasPrefix(string(record.Provenance), "imported-") {
		record.Provenance = ThreadNameHistoricalImport
	}
	if timestamp := strings.TrimSpace(entry.UpdatedAt); timestamp != "" {
		parsed, err := time.Parse(time.RFC3339, timestamp)
		if err != nil {
			return ThreadNameRecord{}, fmt.Errorf("updatedAt is not an RFC3339 timestamp: %w", err)
		}
		record.UpdatedAt = &parsed
	}
	if err := validateThreadNameRecord(record); err != nil {
		return ThreadNameRecord{}, err
	}
	if record.Provenance == ThreadNameCreation {
		return ThreadNameRecord{}, errors.New("creation provenance is derived and is not valid in the thread names file")
	}
	return record, nil
}

func validateThreadNameRecord(record ThreadNameRecord) error {
	if strings.TrimSpace(string(record.Provider)) == "" {
		return errors.New("provider is required")
	}
	if strings.TrimSpace(record.NativeThreadID) == "" {
		return errors.New("nativeThreadId is required")
	}
	name := strings.TrimSpace(record.Name)
	if name == "" {
		return errors.New("name is required and must be non-empty")
	}
	if utf8.RuneCountInString(name) > maxThreadNameRunes {
		return fmt.Errorf("name exceeds %d runes", maxThreadNameRunes)
	}
	if record.Provenance != ThreadNameManual && record.Provenance != ThreadNameHistoricalImport && record.Provenance != ThreadNameCreation {
		return fmt.Errorf("provenance %q is not manual, creation, or the supported historical import", record.Provenance)
	}
	return nil
}

func threadNameKey(provider Provider, nativeThreadID string) string {
	return string(provider) + "\x00" + nativeThreadID
}

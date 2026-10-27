package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeThreadNamesFile(t *testing.T, content string) string {
	t.Helper()
	// t.TempDir keeps the real ~/.nanod/thread-names.json untouched.
	path := filepath.Join(t.TempDir(), "thread-names.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadThreadNamesMissingFileIsEmptySet(t *testing.T) {
	records, err := LoadThreadNames(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || records != nil {
		t.Fatalf("missing file = %#v, %v; want empty, nil", records, err)
	}
}

func TestLoadThreadNamesValidSnapshot(t *testing.T) {
	path := writeThreadNamesFile(t, `{
	  "version": 1,
	  "extraTopLevel": "ignored",
	  "names": [
	    {"provider": "claude", "nativeThreadId": "  abc  ", "name": "  influence graph classifier  ", "provenance": "manual", "updatedAt": "2026-07-21T18:00:00Z", "unknownField": true},
	    {"provider": "codex", "nativeThreadId": "def", "name": "import guess", "provenance": "imported-legacy"}
	  ]
	}`)
	records, err := LoadThreadNames(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %#v", records)
	}
	if records[0].Provider != ProviderClaude || records[0].NativeThreadID != "abc" ||
		records[0].Name != "influence graph classifier" || records[0].Provenance != ThreadNameManual ||
		records[0].UpdatedAt == nil {
		t.Fatalf("trimmed manual record = %#v", records[0])
	}
	// A rename must not reject an older import source or rewrite its truth file.
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), `"provenance": "imported-legacy"`) {
		t.Fatalf("legacy import file changed: %s, %v", raw, err)
	}
	if records[1].Provenance != ThreadNameHistoricalImport || records[1].UpdatedAt != nil {
		t.Fatalf("imported record = %#v", records[1])
	}
}

func TestLoadThreadNamesRejectsMalformedSnapshots(t *testing.T) {
	cases := map[string]string{
		"invalid json":     `{"version": 1, "names": [`,
		"trailing garbage": `{"version": 1, "names": []} trailing`,
		"wrong version":    `{"version": 2, "names": []}`,
		"duplicate key": `{"version": 1, "names": [
		  {"provider": "claude", "nativeThreadId": "x", "name": "a", "provenance": "manual"},
		  {"provider": "claude", "nativeThreadId": "x", "name": "b", "provenance": "manual"}
		]}`,
		"empty name":         `{"version": 1, "names": [{"provider": "claude", "nativeThreadId": "x", "name": "   ", "provenance": "manual"}]}`,
		"missing provider":   `{"version": 1, "names": [{"provider": "", "nativeThreadId": "x", "name": "a", "provenance": "manual"}]}`,
		"missing native id":  `{"version": 1, "names": [{"provider": "claude", "nativeThreadId": "", "name": "a", "provenance": "manual"}]}`,
		"bad provenance":     `{"version": 1, "names": [{"provider": "claude", "nativeThreadId": "x", "name": "a", "provenance": "auto"}]}`,
		"derived provenance": `{"version": 1, "names": [{"provider": "claude", "nativeThreadId": "x", "name": "a", "provenance": "creation"}]}`,
		"bad updatedAt":      `{"version": 1, "names": [{"provider": "claude", "nativeThreadId": "x", "name": "a", "provenance": "manual", "updatedAt": "yesterday"}]}`,
	}
	for label, content := range cases {
		path := writeThreadNamesFile(t, content)
		if _, err := LoadThreadNames(path); err == nil {
			t.Fatalf("%s: malformed snapshot was accepted", label)
		}
	}
}

func TestLoadThreadNamesRejectsOverlongName(t *testing.T) {
	long := strings.Repeat("x", maxThreadNameRunes+1)
	path := writeThreadNamesFile(t, `{"version": 1, "names": [{"provider": "claude", "nativeThreadId": "x", "name": "`+long+`", "provenance": "manual"}]}`)
	if _, err := LoadThreadNames(path); err == nil {
		t.Fatal("name over the rune budget was accepted")
	}
	exact := strings.Repeat("x", maxThreadNameRunes)
	path = writeThreadNamesFile(t, `{"version": 1, "names": [{"provider": "claude", "nativeThreadId": "x", "name": "`+exact+`", "provenance": "manual"}]}`)
	records, err := LoadThreadNames(path)
	if err != nil || len(records) != 1 || records[0].Name != exact {
		t.Fatalf("name at the rune budget = %#v, %v", records, err)
	}
}

package history

import (
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}

func TestApplyThreadNameSnapshotFreshFileByteExact(t *testing.T) {
	got, changed, err := ApplyThreadNameSnapshot(nil, ThreadNameMutation{
		Provider: ProviderClaude, NativeThreadID: "77777777-7777-4777-8777-777777777777",
		Name: "graph classifier", UpdatedAt: mustTime(t, "2026-07-21T18:00:00Z"),
	})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	want := `{
  "version": 1,
  "names": [
    {
      "provider": "claude",
      "nativeThreadId": "77777777-7777-4777-8777-777777777777",
      "name": "graph classifier",
      "provenance": "manual",
      "updatedAt": "2026-07-21T18:00:00Z"
    }
  ]
}
`
	if string(got) != want {
		t.Fatalf("fresh snapshot mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestApplyThreadNameSnapshotPreservesUnknownFieldsSortedAndFlipsProvenance(t *testing.T) {
	existing := []byte(`{"version":1,"topExtra":{"a":1},"names":[` +
		`{"provider":"codex","nativeThreadId":"z","name":"old","provenance":"` + string(ThreadNameHistoricalImport) + `","updatedAt":"2026-01-01T00:00:00Z","note":"keep me"},` +
		`{"provider":"claude","nativeThreadId":"a","name":"keep","provenance":"manual","updatedAt":"2026-01-02T00:00:00Z"}]}`)
	// Rename the imported codex record: provenance must flip to manual, updatedAt
	// restamped, its unknown "note" and the unknown top-level "topExtra" survive,
	// output re-sorted by (provider, nativeThreadId).
	got, changed, err := ApplyThreadNameSnapshot(existing, ThreadNameMutation{
		Provider: ProviderCodex, NativeThreadID: "z",
		Name: "new", UpdatedAt: mustTime(t, "2026-07-21T18:00:00Z"),
	})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	want := `{
  "version": 1,
  "topExtra": {
    "a": 1
  },
  "names": [
    {
      "provider": "claude",
      "nativeThreadId": "a",
      "name": "keep",
      "provenance": "manual",
      "updatedAt": "2026-01-02T00:00:00Z"
    },
    {
      "provider": "codex",
      "nativeThreadId": "z",
      "name": "new",
      "provenance": "manual",
      "updatedAt": "2026-07-21T18:00:00Z",
      "note": "keep me"
    }
  ]
}
`
	if string(got) != want {
		t.Fatalf("merged snapshot mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestApplyThreadNameSnapshotClearRemovesRecordRegardlessOfProvenance(t *testing.T) {
	existing := []byte(`{"version":1,"names":[` +
		`{"provider":"claude","nativeThreadId":"a","name":"keep","provenance":"manual","updatedAt":"2026-01-02T00:00:00Z"},` +
		`{"provider":"codex","nativeThreadId":"z","name":"drop","provenance":"` + string(ThreadNameHistoricalImport) + `","updatedAt":"2026-01-01T00:00:00Z"}]}`)
	got, changed, err := ApplyThreadNameSnapshot(existing, ThreadNameMutation{
		Provider: ProviderCodex, NativeThreadID: "z", Clear: true,
	})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if strings.Contains(string(got), "\"z\"") || strings.Contains(string(got), "drop") {
		t.Fatalf("cleared record still present:\n%s", got)
	}
	if !strings.Contains(string(got), "\"keep\"") {
		t.Fatalf("clear removed the wrong record:\n%s", got)
	}
}

func TestApplyThreadNameSnapshotNoOps(t *testing.T) {
	existing := []byte(`{"version":1,"names":[{"provider":"claude","nativeThreadId":"a","name":"same","provenance":"manual","updatedAt":"2026-01-02T00:00:00Z"}]}`)
	// PUT of an identical manual name changes nothing.
	if _, changed, err := ApplyThreadNameSnapshot(existing, ThreadNameMutation{
		Provider: ProviderClaude, NativeThreadID: "a", Name: "same", UpdatedAt: mustTime(t, "2026-07-21T18:00:00Z"),
	}); err != nil || changed {
		t.Fatalf("unchanged manual PUT: changed=%v err=%v", changed, err)
	}
	// DELETE of an absent record changes nothing.
	if _, changed, err := ApplyThreadNameSnapshot(existing, ThreadNameMutation{
		Provider: ProviderCodex, NativeThreadID: "missing", Clear: true,
	}); err != nil || changed {
		t.Fatalf("clear-of-absent: changed=%v err=%v", changed, err)
	}
}

func TestApplyThreadNameSnapshotOnlyIfAbsentPreservesOperatorRename(t *testing.T) {
	existing := []byte(`{"version":1,"names":[{"provider":"claude","nativeThreadId":"a","name":"operator rename","provenance":"manual","updatedAt":"2026-01-02T00:00:00Z"}]}`)
	if _, changed, err := ApplyThreadNameSnapshot(existing, ThreadNameMutation{
		Provider: ProviderClaude, NativeThreadID: "a", Name: "creation name",
		UpdatedAt: mustTime(t, "2026-07-21T18:00:00Z"), OnlyIfAbsent: true,
	}); err != nil || changed {
		t.Fatalf("creation name overwrote operator rename: changed=%v err=%v", changed, err)
	}
}

func TestApplyThreadNameSnapshotSameNameFlipsImportedProvenance(t *testing.T) {
	existing := []byte(`{"version":1,"names":[{"provider":"claude","nativeThreadId":"a","name":"same","provenance":"` + string(ThreadNameHistoricalImport) + `","updatedAt":"2026-01-02T00:00:00Z"}]}`)
	got, changed, err := ApplyThreadNameSnapshot(existing, ThreadNameMutation{
		Provider: ProviderClaude, NativeThreadID: "a", Name: "same", UpdatedAt: mustTime(t, "2026-07-21T18:00:00Z"),
	})
	if err != nil || !changed {
		t.Fatalf("claiming an imported name must change it: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(string(got), `"provenance": "manual"`) || strings.Contains(string(got), string(ThreadNameHistoricalImport)) {
		t.Fatalf("provenance did not flip to manual:\n%s", got)
	}
}

func TestApplyThreadNameSnapshotRefusesMalformed(t *testing.T) {
	for _, existing := range []string{
		`{"version":1,"names":[`,                        // truncated
		`{"version":2,"names":[]}`,                      // unsupported version
		`{"version":1,"names":[{"provider":"claude"}]}`, // record missing required fields
	} {
		_, _, err := ApplyThreadNameSnapshot([]byte(existing), ThreadNameMutation{
			Provider: ProviderClaude, NativeThreadID: "a", Name: "x", UpdatedAt: time.Now(),
		})
		if err == nil || !strings.Contains(err.Error(), ErrThreadNamesMalformed.Error()) {
			t.Fatalf("expected malformed error for %q, got %v", existing, err)
		}
	}
}

func TestValidateThreadName(t *testing.T) {
	if got, err := ValidateThreadName("  spaced name  "); err != nil || got != "spaced name" {
		t.Fatalf("trim: got %q err %v", got, err)
	}
	if _, err := ValidateThreadName("   "); err == nil {
		t.Fatal("whitespace-only name must be rejected")
	}
	if _, err := ValidateThreadName(""); err == nil {
		t.Fatal("empty name must be rejected")
	}
	if _, err := ValidateThreadName(strings.Repeat("x", maxThreadNameRunes)); err != nil {
		t.Fatalf("exactly max runes must be accepted: %v", err)
	}
	if _, err := ValidateThreadName(strings.Repeat("x", maxThreadNameRunes+1)); err == nil {
		t.Fatal("over-max name must be rejected")
	}
}

package sidebar

import (
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"path/filepath"
	"testing"
)

func openTestThreadStars(t *testing.T) *Stars {
	t.Helper()
	store, err := OpenStars(filepath.Join(t.TempDir(), "stars.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
func TestThreadStarsSetUnsetAndSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stars.sqlite")
	store, err := OpenStars(path)
	if err != nil {
		t.Fatal(err)
	}
	if store.IsStarred("codex", "native-a") {
		t.Fatal("empty store reported a star")
	}
	if err := store.SetStarred("codex", "native-a", true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetStarred("claude", "native-b", true); err != nil {
		t.Fatal(err)
	}
	if !store.IsStarred("codex", "native-a") || !store.IsStarred("claude", "native-b") {
		t.Fatal("set stars missing from cache")
	}
	if err := store.SetStarred("codex", "native-a", false); err != nil {
		t.Fatal(err)
	}
	if store.IsStarred("codex", "native-a") {
		t.Fatal("unstar left the row starred")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStars(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if reopened.IsStarred("codex", "native-a") {
		t.Fatal("unstar did not survive reopen")
	}
	if !reopened.IsStarred("claude", "native-b") {
		t.Fatal("star did not survive reopen")
	}
}

func TestThreadStarsRejectBlankIdentity(t *testing.T) {
	store := openTestThreadStars(t)
	if err := store.SetStarred("codex", "", true); err == nil {
		t.Fatal("expected identity error")
	}
	if store.IsStarred("codex", "") {
		t.Fatal("blank native id must not star")
	}
}

func TestThreadStarsUnstarManyIsAtomicAndUpdatesCache(t *testing.T) {
	store := openTestThreadStars(t)
	for _, nativeID := range []string{"native-a", "native-b"} {
		if err := store.SetStarred("codex", nativeID, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UnstarMany([]hostedstore.AttachmentKey{
		{Provider: "codex", NativeThreadID: "native-a"},
		{Provider: "codex", NativeThreadID: "native-b"},
	}); err != nil {
		t.Fatal(err)
	}
	if store.IsStarred("codex", "native-a") || store.IsStarred("codex", "native-b") {
		t.Fatal("bulk unstar left cache entries behind")
	}
}

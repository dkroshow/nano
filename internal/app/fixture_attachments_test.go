package app

import (
	"path/filepath"
	"testing"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// newTestStore supplies a real attachment store to app integration tests.
func newTestStore(t *testing.T) *hostedstore.AttachmentStore {
	t.Helper()
	store, err := hostedstore.NewAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}
func attach(id, cwd, at string) hostedstore.Attachment {
	return hostedstore.Attachment{Provider: "codex", NativeThreadID: id, Cwd: cwd, Model: "gpt-5", AttachedAt: at}
}

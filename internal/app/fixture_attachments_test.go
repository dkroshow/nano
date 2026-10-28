package app

import (
	"github.com/dkroshow/nano/internal/hosted"
	"path/filepath"
	"testing"
)

import fixture "github.com/dkroshow/nano/internal/testsupport"

type attachmentRecorder = fixture.AttachmentRecorder

// newTestStore supplies a real attachment store to app integration tests.
func newTestStore(t *testing.T) *hosted.AttachmentStore {
	t.Helper()
	store, err := hosted.NewAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}
func attach(id, cwd, at string) attachmentRecord {
	return attachmentRecord{Provider: "codex", NativeThreadID: id, Cwd: cwd, Model: "gpt-5", AttachedAt: at}
}

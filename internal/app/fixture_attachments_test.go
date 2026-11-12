package app

import (
	"github.com/dkroshow/nano/internal/testsupport"
	"testing"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// newTestStore supplies a real attachment store to app integration tests.
func newTestStore(t *testing.T) *hostedstore.AttachmentStore {
	t.Helper()
	store, _ := testsupport.AttachmentStore(t)
	return store
}

// fixtureHosted gives a test record the hosted identity every stored
// attachment has, derived from its native id as seedHostedAttachmentFixture
// expects.
func fixtureHosted(rec hostedstore.Attachment) hostedstore.Attachment {
	if rec.HostedThreadID == "" {
		rec.HostedThreadID = "nht_fixture_" + hostedstore.AgentHash([]byte(rec.Provider + "\x00" + rec.NativeThreadID))[:16]
	}
	return rec
}

func attach(id, cwd, at string) hostedstore.Attachment {
	return hostedstore.Attachment{Provider: "codex", NativeThreadID: id, Cwd: cwd, Model: "gpt-5", AttachedAt: at}
}

package sessions

import (
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/testsupport"
)

func claudeTestSession(t *testing.T) (*Session, *testsupport.AttachmentRecorder) {
	t.Helper()
	recorder := &testsupport.AttachmentRecorder{}
	queue := mustQueuedTurnStore(t)
	attempts, err := newSendAttemptStore(queue.Database(), queue.Path(), "claude-test", testPromptStore(t, queue.Database()))
	if err != nil {
		t.Fatal(err)
	}
	store := mustBindTransactionalTurnStore(t, queue, attempts)
	sess := New(Config{ID: "sess-1", Provider: "claude", HostedThreadID: "nht_claude_test", CWD: t.TempDir(), CreatedAt: time.Now(), ProcessStarted: time.Now(), RememberAttachment: recorder.Remember, ForgetAttachment: recorder.Forget, Metadata: newRecordingMetadata(queue.Database(), "nht_claude_test"), Turns: store, QueuedTurns: queue, SendAttempts: attempts})
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	return sess, recorder
}

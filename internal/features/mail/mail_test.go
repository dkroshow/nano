package mail

import (
	"testing"

	"github.com/dkroshow/nano/internal/interact"
)

func TestSyncUnreadNoticeOwnsOnlyMailLine(t *testing.T) {
	notices := interact.NewNotices()
	if err := notices.Set("thread", "heartbeat", "heartbeat note"); err != nil {
		t.Fatal(err)
	}
	if err := SyncUnreadNotice(notices, "thread", 2); err != nil {
		t.Fatal(err)
	}
	want := "prompt\n\nheartbeat note\n\n[nano: 2 unread — GET /api/v1/hosted-threads/thread/inbox]"
	if got := notices.Append("thread", "prompt"); got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}
	if err := SyncUnreadNotice(notices, "thread", 0); err != nil {
		t.Fatal(err)
	}
	if got := notices.Append("thread", "prompt"); got != "prompt\n\nheartbeat note" {
		t.Fatalf("clear removed another owner: %q", got)
	}
}

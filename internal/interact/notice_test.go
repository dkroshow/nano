package interact

import (
	"strings"
	"testing"
)

func TestNoticesKeepOneBoundedVerbatimNotePerOwner(t *testing.T) {
	notices := NewNotices()
	if err := notices.Set("target", "mail", "[nano: 2 unread]"); err != nil {
		t.Fatal(err)
	}
	if err := notices.Set("target", "mail", " [nano: 3 unread] "); err != nil {
		t.Fatal(err)
	}
	if err := notices.Set("target", "status", "status line"); err != nil {
		t.Fatal(err)
	}
	if got, want := notices.Append("target", "prompt"), "prompt\n\n [nano: 3 unread] \n\nstatus line"; got != want {
		t.Fatalf("appended prompt = %q, want %q", got, want)
	}
	if err := notices.Set("target", "mail", strings.Repeat("x", MaxNoticeBytes+1)); err == nil {
		t.Fatal("oversized notice accepted")
	}
	if err := notices.Clear("target", "mail"); err != nil {
		t.Fatal(err)
	}
	if got := notices.Append("target", "prompt"); got != "prompt\n\nstatus line" {
		t.Fatalf("prompt after owner clear = %q", got)
	}
}

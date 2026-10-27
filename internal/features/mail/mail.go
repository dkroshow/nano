package mail

import (
	"fmt"

	"github.com/dkroshow/nano/internal/interact"
)

const NoticeOwner = "mail"

// SyncUnreadNotice changes only mail's bounded per-thread notice. Core prompt
// admission appends the shared notice set without knowing how mail is stored.
func SyncUnreadNotice(notices *interact.Notices, recipient string, unread int) error {
	if unread == 0 {
		return notices.Clear(recipient, NoticeOwner)
	}
	return notices.Set(recipient, NoticeOwner, fmt.Sprintf("[nano: %d unread — GET /api/v1/hosted-threads/%s/inbox]", unread, recipient))
}

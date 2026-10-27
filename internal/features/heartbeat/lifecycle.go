package heartbeat

import (
	"database/sql"
	"errors"
	"time"
)

const TargetClosedReason = "target is gone: thread closed"

// PauseAttachmentForClosedTarget makes an explicit target close pause
// heartbeat scheduling with retained configuration and audit evidence.
func (h *Service) PauseAttachmentForClosedTarget(target string, at time.Time) error {
	attachment, err := h.Store.AttachmentForTarget(target)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := h.Store.MutateLifecycle(
		attachment.ID,
		"target_gone",
		TargetClosedReason,
		Actor{Kind: "system", Name: "nano"},
		StatePaused,
		at,
	); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

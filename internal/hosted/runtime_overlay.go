package hosted

import (
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"log"
	goruntime "runtime"
	"time"
)

// NotifyRuntime tells thread-index SSE subscribers that some runtime state
// changed (bind, unbind, turnActive flip). Nil-safe: a server wired without
// the hub simply has no subscribers to tell.
func (s *AttachmentCoordinator) NotifyRuntime() {
	if s.deferBootRuntime(false) {
		return
	}
	s.publishRuntime(false)
}

// NotifyRuntimeRefresh publishes the latest runtime snapshot and also tells
// browsers to re-read catalog/sidebar structure. Use it for durable metadata
// changes that alter row placement but are not represented by sidebar.RuntimeInfo.
// Every open page reads the whole session list for it, so each call is logged
// with its caller: a burst of reads can then be traced to who asked.
func (s *AttachmentCoordinator) NotifyRuntimeRefresh() {
	if pc, _, _, ok := goruntime.Caller(1); ok {
		log.Printf("runtime refresh requested by %s", goruntime.FuncForPC(pc).Name())
	}
	if s.deferBootRuntime(true) {
		return
	}
	s.publishRuntime(true)
}

// Handshakes must not pay for fleet snapshots while the boot queues register.
func (s *AttachmentCoordinator) deferBootRuntime(refresh bool) bool {
	boot := s.boot
	if boot == nil {
		return false
	}
	boot.mu.Lock()
	defer boot.mu.Unlock()
	if !boot.runtimeActive {
		return false
	}
	boot.runtimeDirty = true
	boot.runtimeRefresh = boot.runtimeRefresh || refresh
	return true
}

func (s *AttachmentCoordinator) publishBootRuntime(final bool) {
	boot := s.boot
	boot.mu.Lock()
	dirty, refresh := boot.runtimeDirty, boot.runtimeRefresh
	boot.runtimeDirty, boot.runtimeRefresh = false, false
	boot.mu.Unlock()
	if dirty || final {
		s.publishRuntime(refresh)
	}
	if final {
		// Notices arriving during the final snapshot must survive the handoff
		// to immediate publishing, without holding handshakes behind its build.
		boot.mu.Lock()
		dirty, refresh = boot.runtimeDirty, boot.runtimeRefresh
		boot.runtimeActive, boot.runtimeDirty, boot.runtimeRefresh = false, false, false
		boot.mu.Unlock()
		if dirty {
			s.publishRuntime(refresh)
		}
	}
}

// publishRuntime sends the current runtime snapshot to sidebar subscribers.
func (s *AttachmentCoordinator) publishRuntime(refresh bool) {
	s.Runtime.PublishRuntimeDelta(refresh, false)
}

// RememberAttachment persists a durable attachment; nil-safe so a provider
// handshake can call it unconditionally when the store is unavailable. Callers
// supply the record's identity and display fields; the attach time is stamped
// here so every provider orders detached entries newest-first identically.
func (s *AttachmentCoordinator) RememberAttachment(rec hostedstore.Attachment) {
	if s.Attachments == nil {
		return
	}
	rec.AttachedAt = time.Now().Format(time.RFC3339)
	s.Attachments.Remember(rec)
}

// ForgetAttachment drops a durable attachment record; nil-safe like
// RememberAttachment. Handed to sessions so a `--resume` successor can retire
// the record it supersedes (claude.go) without reaching for the store itself.
func (s *AttachmentCoordinator) ForgetAttachment(provider, nativeThreadID string) {
	if s.Attachments == nil {
		return
	}
	s.Attachments.Forget(provider, nativeThreadID)
}

// setAttachmentTurnAnchor persists (or clears) the live-turn anchor on an
// existing attachment record; nil-safe like the rest. Handed to sessions so
// the claude transport can keep the record current without reaching for the
// store — it is the one piece of live state that has to outlive the process.
func (s *AttachmentCoordinator) setAttachmentTurnAnchor(provider, nativeThreadID string, startedAtMs, anchorSeq int64) {
	if s.Attachments == nil {
		return
	}
	s.Attachments.SetTurnAnchor(provider, nativeThreadID, startedAtMs, anchorSeq)
}

// setAttachmentPendingName persists a pre-init rename onto an existing
// provisional record; nil-safe like the rest (handleSessionName).
func (s *AttachmentCoordinator) setAttachmentPendingName(provider, nativeThreadID, name string) {
	if s.Attachments == nil {
		return
	}
	s.Attachments.SetPendingName(provider, nativeThreadID, name)
}

// setAttachmentShimSession ties an existing attachment record to the shim
// child now hosting its thread; nil-safe like the rest. Handed to sessions for
// the `--resume` respawn window (claude.go, openClaudeSession).
func (s *AttachmentCoordinator) setAttachmentShimSession(provider, nativeThreadID, shimSessionID string) {
	if s.Attachments == nil {
		return
	}
	s.Attachments.SetShimSession(provider, nativeThreadID, shimSessionID)
}

// AttachmentFor looks up a durable attachment; nil store (feature off) simply
// reports none, which callers must treat as "no evidence", never as an error.
func (s *AttachmentCoordinator) attachmentFor(provider, nativeThreadID string) (hostedstore.Attachment, bool) {
	if s.Attachments == nil {
		return hostedstore.Attachment{}, false
	}
	return s.Attachments.Get(provider, nativeThreadID)
}

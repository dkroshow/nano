package app

import (
	"log"
	goruntime "runtime"
	"time"
)

// notifyRuntime tells thread-index SSE subscribers that some runtime state
// changed (bind, unbind, turnActive flip). Nil-safe: a server wired without
// the hub simply has no subscribers to tell.
func (s *server) notifyRuntime() {
	if s.deferBootRuntime(false) {
		return
	}
	s.publishRuntime(false)
}

// notifyRuntimeRefresh publishes the latest runtime snapshot and also tells
// browsers to re-read catalog/sidebar structure. Use it for durable metadata
// changes that alter row placement but are not represented by sidebar.RuntimeInfo.
// Every open page reads the whole session list for it, so each call is logged
// with its caller: a burst of reads can then be traced to who asked.
func (s *server) notifyRuntimeRefresh() {
	if pc, _, _, ok := goruntime.Caller(1); ok {
		log.Printf("runtime refresh requested by %s", goruntime.FuncForPC(pc).Name())
	}
	if s.deferBootRuntime(true) {
		return
	}
	s.publishRuntime(true)
}

// Handshakes must not pay for fleet snapshots while the boot queues register.
func (s *server) deferBootRuntime(refresh bool) bool {
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

func (s *server) publishBootRuntime(final bool) {
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

func (s *server) publishRuntime(refresh bool) {
	s.sidebarOwner().PublishRuntimeDelta(refresh, false)
}

// rememberAttachment persists a durable attachment; nil-safe so a provider
// handshake can call it unconditionally when the store is unavailable. Callers
// supply the record's identity and display fields; the attach time is stamped
// here so every provider orders detached entries newest-first identically.
func (s *server) rememberAttachment(rec attachmentRecord) {
	if s.attachments == nil {
		return
	}
	rec.AttachedAt = time.Now().Format(time.RFC3339)
	s.attachments.Remember(rec)
}

// forgetAttachment drops a durable attachment record; nil-safe like
// rememberAttachment. Handed to sessions so a `--resume` successor can retire
// the record it supersedes (claude.go) without reaching for the store itself.
func (s *server) forgetAttachment(provider, nativeThreadID string) {
	if s.attachments == nil {
		return
	}
	s.attachments.Forget(provider, nativeThreadID)
}

// setAttachmentTurnAnchor persists (or clears) the live-turn anchor on an
// existing attachment record; nil-safe like the rest. Handed to sessions so
// the claude transport can keep the record current without reaching for the
// store — it is the one piece of live state that has to outlive the process.
func (s *server) setAttachmentTurnAnchor(provider, nativeThreadID string, startedAtMs, anchorSeq int64) {
	if s.attachments == nil {
		return
	}
	s.attachments.SetTurnAnchor(provider, nativeThreadID, startedAtMs, anchorSeq)
}

// setAttachmentPendingName persists a pre-init rename onto an existing
// provisional record; nil-safe like the rest (handleSessionName).
func (s *server) setAttachmentPendingName(provider, nativeThreadID, name string) {
	if s.attachments == nil {
		return
	}
	s.attachments.SetPendingName(provider, nativeThreadID, name)
}

// setAttachmentShimSession ties an existing attachment record to the shim
// child now hosting its thread; nil-safe like the rest. Handed to sessions for
// the `--resume` respawn window (claude.go, openClaudeSession).
func (s *server) setAttachmentShimSession(provider, nativeThreadID, shimSessionID string) {
	if s.attachments == nil {
		return
	}
	s.attachments.SetShimSession(provider, nativeThreadID, shimSessionID)
}

// attachmentFor looks up a durable attachment; nil store (feature off) simply
// reports none, which callers must treat as "no evidence", never as an error.
func (s *server) attachmentFor(provider, nativeThreadID string) (attachmentRecord, bool) {
	if s.attachments == nil {
		return attachmentRecord{}, false
	}
	return s.attachments.Get(provider, nativeThreadID)
}

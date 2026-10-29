package sessions

import (
	"sync"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// BootAttachments owns one boot's stored attachments that have not registered.
// Boot orchestration and the sidebar share these facts; neither keeps a copy.
type BootAttachments struct {
	mu       sync.Mutex
	pending  map[hostedstore.AttachmentKey]bool
	resting  map[hostedstore.AttachmentKey]bool
	byHosted map[string]hostedstore.Attachment
}

// NewBootAttachments marks the supplied stored attachments as starting.
func NewBootAttachments(records []hostedstore.Attachment) BootAttachments {
	pending := make(map[hostedstore.AttachmentKey]bool, len(records))
	byHosted := make(map[string]hostedstore.Attachment, len(records))
	for _, rec := range records {
		pending[rec.Key()] = true
		if rec.HostedThreadID != "" {
			byHosted[rec.HostedThreadID] = rec
		}
	}
	return BootAttachments{pending: pending, resting: map[hostedstore.AttachmentKey]bool{}, byHosted: byHosted}
}

// State reports a stored thread that still waits for its registration:
// "starting" while the boot loop will register it, "resting" while it waits
// for something to need it, and "" once settled. Nil-safe: without a boot set
// nothing waits.
func (b *BootAttachments) State(key hostedstore.AttachmentKey) string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stateLocked(key)
}

func (b *BootAttachments) stateLocked(key hostedstore.AttachmentKey) string {
	switch {
	case !b.pending[key]:
		return ""
	case b.resting[key]:
		return "resting"
	default:
		return "starting"
	}
}

// Waiting reports whether an attachment has not yet registered.
func (b *BootAttachments) Waiting(key hostedstore.AttachmentKey) bool { return b.State(key) != "" }

// WaitingHosted returns a waiting thread's record and state by hosted id.
func (b *BootAttachments) WaitingHosted(hostedThreadID string) (hostedstore.Attachment, string) {
	if b == nil || hostedThreadID == "" {
		return hostedstore.Attachment{}, ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, ok := b.byHosted[hostedThreadID]
	if !ok {
		return hostedstore.Attachment{}, ""
	}
	return rec, b.stateLocked(rec.Key())
}

// Rest leaves records for something to need them.
func (b *BootAttachments) Rest(records []hostedstore.Attachment) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, rec := range records {
		b.resting[rec.Key()] = true
	}
}

// Wake ends a thread's rest as its attach begins, so it reports starting.
func (b *BootAttachments) Wake(rec hostedstore.Attachment) {
	b.mu.Lock()
	delete(b.resting, rec.Key())
	b.mu.Unlock()
}

// Settle removes a completed registration from the waiting set.
func (b *BootAttachments) Settle(rec hostedstore.Attachment) {
	b.mu.Lock()
	delete(b.pending, rec.Key())
	delete(b.resting, rec.Key())
	b.mu.Unlock()
}

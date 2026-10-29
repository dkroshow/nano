package testsupport

import (
	"sync"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// AttachmentRecorder captures what a session would have persisted, standing in
// for the attachment store: remember and forget apply in order, so all()
// reports the records that would survive on disk.
type AttachmentRecorder struct {
	mu      sync.Mutex
	records []hostedstore.Attachment
}

func (r *AttachmentRecorder) Remember(rec hostedstore.Attachment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.records {
		if existing.Key() == rec.Key() {
			r.records[i] = rec
			return
		}
	}
	r.records = append(r.records, rec)
}

func (r *AttachmentRecorder) Forget(provider, nativeThreadID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := hostedstore.AttachmentKey{Provider: provider, NativeThreadID: nativeThreadID}
	kept := r.records[:0]
	for _, rec := range r.records {
		if rec.Key() != key {
			kept = append(kept, rec)
		}
	}
	r.records = kept
}

func (r *AttachmentRecorder) All() []hostedstore.Attachment {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hostedstore.Attachment(nil), r.records...)
}

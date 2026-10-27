package interact

import (
	"errors"
	"sort"
	"strings"
	"sync"
)

const (
	MaxNoticeBytes = 512
	maxNoticeKey   = 200
)

type Notices struct {
	mu       sync.RWMutex
	byThread map[string]map[string]string
}

// NewNotices creates the process-local prompt notice set. The owner key is the
// mutation boundary: Set and Clear can address only that key, never all notes.
func NewNotices() *Notices {
	return &Notices{byThread: make(map[string]map[string]string)}
}

// Set replaces one owner's note without normalizing its bytes.
func (n *Notices) Set(threadRef, ownerKey, note string) error {
	if n == nil {
		return errors.New("notices unavailable")
	}
	if !validNoticeKey(threadRef) || !validNoticeKey(ownerKey) {
		return errors.New("thread reference and owner key must contain 1 to 200 bytes without edge whitespace")
	}
	if note == "" || len(note) > MaxNoticeBytes {
		return errors.New("notice must contain 1 to 512 bytes")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	owners := n.byThread[threadRef]
	if owners == nil {
		owners = make(map[string]string)
		n.byThread[threadRef] = owners
	}
	owners[ownerKey] = note
	return nil
}

// Clear removes only the named owner's note.
func (n *Notices) Clear(threadRef, ownerKey string) error {
	if n == nil {
		return errors.New("notices unavailable")
	}
	if !validNoticeKey(threadRef) || !validNoticeKey(ownerKey) {
		return errors.New("thread reference and owner key must contain 1 to 200 bytes without edge whitespace")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	owners := n.byThread[threadRef]
	delete(owners, ownerKey)
	if len(owners) == 0 {
		delete(n.byThread, threadRef)
	}
	return nil
}

func validNoticeKey(value string) bool {
	return value != "" && len(value) <= maxNoticeKey && strings.TrimSpace(value) == value
}

// Append snapshots notes in owner-key order so retries see deterministic text.
func (n *Notices) Append(threadRef, prompt string) string {
	if n == nil {
		return prompt
	}
	n.mu.RLock()
	owners := n.byThread[threadRef]
	keys := make([]string, 0, len(owners))
	for owner := range owners {
		keys = append(keys, owner)
	}
	sort.Strings(keys)
	notes := make([]string, 0, len(keys))
	for _, owner := range keys {
		notes = append(notes, owners[owner])
	}
	n.mu.RUnlock()
	if len(notes) == 0 {
		return prompt
	}
	return prompt + "\n\n" + strings.Join(notes, "\n\n")
}

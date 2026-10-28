package hosted

import (
	"fmt"
	"time"

	"github.com/dkroshow/nano/internal/threads"
)

// NoteHumanPrompt persists the newest human-origin prompt time for a hosted
// thread (hosted_threads.last_human_prompt_at). Monotonic: an older time never
// overwrites a newer one. No-op without the control store.
func (s *ExecutionStore) NoteHumanPrompt(hostedThreadID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.sessions[hostedThreadID]
	if !ok {
		return fmt.Errorf("unknown hosted thread")
	}
	// Compared as times: RFC3339Nano trims trailing zeros, so as text ".2374Z"
	// sorts after the later ".237401Z". This store is the column's only writer
	// and loads it at start, so the record here decides.
	if rec.LastHumanPromptAt != "" && !threads.ParseTimestamp(rec.LastHumanPromptAt).Before(at) {
		return nil
	}
	stamp := at.UTC().Format(time.RFC3339Nano)
	if s.control != nil {
		if _, err := s.control.db.Exec(`UPDATE hosted_threads SET last_human_prompt_at=? WHERE hosted_thread_id=?`, stamp, hostedThreadID); err != nil {
			return err
		}
	}
	rec.LastHumanPromptAt = stamp
	s.sessions[hostedThreadID] = rec
	return nil
}

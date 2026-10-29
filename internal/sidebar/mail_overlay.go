package sidebar

import (
	"log"
)

// AttachMailOverlay stamps inbox counts on every catalog row with a hosted
// identity — one batched query per page, read-time only, absent when a row has
// no hosted thread or no mail.
func (s *Controller) AttachMailOverlay(threads []Thread) {
	if s.mail == nil || s.mailErr != "" {
		return
	}
	ids := make([]string, 0, len(threads))
	for i := range threads {
		if threads[i].HostedThreadID != "" {
			ids = append(ids, threads[i].HostedThreadID)
		}
	}
	if len(ids) == 0 {
		return
	}
	counts, err := s.mail.CountsFor(ids)
	if err != nil {
		log.Printf("catalog mail overlay: %v", err)
		return
	}
	for i := range threads {
		if value, ok := counts[threads[i].HostedThreadID]; ok {
			mail := value
			threads[i].Mail = &mail
		}
	}
}

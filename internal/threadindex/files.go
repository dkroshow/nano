package threadindex

import (
	"context"
)

// FindFileEvidenceBlobs returns bounded saved evidence for a referenced file.
func (s *Service) FindFileEvidenceBlobs(ctx context.Context, threadID, basename string, maxRows, maxBytes int) ([]string, error) {
	return s.store.FindFileEvidenceBlobs(ctx, threadID, basename, maxRows, maxBytes)
}

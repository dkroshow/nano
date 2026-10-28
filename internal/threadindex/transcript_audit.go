package threadindex

import (
	"context"
	"errors"

	"github.com/dkroshow/nano/internal/history"
)

// AuditScope selects the saved rows inspected by a transcript audit.
type AuditScope string

const AuditMessages AuditScope = "messages"

// AuditReferenceRow is one bounded saved row used by the audit comparator.
type AuditReferenceRow struct {
	ID               string `json:"id"`
	ProjectedSeq     int64  `json:"projectedSeq"`
	Category         string `json:"category"`
	Role             string `json:"role,omitempty"`
	Text             string `json:"text,omitempty"`
	ContentTruncated bool   `json:"contentTruncated"`
}

// AuditReference is a fixed saved window against which an audit compares observations.
type AuditReference struct {
	ProjectionKey   string              `json:"projectionKey"`
	SnapshotHeadSeq int64               `json:"snapshotHeadSeq"`
	StartSeq        int64               `json:"startSeq"`
	EndSeq          int64               `json:"endSeq"`
	HasMoreBefore   bool                `json:"hasMoreBefore"`
	CapReached      bool                `json:"capReached"`
	CapReason       string              `json:"capReason,omitempty"`
	Rows            []AuditReferenceRow `json:"rows"`
}

// LoadTranscriptAuditReference reads an audit’s fixed saved-transcript window.
func (s *Service) LoadTranscriptAuditReference(ctx context.Context, threadID, projectionKey string, snapshotHead, minSeq, maxSeq int64, scopes []AuditScope) (AuditReference, error) {
	hidden := make([]history.TranscriptCategory, 0, len(history.TranscriptCategories))
	for _, category := range history.TranscriptCategories {
		if category != history.CategoryMessages {
			hidden = append(hidden, category)
		}
	}
	before := int64(0)
	if maxSeq > 0 {
		if maxSeq > snapshotHead {
			maxSeq = snapshotHead
		}
		before = maxSeq + 1
	}
	page, err := s.store.ReadTranscript(ctx, history.TranscriptReadRequest{
		ThreadID: threadID, Limit: MaxAuditReference, MaxBytes: MaxThreadTranscriptBytes,
		HiddenCategories: hidden, ExpectedProjectionKey: projectionKey, SnapshotHeadSeq: snapshotHead, BeforeSeq: before,
	})
	if err != nil {
		return AuditReference{}, err
	}
	if page.Projection.Key != projectionKey || page.SnapshotHeadSeq != snapshotHead {
		return AuditReference{}, ErrAuditReferenceChanged
	}
	result := AuditReference{
		ProjectionKey: page.Projection.Key, SnapshotHeadSeq: page.SnapshotHeadSeq,
		HasMoreBefore: page.HasMoreBefore,
		Rows:          make([]AuditReferenceRow, 0, len(page.Items)),
	}
	for _, item := range page.Items {
		if minSeq > 0 && item.ProjectedSeq < minSeq {
			continue
		}
		result.Rows = append(result.Rows, AuditReferenceRow{
			ID: item.ID, ProjectedSeq: item.ProjectedSeq, Category: string(item.Category), Role: string(item.Role),
			Text: item.Text, ContentTruncated: item.ContentTruncated,
		})
	}
	if len(result.Rows) > 0 {
		result.StartSeq = result.Rows[0].ProjectedSeq
		result.EndSeq = result.Rows[len(result.Rows)-1].ProjectedSeq
	}
	result.CapReached = page.HasMoreBefore && (minSeq == 0 || page.StartSeq > minSeq)
	if result.CapReached {
		if len(page.Items) >= MaxAuditReference {
			result.CapReason = "rows"
		} else {
			result.CapReason = "bytes"
		}
	}
	return result, nil
}

const MaxAuditReference = 100

var ErrAuditReferenceChanged = errors.New("transcript audit reference changed")

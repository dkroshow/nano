package threadindex

import (
	"context"
	"errors"

	"github.com/dkroshow/nano/internal/history"
)

// LocatePinAnchor finds the saved message used to anchor a pin.
func (s *Service) LocatePinAnchor(ctx context.Context, threadID, itemID string) (history.PinMessage, error) {
	message, err := s.store.LocatePinAnchor(ctx, threadID, itemID)
	return message, mapPinCatalogError(err)
}

// ReadPinContext reads the saved turns surrounding a pinned message.
func (s *Service) ReadPinContext(ctx context.Context, threadID, itemID string, turns int) (history.PinContext, error) {
	result, err := s.store.ReadPinContext(ctx, threadID, itemID, turns)
	return result, mapPinCatalogError(err)
}

func mapPinCatalogError(err error) error {
	if errors.Is(err, history.ErrThreadNotFound) {
		return ErrCatalogThreadNotFound
	}
	return err
}

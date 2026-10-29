package store

import (
	"errors"
)

// ErrNativeClaimOwned reports a native identity already assigned to another hosted record.
var ErrNativeClaimOwned = errors.New("native identity is owned by another hosted record")

// SkipOwnedNativeClaim permits an existing owner while preserving other claim failures.
func SkipOwnedNativeClaim(err error) error {
	if errors.Is(err, ErrNativeClaimOwned) {
		return nil
	}
	return err
}

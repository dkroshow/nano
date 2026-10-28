package sessions

import (
	"crypto/rand"
	"encoding/hex"
)

// NewID creates an attachment ID from cryptographic randomness.
func NewID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

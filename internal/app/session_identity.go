package app

import (
	"crypto/rand"
	"encoding/hex"
)

func newSessionID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

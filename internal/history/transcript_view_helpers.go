package history

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	maxTranscriptInlineBytes = 8 << 10
	// MaxTranscriptBodyBytes is Nano's retained normalized-body cap.
	MaxTranscriptBodyBytes   = 1 << 20
	maxTranscriptDetailBytes = 8 << 10
)

func canonicalTranscriptItemID(provider Provider, nativeThreadID, sourceKey string) string {
	// Production callers always have a native id after catalog reconciliation.
	// A direct adapter probe may omit it; retain a deterministic non-generation
	// fallback so parser tests remain a pure function of their source fixture.
	if nativeThreadID == "" {
		nativeThreadID = "source:" + sourceKey
	}
	return opaqueID("nit_", string(provider)+":"+nativeThreadID+":"+sourceKey)
}

func boundTranscriptText(itemID, value string) (string, *TranscriptBody) {
	value = strings.TrimSpace(value)
	original := len(value)
	if original <= maxTranscriptInlineBytes {
		return value, nil
	}
	inline := truncateUTF8(value, maxTranscriptInlineBytes)
	stored := truncateUTF8(value, MaxTranscriptBodyBytes)
	ref := opaqueID("ncb_", itemID+":"+fmt.Sprint(original))
	return inline, &TranscriptBody{
		ItemID: itemID, ContentRef: ref, Text: stored,
		OriginalBytes: int64(original), StoredBytes: int64(len(stored)),
		Truncated: len(stored) < original,
	}
}

func truncateUTF8(value string, max int) string {
	if len(value) <= max {
		return value
	}
	value = value[:max]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value) + "…"
}

func safeDetail(value any) json.RawMessage {
	if value == nil {
		return json.RawMessage(`{}`)
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > maxTranscriptDetailBytes {
		return json.RawMessage(`{"truncated":true}`)
	}
	return encoded
}

func opaqueID(prefix, value string) string {
	sum := sha256.Sum256([]byte(value))
	return prefix + hex.EncodeToString(sum[:12])
}

func mintOpaqueID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("mint transcript projection key: %w", err)
	}
	return prefix + hex.EncodeToString(value[:]), nil
}

func int64Pointer(value int64) *int64 { return &value }

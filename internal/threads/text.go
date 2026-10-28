package threads

import (
	"strings"
	"time"
)

// TruncateText bounds a text value without splitting a UTF-8 code point.
func TruncateText(value string, cap int) string {
	if len(value) <= cap {
		return value
	}
	for cap > 0 && cap < len(value) && (value[cap]&0xc0) == 0x80 {
		cap--
	}
	return value[:cap]
}

// TimestampBefore orders two stored RFC3339Nano times as times. As text they
// misorder within a second: the format trims trailing zeros, so ".2374Z" sorts
// after the later ".237401Z".
func TimestampBefore(a, b string) bool { return ParseTimestamp(a).Before(ParseTimestamp(b)) }

// ParseTimestamp reads a stored RFC3339 time, returning zero for missing or malformed values.
func ParseTimestamp(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed
	}
	return time.Time{}
}

// FirstNonEmpty selects the first present scalar without changing its bytes.
func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

package wire

import "encoding/json"

// Int64 reads an integer or a floating-point JSON number as an integer.
func Int64(raw json.RawMessage) int64 {
	var value int64
	if len(raw) > 0 && json.Unmarshal(raw, &value) == nil {
		return value
	}
	var number float64
	if len(raw) > 0 && json.Unmarshal(raw, &number) == nil {
		return int64(number)
	}
	return 0
}

// String reads the first encoded JSON string.
func String(values ...json.RawMessage) string {
	for _, raw := range values {
		var value string
		if len(raw) > 0 && json.Unmarshal(raw, &value) == nil {
			return value
		}
	}
	return ""
}

// TokenString reads a string or a JSON number token.
func TokenString(raw json.RawMessage) string {
	if value := String(raw); value != "" {
		return value
	}
	var number json.Number
	if len(raw) > 0 && json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return ""
}

// Bool reads an encoded JSON boolean.
func Bool(raw json.RawMessage) bool {
	var value bool
	_ = json.Unmarshal(raw, &value)
	return value
}

package threads

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

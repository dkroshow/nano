package store

// FieldValue is one named canonical metadata value before provenance is attached.
type FieldValue struct {
	// Field names the metadata field.
	Field string
	// Value is its supplied string value.
	Value string
}

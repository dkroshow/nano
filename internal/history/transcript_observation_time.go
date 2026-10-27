package history

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Tuple slots: scope, reference, value, raw mode, raw, class, field, precision,
// meaning. Zero scope/reference inherit the containing observation's keys.
// Raw mode preserves absent (0), exactly the encoded value (1), or distinct (2).
// This is a private storage shape; the model still exposes named clock fields.
type storedObservationTime struct {
	storedTimeFact
	RawMode uint8
}

func (t storedObservationTime) MarshalJSON() ([]byte, error) {
	return json.Marshal([9]any{t.Scope, t.Ref, t.Value, t.RawMode, t.Raw, t.Class, t.Field, t.Precision, t.Meaning})
}
func (t *storedObservationTime) UnmarshalJSON(raw []byte) error {
	var slots []json.RawMessage
	if err := json.Unmarshal(raw, &slots); err != nil {
		return err
	}
	if len(slots) != 9 {
		return fmt.Errorf("invalid observation time tuple")
	}
	*t = storedObservationTime{}
	for i, value := range []any{&t.Scope, &t.Ref, &t.Value, &t.RawMode, &t.Raw, &t.Class, &t.Field, &t.Precision, &t.Meaning} {
		if err := json.Unmarshal(slots[i], value); err != nil {
			return err
		}
	}
	if t.Scope < 0 || t.Ref < 0 || t.RawMode > 2 {
		return fmt.Errorf("invalid observation time references or raw mode")
	}
	if t.RawMode < 2 {
		if !bytes.Equal(t.Raw, []byte("null")) {
			return fmt.Errorf("unexpected observation raw time")
		}
		t.Raw = nil
	}
	return nil
}

func compactObservationTimes(times []storedTimeFact, scope, key int64) ([]storedObservationTime, error) {
	var result []storedObservationTime
	for _, fact := range times {
		saved := storedObservationTime{storedTimeFact: fact}
		if saved.Scope == scope {
			saved.Scope = 0
		}
		if saved.Ref == key {
			saved.Ref = 0
		}
		if len(fact.Raw) != 0 {
			value, err := json.Marshal(fact.Value)
			if err != nil {
				return nil, err
			}
			saved.RawMode = 2
			if bytes.Equal(fact.Raw, value) {
				saved.RawMode, saved.Raw = 1, nil
			}
		}
		result = append(result, saved)
	}
	return result, nil
}
func expandObservationTimes(times []storedObservationTime, scope, key int64) ([]storedTimeFact, error) {
	var result []storedTimeFact
	for _, saved := range times {
		fact := saved.storedTimeFact
		if fact.Scope == 0 {
			fact.Scope = scope
		}
		if fact.Ref == 0 {
			fact.Ref = key
		}
		if saved.RawMode == 1 {
			var err error
			fact.Raw, err = json.Marshal(fact.Value)
			if err != nil {
				return nil, err
			}
		}
		result = append(result, fact)
	}
	return result, nil
}

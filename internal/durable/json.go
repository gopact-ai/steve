package durable

import (
	"bytes"
	"encoding/json"
)

// CanonicalJSON normalizes object key order without rounding large integers.
func CanonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

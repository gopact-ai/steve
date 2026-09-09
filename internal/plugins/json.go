package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

func ParseManifest(raw []byte) (Manifest, error) {
	var m Manifest
	if len(raw) > MaxManifestBytes {
		return m, fmt.Errorf("%w: manifest exceeds %d bytes", ErrInvalid, MaxManifestBytes)
	}
	if err := decodeStrict(raw, &m); err != nil {
		return m, err
	}
	if err := manifestShape().validate(raw); err != nil {
		return m, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return m, m.Validate()
}

func decodeStrict(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := uniqueObjectKeys(d, 0); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("%w: expected exactly one JSON value", ErrInvalid)
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

// Duplicate keys must not let a reviewer and the runtime see different
// declarations. The bounded token walk checks nested objects too.
func uniqueObjectKeys(d *json.Decoder, depth int) error {
	if depth > 64 {
		return fmt.Errorf("JSON nesting exceeds 64")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		if delimiter == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid object key %q", name)
			}
			seen[name] = true
		}
		if err := uniqueObjectKeys(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

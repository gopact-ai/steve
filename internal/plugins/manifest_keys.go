package plugins

import (
	"encoding/json"
	"fmt"
)

type jsonShape struct {
	fields  map[string]*jsonShape
	values  *jsonShape
	element *jsonShape
}

func objectShape(names ...string) *jsonShape {
	shape := &jsonShape{fields: map[string]*jsonShape{}}
	for _, name := range names {
		shape.fields[name] = nil
	}
	return shape
}

// encoding/json matches struct fields case-insensitively. A public manifest
// has one spelling for each field so an alternate spelling cannot override
// the value another reader would use.
func manifestShape() *jsonShape {
	value := objectShape("text", "config", "secret", "prefix")
	program := objectShape("path", "runtime", "args")
	program.fields["args"] = &jsonShape{element: value}
	server := objectShape("transport", "url", "headers", "program", "env")
	server.fields["url"] = value
	server.fields["headers"] = &jsonShape{values: value}
	server.fields["env"] = &jsonShape{values: value}
	server.fields["program"] = program
	agent := objectShape("harness", "model", "options", "system_prompt", "skills", "mcp_servers")
	manifest := objectShape("schema", "api", "id", "version", "description", "platforms", "settings", "skills", "mcp", "agents")
	manifest.fields["platforms"] = &jsonShape{element: objectShape("os", "arch")}
	manifest.fields["settings"] = &jsonShape{values: objectShape("description", "secret", "required", "default")}
	manifest.fields["mcp"] = &jsonShape{values: server}
	manifest.fields["agents"] = &jsonShape{values: agent}
	return manifest
}

func (shape *jsonShape) validate(raw json.RawMessage) error {
	if shape == nil {
		return nil
	}
	if shape.element != nil {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		for _, item := range items {
			if err := shape.element.validate(item); err != nil {
				return err
			}
		}
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, name := range sortedKeys(fields) {
		child := shape.values
		if shape.fields != nil {
			var ok bool
			child, ok = shape.fields[name]
			if !ok {
				return fmt.Errorf("%w: unknown field %q", ErrInvalid, name)
			}
		}
		if err := child.validate(fields[name]); err != nil {
			return err
		}
	}
	return nil
}

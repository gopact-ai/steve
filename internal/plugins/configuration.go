package plugins

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

// SecretRef identifies a version of a secret stored only on its execution
// node. Neither the value nor a value-derived digest crosses the node boundary.
type SecretRef struct {
	Name     string `json:"name"`
	Revision string `json:"revision"`
}

type Configuration struct {
	Values  map[string]string    `json:"values,omitempty"`
	Secrets map[string]SecretRef `json:"secrets,omitempty"`
}

// Installation is the owner's desired package, scopes and node configurations.
// Targets uses the existing node IDs; an empty target names a standalone hub.
type Installation struct {
	PackageID string                   `json:"package_id"`
	Digest    string                   `json:"digest"`
	Enabled   bool                     `json:"enabled"`
	Projects  []string                 `json:"projects"`
	Targets   map[string]Configuration `json:"targets"`
}

// Deployment is one installation resolved for one node. Its identity includes
// the exact package, public configuration, secret references and project scope.
type Deployment struct {
	Installation  string        `json:"installation"`
	PackageID     string        `json:"package_id"`
	Digest        string        `json:"digest"`
	Node          string        `json:"node"`
	Projects      []string      `json:"projects"`
	Configuration Configuration `json:"configuration"`
}

func (c Configuration) Clone() Configuration {
	return Configuration{Values: maps.Clone(c.Values), Secrets: maps.Clone(c.Secrets)}
}

func (i Installation) Deployment(id, node string) (Deployment, error) {
	c, ok := i.Targets[node]
	if !ok {
		return Deployment{}, fmt.Errorf("%w: installation has no configuration for this node", ErrUnavailable)
	}
	d := Deployment{Installation: id, PackageID: i.PackageID, Digest: i.Digest, Node: node, Projects: slices.Clone(i.Projects), Configuration: c.Clone()}
	slices.Sort(d.Projects)
	return d, d.Validate()
}

func (d Deployment) Validate() error {
	if !nameShape.MatchString(d.Installation) || !ValidID(d.PackageID) || !digestShape.MatchString(d.Digest) {
		return fmt.Errorf("%w: deployment identity", ErrInvalid)
	}
	if len(d.Node) > 128 || strings.ContainsAny(d.Node, "\x00\r\n") || !utf8.ValidString(d.Node) {
		return fmt.Errorf("%w: node identity", ErrInvalid)
	}
	if len(d.Projects) == 0 || len(d.Projects) > 64 || duplicate(d.Projects) {
		return fmt.Errorf("%w: deployment requires distinct projects", ErrInvalid)
	}
	for _, id := range d.Projects {
		if id == "" || len(id) > 256 || strings.ContainsAny(id, "\x00\r\n") || !utf8.ValidString(id) {
			return fmt.Errorf("%w: project identity", ErrInvalid)
		}
	}
	for _, key := range sortedKeys(d.Configuration.Values) {
		value := d.Configuration.Values[key]
		if !nameShape.MatchString(key) || len(value) > 16<<10 || strings.ContainsRune(value, '\x00') || !utf8.ValidString(value) {
			return fmt.Errorf("%w: public configuration value", ErrInvalid)
		}
	}
	for _, key := range sortedKeys(d.Configuration.Secrets) {
		ref := d.Configuration.Secrets[key]
		if !nameShape.MatchString(key) || !nameShape.MatchString(ref.Name) || !secretRevision(ref.Revision) {
			return fmt.Errorf("%w: secret reference", ErrInvalid)
		}
	}
	return nil
}

func (d Deployment) Hash() (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	d.Projects = slices.Clone(d.Projects)
	slices.Sort(d.Projects)
	raw, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	if len(raw) > MaxManifestBytes/2 {
		return "", fmt.Errorf("%w: deployment configuration is too large", ErrInvalid)
	}
	return contentDigest(raw), nil
}

func (m Manifest) CheckConfiguration(c Configuration) error {
	for _, key := range sortedKeys(c.Values) {
		setting, ok := m.Settings[key]
		if !ok || setting.Secret {
			return fmt.Errorf("%w: %s is not a public setting", ErrInvalid, key)
		}
	}
	for _, key := range sortedKeys(c.Secrets) {
		setting, ok := m.Settings[key]
		if !ok || !setting.Secret {
			return fmt.Errorf("%w: %s is not a secret setting", ErrInvalid, key)
		}
	}
	for _, key := range sortedKeys(m.Settings) {
		setting := m.Settings[key]
		if !setting.Required {
			continue
		}
		if setting.Secret {
			if _, ok := c.Secrets[key]; !ok {
				return fmt.Errorf("%w: required secret %s is not configured", ErrUnavailable, key)
			}
		} else {
			if value, ok := c.Values[key]; (!ok && setting.Default == nil) || (ok && value == "") {
				return fmt.Errorf("%w: required setting %s is not configured", ErrUnavailable, key)
			}
		}
	}
	return nil
}

func (c Configuration) value(m Manifest, v Value, secret func(SecretRef) (string, error)) (string, error) {
	if v.Config != "" {
		value, ok := c.Values[v.Config]
		if !ok {
			if fallback := m.Settings[v.Config].Default; fallback != nil {
				value = *fallback
			}
		}
		return v.Prefix + value, nil
	}
	if v.Secret != "" {
		ref, ok := c.Secrets[v.Secret]
		if !ok {
			return "", fmt.Errorf("%w: secret %s has no reference", ErrUnavailable, v.Secret)
		}
		value, err := secret(ref)
		if err != nil {
			return "", fmt.Errorf("%w: secret %s is not available on this node", ErrUnavailable, v.Secret)
		}
		return v.Prefix + value, nil
	}
	return v.Text, nil
}

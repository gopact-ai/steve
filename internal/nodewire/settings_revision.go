package nodewire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
)

const SettingsRevisionConflictCode = "settings_revision_conflict"

var ErrSettingsRevisionConflict = errors.New("node settings changed; reload before saving")
var ErrSettingsRevisionUnsupported = errors.New("node does not support settings revision checks; upgrade it before editing")

func SettingsRevision(settings Settings) string {
	settings.Revision = ""
	raw, _ := json.Marshal(settings)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// CloneSettings gives each settings response ownership of mutable values.
func CloneSettings(s Settings) Settings {
	s.Tools = slices.Clone(s.Tools)
	s.Declares = slices.Clone(s.Declares)
	s.Capabilities = slices.Clone(s.Capabilities)
	s.Harnesses = maps.Clone(s.Harnesses)
	for id, h := range s.Harnesses {
		h.Args = slices.Clone(h.Args)
		h.Env = slices.Clone(h.Env)
		h.Models = slices.Clone(h.Models)
		if h.Adapter != nil {
			v := *h.Adapter
			h.Adapter = &v
		}
		if h.Slots != nil {
			v := *h.Slots
			h.Slots = &v
		}
		if h.Permission != nil {
			v := *h.Permission
			h.Permission = &v
		}
		s.Harnesses[id] = h
	}
	s.MCPServers = maps.Clone(s.MCPServers)
	for id, m := range s.MCPServers {
		m.Args = slices.Clone(m.Args)
		m.Env = maps.Clone(m.Env)
		m.Headers = maps.Clone(m.Headers)
		s.MCPServers[id] = m
	}
	return s
}

package consoleapi

import (
	"context"
	"encoding/json"
	"errors"
)

var ErrSettingsConflict = errors.New("settings revision conflict")

type SettingsField struct {
	Default   json.RawMessage `json:"default,omitempty"`
	Path      string          `json:"path"`
	Type      string          `json:"type"`
	Unit      string          `json:"unit,omitempty"`
	Minimum   *int64          `json:"minimum,omitempty"`
	Maximum   *int64          `json:"maximum,omitempty"`
	Enum      []string        `json:"enum,omitempty"`
	ApplyMode string          `json:"apply_mode"`
}
type SettingsView struct {
	Revision       string          `json:"revision"`
	Desired        json.RawMessage `json:"desired"`
	Effective      json.RawMessage `json:"effective"`
	PendingRestart bool            `json:"pending_restart"`
	ApplyMode      string          `json:"apply_mode"`
	Fields         []SettingsField `json:"fields"`
	Warning        string          `json:"warning,omitempty"`
}
type SettingsUpdate struct {
	BaseRevision string          `json:"base_revision"`
	Settings     json.RawMessage `json:"settings"`
}
type SettingsService interface {
	Settings(context.Context) (SettingsView, error)
	UpdateSettings(context.Context, SettingsUpdate) (SettingsView, error)
}

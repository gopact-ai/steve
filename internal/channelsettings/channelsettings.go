// Package channelsettings holds the channel settings the console reads and
// patches. The configuration renders and applies them; the console API
// carries them. Neither needs the other to share these values.
package channelsettings

// Settings contains only values safe to return to the console.
// Credentials never appear in either desired or effective settings.
type Settings struct {
	DefaultChannel string         `json:"default_channel"`
	Console        ConsoleSetting `json:"console"`
	Feishu         FeishuSetting  `json:"feishu"`
}

type ConsoleSetting struct {
	Enabled bool   `json:"enabled"`
	OwnerID string `json:"owner_id"`
}

type FeishuSetting struct {
	Enabled             bool     `json:"enabled"`
	AppID               string   `json:"app_id"`
	AppSecretConfigured bool     `json:"app_secret_configured"`
	Domain              string   `json:"domain"`
	OwnerOpenID         string   `json:"owner_open_id"`
	GroupPolicy         string   `json:"group_policy"`
	AllowUnmentioned    bool     `json:"allow_unmentioned"`
	AllowedSenders      []string `json:"allowed_senders"`
	BlockedSenders      []string `json:"blocked_senders"`
}

// Patch intentionally excludes console identity and listener settings.
// Their changes require deployment configuration rather than an IM edit.
type Patch struct {
	DefaultChannel *string      `json:"default_channel,omitempty"`
	Feishu         *FeishuPatch `json:"feishu,omitempty"`
}

type FeishuPatch struct {
	Enabled          *bool     `json:"enabled,omitempty"`
	AppID            *string   `json:"app_id,omitempty"`
	AppSecret        *Secret   `json:"app_secret,omitempty"`
	Domain           *string   `json:"domain,omitempty"`
	OwnerOpenID      *string   `json:"owner_open_id,omitempty"`
	GroupPolicy      *string   `json:"group_policy,omitempty"`
	AllowUnmentioned *bool     `json:"allow_unmentioned,omitempty"`
	AllowedSenders   *[]string `json:"allowed_senders,omitempty"`
	BlockedSenders   *[]string `json:"blocked_senders,omitempty"`
}

// Secret is write-only: omission preserves the current value, replace
// requires a nonempty value, and clear must not include a value.
type Secret struct {
	Action string  `json:"action"`
	Value  *string `json:"value,omitempty"`
}

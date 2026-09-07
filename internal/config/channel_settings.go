package config

import (
	"fmt"
	"slices"
	"strings"
)

// ChannelSettings contains only values safe to return to the console.
// Credentials never appear in either desired or effective settings.
type ChannelSettings struct {
	DefaultChannel string                `json:"default_channel"`
	Console        ConsoleChannelSetting `json:"console"`
	Feishu         FeishuChannelSetting  `json:"feishu"`
}

type ConsoleChannelSetting struct {
	Enabled bool   `json:"enabled"`
	OwnerID string `json:"owner_id"`
}

type FeishuChannelSetting struct {
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

// ChannelPatch intentionally excludes console identity and listener settings.
// Their changes require deployment configuration rather than an IM edit.
type ChannelPatch struct {
	DefaultChannel *string             `json:"default_channel,omitempty"`
	Feishu         *FeishuChannelPatch `json:"feishu,omitempty"`
}

type FeishuChannelPatch struct {
	Enabled          *bool          `json:"enabled,omitempty"`
	AppID            *string        `json:"app_id,omitempty"`
	AppSecret        *ChannelSecret `json:"app_secret,omitempty"`
	Domain           *string        `json:"domain,omitempty"`
	OwnerOpenID      *string        `json:"owner_open_id,omitempty"`
	GroupPolicy      *string        `json:"group_policy,omitempty"`
	AllowUnmentioned *bool          `json:"allow_unmentioned,omitempty"`
	AllowedSenders   *[]string      `json:"allowed_senders,omitempty"`
	BlockedSenders   *[]string      `json:"blocked_senders,omitempty"`
}

// ChannelSecret is write-only: omission preserves the current value, replace
// requires a nonempty value, and clear must not include a value.
type ChannelSecret struct {
	Action string  `json:"action"`
	Value  *string `json:"value,omitempty"`
}

func (c *Config) ChannelSettings() ChannelSettings {
	f := c.Feishu
	f.applyDefaults()
	defaultChannel := c.Gateway.DefaultChannel
	if defaultChannel == "" {
		defaultChannel = "console"
		if c.FeishuEnabled() {
			defaultChannel = "feishu"
		}
	}
	return ChannelSettings{
		DefaultChannel: defaultChannel,
		Console:        ConsoleChannelSetting{Enabled: true, OwnerID: c.EffectiveOwnerID()},
		Feishu: FeishuChannelSetting{
			Enabled: c.FeishuEnabled(), AppID: f.AppID, AppSecretConfigured: f.AppSecret != "",
			Domain: f.Domain, OwnerOpenID: f.OwnerOpenID, GroupPolicy: f.GroupPolicy,
			AllowUnmentioned: f.AllowUnmentioned,
			AllowedSenders:   append([]string{}, f.AllowedSenders...),
			BlockedSenders:   append([]string{}, f.BlockedSenders...),
		},
	}
}

func (c *Config) PatchChannels(patch ChannelPatch) (*Config, error) {
	next := *c
	next.Feishu.AllowedSenders = slices.Clone(c.Feishu.AllowedSenders)
	next.Feishu.BlockedSenders = slices.Clone(c.Feishu.BlockedSenders)
	// Pin the current console identity before touching the IM owner. An IM
	// edit cannot grant console ownership to somebody else or remove it.
	next.Gateway.OwnerID = strings.TrimSpace(c.EffectiveOwnerID())
	if next.Gateway.OwnerID == "" {
		return nil, fmt.Errorf("configure gateway.owner_id before changing channels")
	}
	// Choosing another IM endpoint does not change the workspace language.
	if next.Gateway.Locale == "" {
		next.Gateway.Locale = c.EffectiveLocale()
	}
	next.Gateway.DefaultChannel = c.ChannelSettings().DefaultChannel
	if patch.DefaultChannel != nil {
		next.Gateway.DefaultChannel = strings.TrimSpace(*patch.DefaultChannel)
		if next.Gateway.DefaultChannel != "console" && next.Gateway.DefaultChannel != "feishu" {
			return nil, fmt.Errorf("default_channel must be console or feishu")
		}
	}
	enabled := c.FeishuEnabled()
	f := &next.Feishu
	f.applyDefaults()
	if change := patch.Feishu; change != nil {
		if change.Enabled != nil {
			enabled = *change.Enabled
		}
		for _, item := range []struct{ to, from *string }{
			{&f.AppID, change.AppID}, {&f.Domain, change.Domain},
			{&f.OwnerOpenID, change.OwnerOpenID}, {&f.GroupPolicy, change.GroupPolicy},
		} {
			if item.from != nil {
				*item.to = strings.TrimSpace(*item.from)
			}
		}
		if change.AppSecret != nil {
			switch change.AppSecret.Action {
			case "replace":
				if change.AppSecret.Value == nil || strings.TrimSpace(*change.AppSecret.Value) == "" {
					return nil, fmt.Errorf("app_secret replace requires a nonempty value")
				}
				f.AppSecret = *change.AppSecret.Value
			case "clear":
				if change.AppSecret.Value != nil {
					return nil, fmt.Errorf("app_secret clear must omit value")
				}
				f.AppSecret = ""
			default:
				return nil, fmt.Errorf("app_secret action must be replace or clear")
			}
		}
		if change.AllowUnmentioned != nil {
			f.AllowUnmentioned = *change.AllowUnmentioned
		}
		if change.AllowedSenders != nil {
			f.AllowedSenders = slices.Clone(*change.AllowedSenders)
		}
		if change.BlockedSenders != nil {
			f.BlockedSenders = slices.Clone(*change.BlockedSenders)
		}
	}
	// Preserve the former adapter's restriction when an unrelated edit
	// leaves an existing open+allowlist policy unspecified.
	if c.Feishu.Enabled == nil && (patch.Feishu == nil || patch.Feishu.GroupPolicy == nil) && (c.Feishu.GroupPolicy == "" || c.Feishu.GroupPolicy == GroupPolicyOpen) && len(c.Feishu.AllowedSenders) > 0 {
		f.GroupPolicy = GroupPolicyAllowlist
	}
	f.Enabled = &enabled
	for _, ids := range [][]string{f.AllowedSenders, f.BlockedSenders} {
		for i := range ids {
			ids[i] = strings.TrimSpace(ids[i])
		}
	}
	if err := next.ValidateChannels(); err != nil {
		return nil, err
	}
	return &next, nil
}

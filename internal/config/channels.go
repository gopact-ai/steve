package config

import (
	"fmt"
	"net/url"
	"strings"
)

func (c *Config) FeishuEnabled() bool {
	if c.Feishu.Enabled != nil {
		return *c.Feishu.Enabled
	}
	return strings.TrimSpace(c.Feishu.AppID) != "" && strings.TrimSpace(c.Feishu.AppSecret) != ""
}

func (c *Config) ValidateChannels() error {
	id, secret := strings.TrimSpace(c.Feishu.AppID), strings.TrimSpace(c.Feishu.AppSecret)
	if c.Feishu.Enabled == nil && (id == "") != (secret == "") {
		return fmt.Errorf("feishu.app_id and feishu.app_secret must be configured together")
	}
	if c.FeishuEnabled() {
		if id == "" || secret == "" {
			return fmt.Errorf("enabled Feishu requires app_id and app_secret")
		}
		if err := c.Feishu.Validate(); err != nil {
			return err
		}
	} else {
		if err := c.Feishu.validateOptions(); err != nil {
			return err
		}
		if strings.TrimSpace(c.EffectiveOwnerID()) == "" {
			return fmt.Errorf("gateway.owner_id is required for a console-only hub")
		}
	}
	if c.Gateway.DefaultChannel != "" && c.Gateway.DefaultChannel != "console" && c.Gateway.DefaultChannel != "feishu" {
		return fmt.Errorf("gateway.default_channel must name a configured channel")
	}
	if c.Gateway.DefaultChannel == "feishu" && !c.FeishuEnabled() {
		return fmt.Errorf("gateway.default_channel is feishu but Feishu is disabled")
	}
	for id, peer := range c.Gateway.Peers {
		if strings.TrimSpace(id) == "" || id == c.Gateway.HubID {
			return fmt.Errorf("gateway.peers needs distinct nonempty hub IDs")
		}
		u, err := url.Parse(peer.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("gateway.peers.%s.url must be an http(s) address without userinfo, query or fragment", id)
		}
		if strings.TrimSpace(peer.Token) == "" {
			return fmt.Errorf("gateway.peers.%s.token is required", id)
		}
	}
	return nil
}

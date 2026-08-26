package feishu

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gopact-ai/steve/internal/config"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

type Identity struct {
	OpenID string
	Name   string
}

func BaseURL(domain string) string {
	if domain == config.DomainLark {
		return lark.LarkBaseUrl
	}
	return lark.FeishuBaseUrl
}

func newAPI(appID, appSecret, domain string) *lark.Client {
	return lark.NewClient(appID, appSecret, lark.WithOpenBaseUrl(BaseURL(domain)))
}

func Probe(ctx context.Context, appID, appSecret, domain string) (Identity, error) {
	api := newAPI(appID, appSecret, domain)
	resp, err := api.GetTenantAccessTokenBySelfBuiltApp(ctx, &larkcore.SelfBuiltTenantAccessTokenReq{
		AppID: appID, AppSecret: appSecret,
	})
	if err != nil {
		return Identity{}, fmt.Errorf("authenticate feishu app: %w", err)
	}
	if !resp.Success() {
		return Identity{}, fmt.Errorf("authenticate feishu app: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return botIdentity(ctx, api)
}

func Check(ctx context.Context, appID, appSecret, domain string) error {
	_, err := Probe(ctx, appID, appSecret, domain)
	return err
}

func OwnerOpenID(ctx context.Context, appID, appSecret, domain string) (string, error) {
	api := newAPI(appID, appSecret, domain)
	resp, err := api.Get(ctx, "/open-apis/application/v6/applications/"+appID+"?user_id_type=open_id", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return "", fmt.Errorf("get feishu app owner: %w", err)
	}
	if resp == nil || resp.StatusCode != 200 {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return "", fmt.Errorf("get feishu app owner: http %d", status)
	}
	owner, err := parseOwnerOpenID(resp.RawBody)
	if err != nil {
		return "", err
	}
	if owner == "" {
		return "", fmt.Errorf("get feishu app owner: empty owner")
	}
	return owner, nil
}

func botIdentity(ctx context.Context, api *lark.Client) (Identity, error) {
	resp, err := api.Get(ctx, "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return Identity{}, fmt.Errorf("get feishu bot identity: %w", err)
	}
	if resp == nil || resp.StatusCode != 200 {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return Identity{}, fmt.Errorf("get feishu bot identity: http %d", status)
	}
	identity, err := parseBotIdentity(resp.RawBody)
	if err != nil {
		return Identity{}, err
	}
	if identity.OpenID == "" {
		return Identity{}, fmt.Errorf("get feishu bot identity: empty open id")
	}
	return identity, nil
}

func parseBotIdentity(body []byte) (Identity, error) {
	var result struct {
		Code int `json:"code"`
		Bot  struct {
			OpenID  string `json:"open_id"`
			AppName string `json:"app_name"`
		} `json:"bot"`
		Data struct {
			Bot struct {
				OpenID  string `json:"open_id"`
				AppName string `json:"app_name"`
			} `json:"bot"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return Identity{}, fmt.Errorf("parse feishu bot identity: %w", err)
	}
	if result.Code != 0 {
		return Identity{}, fmt.Errorf("get feishu bot identity: code=%d", result.Code)
	}
	identity := Identity{OpenID: result.Bot.OpenID, Name: result.Bot.AppName}
	if identity.OpenID == "" {
		identity.OpenID = result.Data.Bot.OpenID
		identity.Name = result.Data.Bot.AppName
	}
	return identity, nil
}

func parseOwnerOpenID(body []byte) (string, error) {
	var result struct {
		Code int `json:"code"`
		Data struct {
			App struct {
				CreatorID string `json:"creator_id"`
				Owner     struct {
					OwnerID   string `json:"owner_id"`
					OwnerType int    `json:"owner_type"`
					Type      int    `json:"type"`
				} `json:"owner"`
			} `json:"app"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse feishu app owner: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("get feishu app owner: code=%d", result.Code)
	}
	ownerType := result.Data.App.Owner.OwnerType
	if ownerType == 0 {
		ownerType = result.Data.App.Owner.Type
	}
	if ownerType == 2 && result.Data.App.Owner.OwnerID != "" {
		return result.Data.App.Owner.OwnerID, nil
	}
	if result.Data.App.CreatorID != "" {
		return result.Data.App.CreatorID, nil
	}
	return result.Data.App.Owner.OwnerID, nil
}

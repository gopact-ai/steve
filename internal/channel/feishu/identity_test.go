package feishu

import (
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func TestParseBotIdentity(t *testing.T) {
	identity, err := parseBotIdentity([]byte(`{"code":0,"bot":{"open_id":"ou_bot","app_name":"Steve"}}`))
	if err != nil || identity.OpenID != "ou_bot" || identity.Name != "Steve" {
		t.Fatalf("parseBotIdentity = %#v, %v", identity, err)
	}
	nested, err := parseBotIdentity([]byte(`{"code":0,"data":{"bot":{"open_id":"ou_nested","app_name":"Nested"}}}`))
	if err != nil || nested.OpenID != "ou_nested" {
		t.Fatalf("nested parseBotIdentity = %#v, %v", nested, err)
	}
	if _, err := parseBotIdentity([]byte(`{"code":1}`)); err == nil {
		t.Fatal("expected code error")
	}
}

func TestParseOwnerOpenID(t *testing.T) {
	owner, err := parseOwnerOpenID([]byte(`{"code":0,"data":{"app":{"owner":{"owner_id":"ou_owner","owner_type":2}}}}`))
	if err != nil || owner != "ou_owner" {
		t.Fatalf("owner = %q, %v", owner, err)
	}
	creator, err := parseOwnerOpenID([]byte(`{"code":0,"data":{"app":{"creator_id":"ou_creator","owner":{"owner_id":"ou_tenant","owner_type":1}}}}`))
	if err != nil || creator != "ou_creator" {
		t.Fatalf("creator fallback = %q, %v", creator, err)
	}
}

func TestBaseURL(t *testing.T) {
	if got := BaseURL(config.DomainLark); got != lark.LarkBaseUrl {
		t.Fatalf("lark url = %q", got)
	}
	if got := BaseURL(config.DomainFeishu); got != lark.FeishuBaseUrl {
		t.Fatalf("feishu url = %q", got)
	}
}

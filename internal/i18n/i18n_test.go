package i18n

import (
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/protocol"
)

func TestFromLang(t *testing.T) {
	tests := []struct {
		name string
		lang string
		want Locale
	}{
		{name: "english us", lang: "en_US.UTF-8", want: LocaleEN},
		{name: "english short", lang: "en", want: LocaleEN},
		{name: "english dash", lang: "en-GB", want: LocaleEN},
		{name: "chinese", lang: "zh_CN.UTF-8", want: LocaleZH},
		{name: "empty", lang: "", want: LocaleZH},
		{name: "c locale", lang: "C", want: LocaleZH},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FromLang(tt.lang); got != tt.want {
				t.Fatalf("FromLang(%q)=%q want %q", tt.lang, got, tt.want)
			}
		})
	}
}

func TestFromDomain(t *testing.T) {
	if FromDomain(config.DomainLark) != LocaleEN {
		t.Fatal("lark should be english")
	}
	if FromDomain(config.DomainFeishu) != LocaleZH {
		t.Fatal("feishu should be chinese")
	}
	if FromDomain("") != LocaleZH {
		t.Fatal("empty domain should default to chinese")
	}
}

func TestCatalogLocales(t *testing.T) {
	zhText := New(LocaleZH).T(Switched, "claude")
	enText := New(LocaleEN).T(Switched, "claude")
	if !strings.Contains(zhText, "已切换到 claude") {
		t.Fatalf("zh = %q", zhText)
	}
	if !strings.Contains(enText, "Switched to claude") {
		t.Fatalf("en = %q", enText)
	}
	if zhText == enText {
		t.Fatal("locales produced the same string")
	}
	drift := New(LocaleZH).T(CapabilityDrift, protocol.CommandNew)
	if !strings.Contains(drift, string(protocol.CommandNew)) {
		t.Fatalf("drift missing command: %q", drift)
	}
}

func TestCatalogUnknownKey(t *testing.T) {
	if got := New(LocaleEN).T(Key("missing_key")); got != "missing_key" {
		t.Fatalf("got %q", got)
	}
}

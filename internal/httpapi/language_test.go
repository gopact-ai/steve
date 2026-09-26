package httpapi

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/readmodel"
)

// A request that names no language is answered in the Hub's; one that
// names a language Steve speaks is answered in that, and a Hub whose
// language changes answers in the new one.
func TestARequestWithoutALanguageIsAnsweredInTheHubLanguage(t *testing.T) {
	var hub atomic.Value
	hub.Store(i18n.LocaleEN)
	server := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: testToken, Text: i18n.Dynamic(func() i18n.Locale { return hub.Load().(i18n.Locale) })})
	english := i18n.New(i18n.LocaleEN).T(i18n.HTTPCoordinationOff)
	chinese := i18n.New(i18n.LocaleZH).T(i18n.HTTPCoordinationOff)
	for _, tc := range []struct {
		name, header string
		hub          i18n.Locale
		want         string
	}{
		{"no header on an english hub", "", i18n.LocaleEN, english},
		{"an unspoken language on an english hub", "fr", i18n.LocaleEN, english},
		{"chinese asked of an english hub", "zh-CN", i18n.LocaleEN, chinese},
		{"no header after the hub turns chinese", "", i18n.LocaleZH, chinese},
	} {
		hub.Store(tc.hub)
		req, err := http.NewRequest(http.MethodPost, server.URL()+"/console/coordination/transfer", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		if tc.header != "" {
			req.Header.Set("Accept-Language", tc.header)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if !strings.Contains(string(body), tc.want) {
			t.Errorf("%s: %s, want %q", tc.name, body, tc.want)
		}
	}
}

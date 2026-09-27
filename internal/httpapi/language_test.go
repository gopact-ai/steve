package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/sshconnect"
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

// A request that names no language keeps the one the front it came
// through already chose; a language the request names still wins.
func TestARequestKeepsTheLanguageItsFrontChose(t *testing.T) {
	token := strings.Repeat("t", 40)
	seen := make(chan i18n.Locale, 1)
	handler, err := SSHHandler(localeSSH{seen: seen}, token, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, header string
		want         i18n.Locale
	}{
		{"no header", "", i18n.LocaleEN},
		{"chinese named", "zh-CN", i18n.LocaleZH},
	} {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1/console/ssh/candidates", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		if tc.header != "" {
			request.Header.Set("Accept-Language", tc.header)
		}
		request = request.WithContext(i18n.WithLocale(request.Context(), i18n.LocaleEN))
		handler.ServeHTTP(httptest.NewRecorder(), request)
		if got := <-seen; got != tc.want {
			t.Errorf("%s: the service saw %q, want %q", tc.name, got, tc.want)
		}
	}
}

type localeSSH struct {
	SSHService
	seen chan i18n.Locale
}

func (s localeSSH) SSHDiscover(ctx context.Context) (sshconnect.Discovery, error) {
	s.seen <- i18n.ContextLocale(ctx)
	return sshconnect.Discovery{}, nil
}

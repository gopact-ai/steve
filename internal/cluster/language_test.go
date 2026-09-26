package cluster

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/i18n"
)

func hasHan(text string) bool {
	return strings.IndexFunc(text, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}

// This machine's own console answers in the language the request names.
func TestTheLocalConsoleAnswersInTheLanguageTheRequestNames(t *testing.T) {
	peer := &Peer{UIToken: "isolated-ui-token", closing: true}
	for _, tc := range []struct {
		header  string
		english bool
	}{{"en-US,en;q=0.9", true}, {"zh-CN", false}} {
		request := httptest.NewRequest(http.MethodGet, "/console/ssh/candidates", nil)
		request.Host = "127.0.0.1:7710"
		request.Header.Set("Authorization", "Bearer "+peer.UIToken)
		request.Header.Set("Accept-Language", tc.header)
		response := httptest.NewRecorder()
		peer.serveUI(response, request)
		if response.Code != http.StatusServiceUnavailable || hasHan(response.Body.String()) == tc.english {
			t.Errorf("%s: %d %q, want a refusal in the language it named", tc.header, response.Code, response.Body.String())
		}
	}
}

// The coordination page explains a node it cannot count as ready in the
// reader's language.
func TestCoordinationReasonsAreInTheReadersLanguage(t *testing.T) {
	state := coordination.State{Members: map[string]coordination.Member{"local": {NodeID: "local"}}, Voters: map[string]string{"local": "local"}}
	for _, locale := range []i18n.Locale{i18n.LocaleEN, i18n.LocaleZH} {
		nodes := coordinationNodes(i18n.WithLocale(t.Context(), locale), state, "local", Status{}, nil)
		if len(nodes) != 1 || nodes[0].Reason == "" || hasHan(nodes[0].Reason) == (locale == i18n.LocaleEN) {
			t.Errorf("%s: %+v, want the reason in that language", locale, nodes)
		}
	}
}

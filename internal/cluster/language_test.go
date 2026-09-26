package cluster

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// A plan reviewed in one language and registered in another is one plan:
// what it will do is said in the reader's language, and only the request
// it was made from identifies it.
func TestAPlanReadInTwoLanguagesIsOnePlan(t *testing.T) {
	plan := PeerEnrollmentPlan{Request: PeerEnrollmentRequest{Name: "remote", PeerAddress: "192.0.2.10:7711", RaftAddress: "192.0.2.10:7712"}, ClusterID: "test-cluster", Effects: []string{"在目标机启动持久节点：HTTPS 192.0.2.10:7711，共识 192.0.2.10:7712"}}
	read := plan
	read.Effects = []string{"Start a persistent node on the target machine: HTTPS 192.0.2.10:7711, consensus 192.0.2.10:7712"}
	if plan.reviewHash() != read.reviewHash() {
		t.Error("the same plan read in two languages has two review IDs")
	}
	moved := plan
	moved.Request.PeerAddress = "192.0.2.11:7711"
	if plan.reviewHash() == moved.reviewHash() {
		t.Error("a plan for another address has the same review ID")
	}
}

// A machine being enrolled has no configuration of its own yet; it
// refuses a package in the language of the Hub that enrolls it.
func TestAnImportedMachineRefusesInTheHubsLanguage(t *testing.T) {
	for _, locale := range []i18n.Locale{i18n.LocaleEN, i18n.LocaleZH} {
		data, err := json.Marshal(map[string]any{"version": 1, "operation_id": "operation", "cluster_id": "test-cluster", "node_id": "node-new", "workspace_dir": "~/steve-workspace", "owner_token": strings.Repeat("o", 40), "worker_token": strings.Repeat("w", 40), "seeds": []any{map[string]any{}}, "storage_level": "internal", "locale": locale})
		if err != nil {
			t.Fatal(err)
		}
		_, err = ImportPeerPackage(data, filepath.Join(t.TempDir(), "peer"))
		if err == nil || hasHan(err.Error()) == (locale == i18n.LocaleEN) || locale == i18n.LocaleEN && !strings.Contains(err.Error(), "ledger") {
			t.Errorf("%s: %v, want the refusal of a package without a ledger grant in that language", locale, err)
		}
	}
}

// Content repair tells the owner what it found in the Hub's language, and
// says one finding once however many rounds find it again.
func TestContentRepairSpeaksTheHubsLanguageOnce(t *testing.T) {
	var said []string
	worker := &contentRepairWorker{peer: &Peer{text: i18n.New(i18n.LocaleEN)}, active: Activation{Context: t.Context()}, previous: map[string]string{}, observe: func(kind, _, text string, _ map[string]string) {
		said = append(said, kind+": "+text)
	}}
	for range 2 {
		worker.notice(t.Context(), "object", "degraded", i18n.ClusterContentShort, "project", "content")
	}
	worker.notice(t.Context(), "object", "healthy", "")
	if len(said) != 2 || hasHan(said[0]) || hasHan(said[1]) || !strings.HasPrefix(said[1], "content.recovered: ") {
		t.Errorf("repair said %q, want one English finding and then its recovery", said)
	}
}

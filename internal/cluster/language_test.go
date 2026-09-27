package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"unicode"

	"github.com/gopact-ai/steve/internal/cluster/clustertest"
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

// A plan previewed in one language is registered from another: the
// reader's language changes how its effects read, not which plan it is.
func TestAPlanPreviewedInOneLanguageRegistersInAnother(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var starts atomic.Int32
	options.Activate = testPeerApplication(t, &starts)
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	ports := clustertest.HoldEnrollmentPorts(t)
	request := PeerEnrollmentRequest{Name: "box", PeerAddress: ports.Peer, RaftAddress: ports.Raft, Level: "restricted"}
	chinese, err := peer.PreviewEnrollment(i18n.WithLocale(t.Context(), i18n.LocaleZH), request, true)
	if err != nil {
		t.Fatal(err)
	}
	english, err := peer.PreviewEnrollment(i18n.WithLocale(t.Context(), i18n.LocaleEN), request, true)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Equal(chinese.Effects, english.Effects) || chinese.ReviewID != english.ReviewID {
		t.Fatalf("the plan read in Chinese (%s) and in English (%s) should differ only in how its effects read", chinese.ReviewID, english.ReviewID)
	}
	request = chinese.Request
	request.ExpectedPlanHash = chinese.ReviewID
	if _, err := peer.PrepareEnrollment(i18n.WithLocale(t.Context(), i18n.LocaleEN), request, "read-in-chinese", true); err != nil {
		t.Fatalf("a plan reviewed in Chinese was refused when registered in English: %v", err)
	}
}

// Every fact a plan is made of identifies it: changing any one gives the
// plan another review ID, so an approval cannot carry over to a plan that
// does something else. Only the ID itself, the ID the request expects and
// the effect sentences, which say the request in the reader's words, are
// left out.
func TestEveryFactOfAPlanIsReviewed(t *testing.T) {
	plan := PeerEnrollmentPlan{
		Request: PeerEnrollmentRequest{Alias: "box", Name: "remote", PeerAddress: "192.0.2.10:7711", RaftAddress: "192.0.2.10:7712", Level: "restricted",
			HubRoute: coordination.Route{Raft: "127.0.0.1:47001", API: "127.0.0.1:47002"}, WorkspaceDir: "~/steve-workspace", ExpectedPlanHash: "expected"},
		ClusterID: "test-cluster",
		Seeds: []coordination.Member{{NodeID: "node-a", Address: "192.0.2.1:7712", APIAddress: "https://192.0.2.1:7711", Name: "a", AutoEligible: true,
			FailureDomain: "domain-a", StorageLevel: "restricted", Voting: true}},
		Effects:  []string{"Start a persistent node on the target machine"},
		ReviewID: "reviewed",
	}
	// A field left empty here would be left out of the walk below.
	for _, value := range []reflect.Value{reflect.ValueOf(plan), reflect.ValueOf(plan.Request), reflect.ValueOf(plan.Request.HubRoute), reflect.ValueOf(plan.Seeds[0])} {
		for i := range value.NumField() {
			if value.Field(i).IsZero() {
				t.Fatalf("give %s.%s a value here so that it is checked", value.Type().Name(), value.Type().Field(i).Name)
			}
		}
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var facts map[string]any
	if err := json.Unmarshal(raw, &facts); err != nil {
		t.Fatal(err)
	}
	reviewed := plan.reviewHash()
	unreviewed := []string{"review_id", "effects", "request.expected_plan_hash"}
	var walked []string
	alterEach(facts, "", func(path string) {
		walked = append(walked, path)
		raw, err := json.Marshal(facts)
		if err != nil {
			t.Fatal(err)
		}
		var altered PeerEnrollmentPlan
		if err := json.Unmarshal(raw, &altered); err != nil {
			t.Fatal(err)
		}
		left := slices.ContainsFunc(unreviewed, func(field string) bool { return path == field || strings.HasPrefix(path, field+"[") })
		if same := altered.reviewHash() == reviewed; same != left {
			t.Errorf("changing %s: same review ID = %v, want %v", path, same, left)
		}
	})
	for _, path := range []string{"request.hub_route.api", "seeds[0].address", "seeds[0].voting", "effects[0]"} {
		if !slices.Contains(walked, path) {
			t.Errorf("the walk never changed %s; it changed %v", path, walked)
		}
	}
}

// alterEach changes, one at a time, every string, number and boolean in a
// decoded JSON value, calls visit with its path, and puts it back.
func alterEach(node any, path string, visit func(path string)) {
	at := func(old any, set func(any), where string) {
		changed, ok := alteredLeaf(old)
		if !ok {
			alterEach(old, where, visit)
			return
		}
		set(changed)
		visit(where)
		set(old)
	}
	switch n := node.(type) {
	case map[string]any:
		for key, old := range n {
			where := key
			if path != "" {
				where = path + "." + key
			}
			at(old, func(v any) { n[key] = v }, where)
		}
	case []any:
		for i, old := range n {
			at(old, func(v any) { n[i] = v }, fmt.Sprintf("%s[%d]", path, i))
		}
	}
}

func alteredLeaf(value any) (any, bool) {
	switch leaf := value.(type) {
	case string:
		return leaf + "-changed", true
	case bool:
		return !leaf, true
	case float64:
		return leaf + 1, true
	}
	return nil, false
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

// A node asked by another on someone's behalf answers in that person's
// language, so a check it fails reads as one sentence in the language of
// whoever is enrolling the machine.
func TestANodeAnswersAnotherInTheLanguageOfWhoeverAsked(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var starts atomic.Int32
	options.Activate = testPeerApplication(t, &starts)
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	self := peer.Runtime.Load().Status().State.Members[peer.Config.NodeID]
	// A member the node does not know yet: it answers that it is still
	// learning the address.
	stranger := coordination.Member{NodeID: "node-stranger", Address: "192.0.2.9:7712", APIAddress: "https://192.0.2.9:7711"}
	for _, locale := range []i18n.Locale{i18n.LocaleEN, i18n.LocaleZH} {
		var result networkCheckResult
		if err := peer.peerJSON(i18n.WithLocale(t.Context(), locale), self, http.MethodPost, "/cluster/network/check", networkCheckRequest{Peers: []coordination.Member{stranger}}, &result); err != nil {
			t.Fatal(err)
		}
		if result.Error == "" || hasHan(result.Error) == (locale == i18n.LocaleEN) {
			t.Errorf("asked in %s, the node answered %q", locale, result.Error)
		}
	}
}

// The application behind this console answers in the language the
// console chose: the one the request names, or else the Hub's. That holds
// for a request the console passes on and for one it makes itself on the
// person's behalf.
func TestTheApplicationAnswersInTheLanguageTheConsoleChose(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	options.Text = i18n.New(i18n.LocaleEN)
	var starts atomic.Int32
	options.Activate = testPeerApplication(t, &starts)
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	var heard struct {
		Language string `json:"language"`
	}
	for _, tc := range []struct {
		header string
		want   i18n.Locale
	}{{"zh-CN,zh;q=0.9", i18n.LocaleZH}, {"en-US", i18n.LocaleEN}, {"", i18n.LocaleEN}, {"fr-FR", i18n.LocaleEN}} {
		request, err := http.NewRequest(http.MethodGet, peer.UiURL+"/console/test", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+peer.UIToken)
		if tc.header != "" {
			request.Header.Set("Accept-Language", tc.header)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		err = json.NewDecoder(response.Body).Decode(&heard)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%q: %d %v", tc.header, response.StatusCode, err)
		}
		if got := i18n.LocaleFromHeader(heard.Language); got != tc.want {
			t.Errorf("passed on with Accept-Language %q, the application was asked in %q, want %s", tc.header, heard.Language, tc.want)
		}
	}
	for _, locale := range []i18n.Locale{i18n.LocaleZH, i18n.LocaleEN} {
		if err := peer.applicationJSON(i18n.WithLocale(t.Context(), locale), http.MethodGet, "/console/test", nil, &heard); err != nil {
			t.Fatal(err)
		}
		if got := i18n.LocaleFromHeader(heard.Language); got != locale {
			t.Errorf("asking for someone who reads %s, the console asked the application in %q", locale, heard.Language)
		}
	}
}

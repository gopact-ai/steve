package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

type readOnlySSH struct{ SSHService }

func (readOnlySSH) SSHDiscover(context.Context) (sshconnect.Discovery, error) {
	return sshconnect.Discovery{}, nil
}

func (readOnlySSH) SSHCheck(context.Context, string) (sshconnect.CheckResult, error) {
	return sshconnect.CheckResult{}, nil
}

func TestSSHHandlerRefusesForeignHostsAndSites(t *testing.T) {
	token := strings.Repeat("t", 40)
	handler, err := SSHHandler(readOnlySSH{}, token, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, host, site string
		want                     int
	}{
		{"loopback", http.MethodGet, "127.0.0.1:1", "", http.StatusOK},
		{"rebound name", http.MethodGet, "evil.example:1", "", http.StatusForbidden},
		{"cross-site post", http.MethodPost, "127.0.0.1:1", "cross-site", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/console/ssh/candidates"
			if tc.method == http.MethodPost {
				path = "/console/ssh/check"
			}
			request := httptest.NewRequest(tc.method, "http://"+tc.host+path, strings.NewReader(`{}`))
			request.Header.Set("Authorization", "Bearer "+token)
			if tc.site != "" {
				request.Header.Set("Sec-Fetch-Site", tc.site)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.want, response.Body)
			}
		})
	}
}

// upgradeSSH answers upgrades the way the service does: a node ID no
// machine has is refused outright, a known machine that cannot be
// reached gets a record that says why, and a known machine with no
// upgrade running or just finished has no status to read.
type upgradeSSH struct{ SSHService }

func (upgradeSSH) SSHUpgrade(_ context.Context, node string) (sshconnect.InstallResult, error) {
	text := i18n.New(i18n.LocaleEN)
	if node == "node-dev" {
		return sshconnect.InstallResult{PlanID: "p1", NodeID: node, Status: "needs_attention"}, sshconnect.Fail(text, "preflight", "upgrade_target", "no tunnel", "fix")
	}
	return sshconnect.InstallResult{}, sshconnect.Fail(text, "preflight", "unknown_node", "no machine has node ID "+node, "use the node ID")
}

func (upgradeSSH) SSHUpgradeStatus(_ context.Context, node string) (sshconnect.InstallResult, error) {
	text := i18n.New(i18n.LocaleEN)
	if node == "node-dev" {
		return sshconnect.InstallResult{}, sshconnect.Fail(text, "planning", "unknown_upgrade", "node-dev has no upgrade running or just finished", "start an upgrade")
	}
	return sshconnect.InstallResult{}, sshconnect.Fail(text, "preflight", "unknown_node", "no machine has node ID "+node, "use the node ID")
}

// An upgrade, or its status, asked for a node ID no machine has is not
// found, and so is the status of a known machine with no upgrade to read,
// each with its refusal in the API's usual error body; a known machine
// that cannot be upgraded still answers with its record.
func TestSSHUpgradeOfAnUnknownNodeOrWithoutARecordIsNotFound(t *testing.T) {
	token := strings.Repeat("t", 40)
	handler, err := SSHHandler(upgradeSSH{}, token, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, node string
		want         int
		code, says   string
	}{
		{http.MethodPost, "Mac%20mini", http.StatusNotFound, "unknown_node", "Mac mini"},
		{http.MethodGet, "Mac%20mini", http.StatusNotFound, "unknown_node", "Mac mini"},
		{http.MethodPost, "node-dev", http.StatusOK, "", ""},
		{http.MethodGet, "node-dev", http.StatusNotFound, "unknown_upgrade", "node-dev"},
	} {
		request := httptest.NewRequest(tc.method, "http://127.0.0.1:1/console/ssh/upgrades/"+tc.node, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tc.want {
			t.Fatalf("%s %s = %d, want %d: %s", tc.method, tc.node, response.Code, tc.want, response.Body)
		}
		if tc.want != http.StatusNotFound {
			continue
		}
		var body struct {
			Error string               `json:"error"`
			Step  sshconnect.StepError `json:"step"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || !strings.Contains(body.Error, tc.says) || body.Step.Code != tc.code {
			t.Fatalf("%s %s body = %s (%v)", tc.method, tc.node, response.Body, err)
		}
	}
}

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

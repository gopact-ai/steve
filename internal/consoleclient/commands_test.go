package consoleclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSayRejectsUnsuccessfulResponses(t *testing.T) {
	var destinationRequests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationRequests.Add(1)
		io.WriteString(w, `{"reply":{"text":"unexpected destination"}}`)
	}))
	defer destination.Close()
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"blocked redirect", http.StatusTemporaryRedirect, `{"reply":{"title":"Success","text":"not actually sent"}}`, "307 Temporary Redirect"},
		{"server failure", http.StatusInternalServerError, `{"reply":{"text":"not a success"}}`, "500 Internal Server Error"},
		{"structured error", http.StatusForbidden, `{"error":"owner access required","reply":{"text":"do not print"}}`, "owner access required"},
		{"non JSON error", http.StatusServiceUnavailable, `<html>unavailable</html>`, "503 Service Unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/console/send" {
					t.Error("unexpected request")
				}
				if test.status == http.StatusTemporaryRedirect {
					w.Header().Set("Location", destination.URL+"/console/send")
				}
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			}))
			defer source.Close()
			output, err := captureClientOutput(t, func() error {
				return Say([]string{"-url", source.URL, "-token", "fixture-token", "/fleet"})
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if output != "" {
				t.Fatalf("failed command printed reply: %q", output)
			}
			if destinationRequests.Load() != 0 {
				t.Fatal("followed blocked cross-origin redirect")
			}
		})
	}
}

func TestRefusedTokenTellsHowToAuthenticate(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer hub.Close()
	for name, run := range map[string]func() error{
		"dash": func() error { return Dash([]string{"-url", hub.URL, "-token", ""}) },
		"say":  func() error { return Say([]string{"-url", hub.URL, "-token", "", "hi"}) },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := captureClientOutput(t, run)
			if err == nil || !strings.Contains(err.Error(), "-config") || !strings.Contains(err.Error(), "-token") {
				t.Fatalf("error = %v, want a hint naming -config and -token", err)
			}
		})
	}
}

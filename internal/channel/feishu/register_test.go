package feishu

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
)

func TestRegisterAppDeviceFlow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		switch r.Form.Get("action") {
		case "begin":
			w.Write([]byte(`{"device_code":"device-1","verification_uri_complete":"https://qr.example.com/scan?foo=bar","interval":1,"expire_in":60}`))
		case "poll":
			w.Write([]byte(`{"client_id":"cli_created","client_secret":"sec_created","user_info":{"open_id":"ou_scan","tenant_brand":"feishu"}}`))
		default:
			t.Fatalf("unexpected action %q", r.Form.Get("action"))
		}
	}))
	t.Cleanup(server.Close)

	var opened string
	var out strings.Builder
	created, err := RegisterApp(t.Context(), RegisterOptions{
		Out:    &out,
		Domain: server.URL,
		OpenURL: func(raw string) error {
			opened = raw
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.AppID != "cli_created" || created.AppSecret != "sec_created" {
		t.Fatalf("created = %#v", created)
	}
	if created.Domain != config.DomainFeishu || created.OpenID != "ou_scan" {
		t.Fatalf("identity = %#v", created)
	}
	if !strings.Contains(out.String(), "https://qr.example.com/scan") {
		t.Fatalf("missing registration link: %s", out.String())
	}
	if !strings.Contains(opened, "https://qr.example.com/scan") {
		t.Fatalf("browser was not opened: %q", opened)
	}
	if strings.Contains(out.String(), "sec_created") {
		t.Fatal("app secret was printed")
	}
}

func TestOpenBrowserRejectsNonHTTP(t *testing.T) {
	if err := openBrowser("file:///tmp/secret"); err == nil {
		t.Fatal("expected non-http url to be rejected")
	}
}

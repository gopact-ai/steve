package sameorigin

import (
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func request(method, host, origin string) *http.Request {
	r := httptest.NewRequest(method, "/console/send", nil)
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

func TestLoopbackSurfaceAcceptsOnlyLoopbackHosts(t *testing.T) {
	for _, host := range []string{"127.0.0.1:7710", "[::1]:7710", "localhost:7710", "LOCALHOST", "console.localhost:7710", "127.0.0.2", "localhost.:7710", "[::ffff:127.0.0.1]:7710", "[::1%25lo0]:7710"} {
		if err := Check(request(http.MethodGet, host, ""), Loopback); err != nil {
			t.Errorf("host %q refused: %v", host, err)
		}
	}
	// A rebound name resolves to loopback but still names the attacker's site.
	for _, host := range []string{"evil.example:7710", "evil.example", "", "10.0.0.5:7710", "localhost.evil.example", "[::ffff:10.0.0.5]:7710", "127.0.0.1.evil.example"} {
		if err := Check(request(http.MethodGet, host, ""), Loopback); err == nil {
			t.Errorf("host %q accepted on a loopback surface", host)
		}
	}
	if err := Check(request(http.MethodGet, "hub.internal:7710", ""), Network); err != nil {
		t.Errorf("a network surface names itself however it is reached: %v", err)
	}
}

func TestWritesRequireASameOriginCaller(t *testing.T) {
	accepted := []*http.Request{
		request(http.MethodPost, "127.0.0.1:7710", ""),
		request(http.MethodPost, "127.0.0.1:7710", "http://127.0.0.1:7710"),
		request(http.MethodPut, "localhost:7710", "http://localhost:7710"),
		request(http.MethodGet, "127.0.0.1:7710", "https://evil.example"),
	}
	typed := request(http.MethodPost, "127.0.0.1:7710", "")
	typed.Header.Set("Sec-Fetch-Site", "none") // the owner's own navigation
	accepted = append(accepted, typed)
	for _, r := range accepted {
		if err := Check(r, Loopback); err != nil {
			t.Errorf("%s from %q refused: %v", r.Method, r.Header.Get("Origin"), err)
		}
	}
	refused := []*http.Request{
		request(http.MethodPost, "127.0.0.1:7710", "https://evil.example"),
		request(http.MethodPost, "127.0.0.1:7710", "http://127.0.0.1:9999"),
		request(http.MethodDelete, "127.0.0.1:7710", "null"),
		request(http.MethodPatch, "127.0.0.1:7710", "file://"),
		request(http.MethodPost, "hub.internal:7710", "https://evil.example"),
	}
	fetched := request(http.MethodPost, "127.0.0.1:7710", "")
	fetched.Header.Set("Sec-Fetch-Site", "cross-site")
	refused = append(refused, fetched)
	for _, r := range refused {
		if err := Check(r, Reach(r.Host != "hub.internal:7710")); err == nil {
			t.Errorf("%s from %q to %q accepted", r.Method, r.Header.Get("Origin"), r.Host)
		}
	}
}

func TestLoopbackListener(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:7710": true, "[::1]:0": true, "localhost:7710": true, "127.0.0.2:1": true,
		"0.0.0.0:7710": false, "[::]:7710": false, ":7710": false, "hub.localhost:7710": false,
		"10.0.0.5:7710": false, "127.0.0.1": false, "": false,
	} {
		if got := LoopbackListener(addr); got != want {
			t.Errorf("LoopbackListener(%q) = %v, want %v", addr, got, want)
		}
	}
}

// DNS ignores ASCII letter case, so LOCALHOST is the name localhost. Only
// ASCII case is ignored: Unicode folding would also match the long s in
// "localhoſt", which is another name.
func TestLoopbackListenerIgnoresTheCaseOfLocalhost(t *testing.T) {
	for addr, want := range map[string]bool{"LOCALHOST:7710": true, "LocalHost:7710": true, "localhoſt:7710": false} {
		if got := LoopbackListener(addr); got != want {
			t.Errorf("LoopbackListener(%q) = %v, want %v", addr, got, want)
		}
	}
}

// A service only answers this machine when its listener is bound to a
// loopback IP, whatever name the configuration gave: a host name mapped to
// 127.0.1.1 is as local as 127.0.0.1, and a wildcard is not.
func TestReachOfFollowsTheBoundAddress(t *testing.T) {
	for _, c := range []struct {
		bound net.IP
		want  Reach
	}{
		{net.IPv4(127, 0, 0, 1), Loopback}, {net.IPv4(127, 0, 1, 1), Loopback}, {net.IPv6loopback, Loopback},
		{net.IPv4zero, Network}, {net.IPv6unspecified, Network}, {net.IPv4(192, 0, 2, 10), Network},
	} {
		if got := ReachOf(&net.TCPAddr{IP: c.bound, Port: 7710}); got != c.want {
			t.Errorf("ReachOf(%s) = %v, want %v", c.bound, got, c.want)
		}
	}
}

// A bound service refuses to start when its address and the IP it bound
// disagree: a name bound to loopback must be one the Host check accepts, or
// every client using that name is refused, and an address that names
// loopback must not have bound beyond it. The refusal names the address, the
// bound IP and the addresses to write instead, and writing one of those ends
// the refusal: a loopback IP binds itself, and a name binds where the refused
// address did.
func TestCheckBoundRefusesAnAddressThatDisagreesWithItsBinding(t *testing.T) {
	fixes := regexp.MustCompile(`write (\S+?)(?: or (\S+?))?,`)
	for _, c := range []struct {
		addr  string
		bound net.IP
		want  []string // nil accepts; otherwise the refusal mentions each
	}{
		{"localtest.me:7710", net.IPv4(127, 0, 0, 1), []string{`"localtest.me:7710"`, "127.0.0.1", "localhost:7710", "127.0.0.1:7710"}},
		{"ip6-localhost:7710", net.IPv6loopback, []string{`"ip6-localhost:7710"`, "::1", "localhost:7710", "[::1]:7710"}},
		{"localhost:7710", net.IPv4(192, 0, 2, 10), []string{`"localhost:7710"`, "192.0.2.10", "127.0.0.1:7710"}},
		{"foo.localhost:7710", net.IPv4(127, 0, 0, 1), nil},
		{"localhost.:7710", net.IPv4(127, 0, 0, 1), nil},
		{"LOCALHOST:7710", net.IPv4(127, 0, 0, 1), nil},
		{"127.0.0.2:7710", net.IPv4(127, 0, 0, 2), nil},
		{"[::1]:7710", net.IPv6loopback, nil},
		{"[::ffff:127.0.0.1]:7710", net.IPv4(127, 0, 0, 1), nil},
		{"0.0.0.0:7710", net.IPv6unspecified, nil},
		{":7710", net.IPv6unspecified, nil},
		{"hub.example:7710", net.IPv4(192, 0, 2, 10), nil},
	} {
		err := CheckBound(c.addr, &net.TCPAddr{IP: c.bound, Port: 7710})
		if c.want == nil {
			if err != nil {
				t.Errorf("CheckBound(%q, %s) = %v, want nil", c.addr, c.bound, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("CheckBound(%q, %s) = nil, want a refusal", c.addr, c.bound)
			continue
		}
		for _, part := range c.want {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("CheckBound(%q, %s) = %q, want it to mention %s", c.addr, c.bound, err, part)
			}
		}
		written := fixes.FindStringSubmatch(err.Error())
		if written == nil {
			t.Errorf("CheckBound(%q, %s) = %q, want it to name an address to write", c.addr, c.bound, err)
			continue
		}
		for _, fix := range written[1:] {
			if fix == "" {
				continue
			}
			binds := c.bound
			if host, _, _ := net.SplitHostPort(fix); net.ParseIP(host) != nil {
				binds = net.ParseIP(host)
			}
			if err := CheckBound(fix, &net.TCPAddr{IP: binds, Port: 7710}); err != nil {
				t.Errorf("CheckBound(%q, %s) suggests %s, which bound to %s is refused in turn: %v", c.addr, c.bound, fix, binds, err)
			}
		}
	}
}

func TestLoopbackIPRequiresLiteralLoopbackAddress(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:7710": true, "[::1]:0": true, "127.0.0.2:1": true, "[::ffff:127.0.0.1]:1": true,
		"localhost:7710": false, "[::1%lo0]:7710": false, "0.0.0.0:7710": false, "[::]:7710": false,
		":7710": false, "10.0.0.5:7710": false, "127.0.0.1": false, "": false,
	} {
		if got := LoopbackIP(addr); got != want {
			t.Errorf("LoopbackIP(%q) = %v, want %v", addr, got, want)
		}
	}
}

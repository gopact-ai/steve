// Package sameorigin keeps local HTTP surfaces out of reach of web pages the
// owner happens to have open. A bind address stops other machines, not the
// browser on this one: any page can POST to loopback, and a rebound DNS name
// lets it read the answers too.
package sameorigin

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

var (
	ErrForeignHost   = errors.New("request names a host this loopback service does not answer to")
	ErrForeignOrigin = errors.New("cross-origin writes are refused")
)

// Check refuses a request a browser sent on another site's behalf.
//
// loopbackOnly services also refuse any Host that is not a loopback name,
// which is what defeats DNS rebinding. Services reachable from the network
// are addressed by whatever name the operator gave them and rely on their
// token for that.
//
// Writes must come from the same origin or from a non-browser client, which
// sends no Origin at all.
func Check(r *http.Request, loopbackOnly bool) error {
	if loopbackOnly && !loopbackHost(hostname(r.Host)) {
		return ErrForeignHost
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || !strings.EqualFold(parsed.Host, r.Host) {
		return ErrForeignOrigin
	}
	return nil
}

func hostname(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
}

func loopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// Guard applies Check in front of next and answers 403 on refusal.
func Guard(next http.Handler, loopbackOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := Check(r, loopbackOnly); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

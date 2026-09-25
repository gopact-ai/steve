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
	"strings"
)

// ErrForeignHost refuses a Host a loopback service does not answer to.
var ErrForeignHost = errors.New("request names a host this loopback service does not answer to")

// crossOrigin refuses unsafe methods a browser sends for another origin,
// judged by Sec-Fetch-Site and then Origin; clients that send neither pass.
var crossOrigin = http.NewCrossOriginProtection()

// Reach says who can address a service, and so which Host names it answers.
type Reach bool

const (
	// Network services are addressed by whatever name the operator gave
	// them and rely on their token for that.
	Network Reach = false
	// Loopback services also refuse any Host that is not a loopback name,
	// which is what defeats DNS rebinding.
	Loopback Reach = true
)

// Check refuses a request a browser sent on another site's behalf.
func Check(r *http.Request, reach Reach) error {
	if reach == Loopback && !loopbackHost(hostname(r.Host)) {
		return ErrForeignHost
	}
	return crossOrigin.Check(r)
}

// Guard applies Check in front of next and answers 403 on refusal.
func Guard(next http.Handler, reach Reach) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := Check(r, reach); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ReachOf is the reach of a service whose listener is bound to addr. Only a
// loopback IP keeps every connection on this machine, and the bound address
// shows it whatever name the configuration gave: "localhost." and
// subdomains of localhost listen on loopback too, and must refuse a rebound
// Host as well.
func ReachOf(addr net.Addr) Reach {
	bound, ok := addr.(*net.TCPAddr)
	return Reach(ok && bound.IP.IsLoopback())
}

// LoopbackListener reports whether a listen address ("host:port") only
// accepts connections from this machine, judged from its text before it is
// bound: whether it may serve a loopback-only endpoint, or be given a token
// generated for this machine. It is stricter than a Host header: a bare
// ":port" listens everywhere, and only "localhost", in any letter case,
// among names is trusted to resolve to loopback. The Host check of a bound
// service follows ReachOf instead.
func LoopbackListener(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	return err == nil && LoopbackName(host)
}

// LoopbackName reports whether host, without a port, names this machine
// for a connection: "localhost" in any letter case, or a loopback IP.
// Subdomains of localhost and other spellings are left to the resolver, so
// they are not. Like LoopbackListener, it judges a name before anything is
// bound and does not decide the Host check.
func LoopbackName(host string) bool {
	// Host names ignore letter case. strings.EqualFold would go further and
	// match "localhoſt" (long s), which does not name this machine.
	if strings.ToLower(host) == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// LoopbackIP reports whether addr ("host:port") names a literal loopback
// IP. Unlike LoopbackListener it refuses "localhost" and zoned addresses:
// callers that require an explicit loopback address use it so a name can
// never stand in for one.
func LoopbackIP(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Zone() == "" && ip.IsLoopback()
}

func hostname(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
}

// loopbackHost accepts the names a browser only resolves to this machine.
func loopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

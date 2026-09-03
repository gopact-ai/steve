package nodewire

import (
	"net"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
)

var (
	selfMu   sync.RWMutex
	selfName string
)

// SetSelf names this process's own machine. The hub is a node too — the one
// with the coordinating role — and it needs a name like any other so that
// "where" never has to be answered with the name of a role.
func SetSelf(name string) {
	selfMu.Lock()
	defer selfMu.Unlock()
	selfName = name
}

// Self is this machine's node name, or "" before SetSelf.
func Self() string {
	selfMu.RLock()
	defer selfMu.RUnlock()
	return selfName
}

// Place renders a node name for people. The empty node means "this
// machine" throughout the model; here it becomes the hub's own name, or the
// role when the machine has not been named.
func Place(node string) string {
	if node != "" {
		return node
	}
	if self := Self(); self != "" {
		return self
	}
	return "hub"
}

// Version is this binary's build: the module version when there is one,
// else the commit it was built from, marked when the tree was dirty. A
// fleet whose machines cannot say which build they run cannot be
// upgraded with confidence.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	version := info.Main.Version
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	switch {
	case strings.HasPrefix(version, "v0.0.0-") && rev != "":
		// A pseudo-version is a timestamp and the same commit: say the commit.
		return rev + dirty
	case version != "" && version != "(devel)" && rev != "":
		return version + " (" + rev + dirty + ")"
	case rev != "":
		return rev + dirty
	case version != "":
		return version
	default:
		return "unknown"
	}
}

// Identity reports this machine's hostname and its non-loopback addresses,
// so a node can be told apart from the others by more than the label a
// config gave it.
func Identity() (hostname string, ips []string) {
	hostname, _ = os.Hostname()
	ifaces, err := net.Interfaces()
	if err != nil {
		return hostname, nil
	}
	var v4, v6 []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || virtual(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
				continue
			}
			if ip4 := ipNet.IP.To4(); ip4 != nil {
				v4 = append(v4, ip4.String())
			} else if ipNet.IP.IsGlobalUnicast() {
				v6 = append(v6, ipNet.IP.String())
			}
		}
	}
	sort.Strings(v4)
	sort.Strings(v6)
	return hostname, append(v4, v6...)
}

// virtual says whether an interface is a container or VM bridge: an
// address there does not help anyone find the machine.
func virtual(name string) bool {
	for _, prefix := range []string{"docker", "br-", "veth", "virbr", "lxc", "cni", "flannel", "tun", "tap"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

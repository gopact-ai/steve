package nodewire

import (
	"net"
	"os"
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

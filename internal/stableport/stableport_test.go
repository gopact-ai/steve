package stableport

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The lowest port any supported platform's default ephemeral range starts
// at (Linux 32768; macOS 49152).
const lowestDefaultEphemeralPort = 32768

type addressListener struct{ address string }

func (l addressListener) Accept() (net.Conn, error) { return nil, errors.New("not serving") }
func (l addressListener) Close() error              { return nil }
func (l addressListener) Addr() net.Addr {
	host, port, _ := net.SplitHostPort(l.address)
	n, _ := strconv.Atoi(port)
	return &net.TCPAddr{IP: net.ParseIP(host), Port: n}
}

func addressInUse(address string) error {
	return &net.OpError{Op: "listen", Net: "tcp", Err: fmt.Errorf("bind %s: %w", address, syscall.EADDRINUSE)}
}

// The range ends below the default ephemeral ranges and below the
// Kubernetes NodePort default range (30000-32767), whose ports kube-proxy
// forwards on a Kubernetes node, where a bind need not find one in use.
func TestRangeEndsBelowNodePortsAndEphemeralRanges(t *testing.T) {
	const firstDefaultNodePort = 30000
	if first < 1024 || last >= firstDefaultNodePort || last >= lowestDefaultEphemeralPort {
		t.Fatalf("range %d-%d reaches a privileged, NodePort or ephemeral port", first, last)
	}
}

// sequence hands out ports in order, from the first again after the last.
func sequence(ports ...int) func() int {
	next := 0
	return func() int {
		port := ports[next%len(ports)]
		next++
		return port
	}
}

func free(network, address string) error { return nil }

func TestKeepsAnExplicitPort(t *testing.T) {
	refused := errors.New("refused")
	var asked []string
	_, err := picker{candidate: sequence(first), probe: func(network, address string) error {
		t.Fatalf("probed %s for an explicit port", address)
		return nil
	}}.listen(func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		return nil, refused
	}, "tcp", "127.0.0.1:7801")
	if !errors.Is(err, refused) || len(asked) != 1 || asked[0] != "127.0.0.1:7801" {
		t.Fatalf("explicit port: asked %v, err %v", asked, err)
	}
}

// An empty port asks the kernel to pick, as port 0 does, so it is resolved
// from the range too.
func TestResolvesAnEmptyPortAsZero(t *testing.T) {
	var asked []string
	listener, err := picker{candidate: sequence(20001), probe: free}.listen(func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		return addressListener{address}, nil
	}, "tcp", "127.0.0.1:")
	if err != nil || len(asked) != 1 || listener.Addr().String() != "127.0.0.1:20001" {
		t.Fatalf("empty port: asked %v, err %v", asked, err)
	}
}

// portAt numbers the range without the SSH link window: indexes map, in
// order, to distinct ports in the range, and none to a link port.
func TestPortAtNumbersTheRangeWithoutTheLinkWindow(t *testing.T) {
	window := LinkLast - LinkFirst + 1
	if size != last-first+1-window {
		t.Fatalf("size %d, want %d", size, last-first+1-window)
	}
	for i, want := range map[int]int{0: first, LinkFirst - first - 1: LinkFirst - 1, LinkFirst - first: LinkLast + 1, size - 1: last} {
		if got := portAt(i); got != want {
			t.Errorf("portAt(%d) = %d, want %d", i, got, want)
		}
	}
	previous := first - 1
	for i := range size {
		port := portAt(i)
		if port <= previous || port > last || port >= LinkFirst && port <= LinkLast {
			t.Fatalf("portAt(%d) = %d after %d", i, port, previous)
		}
		previous = port
	}
}

// InRange holds for the range's ends and the ports next to the SSH link
// window, and not for the ports just outside the range or in the window.
func TestInRangeIsTheRangeWithoutTheLinkWindow(t *testing.T) {
	for port, want := range map[int]bool{
		first - 1: false, first: true, LinkFirst - 1: true, LinkFirst: false,
		LinkLast: false, LinkLast + 1: true, last: true, last + 1: false,
	} {
		if InRange(port) != want {
			t.Errorf("InRange(%d) = %v, want %v", port, !want, want)
		}
	}
}

// Port 0 is resolved on the host asked for, below every default ephemeral
// range, and not always to the same port.
func TestResolvesZeroOnTheSameHostBelowEphemeralRanges(t *testing.T) {
	seen := map[int]bool{}
	for range 200 {
		listener, err := picker{candidate: candidate, probe: free}.listen(func(network, address string) (net.Listener, error) {
			return addressListener{address}, nil
		}, "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().(*net.TCPAddr)
		if !address.IP.Equal(net.IPv4(127, 0, 0, 1)) {
			t.Fatalf("host changed: %s", address)
		}
		if address.Port < first || address.Port >= lowestDefaultEphemeralPort {
			t.Fatalf("port %d is not below every default ephemeral range", address.Port)
		}
		seen[address.Port] = true
	}
	if len(seen) < 2 {
		t.Fatalf("200 draws all gave port %v", seen)
	}
}

// A bind that finds its port in use moves on to the next candidate; any
// other bind error is the caller's answer.
func TestRetriesOnlyAPortInUse(t *testing.T) {
	var asked []string
	listener, err := picker{candidate: sequence(20001, 20002, 20003, 20004), probe: free}.listen(func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		if len(asked) <= 3 {
			return nil, addressInUse(address)
		}
		return addressListener{address}, nil
	}, "tcp", "127.0.0.1:0")
	if err != nil || len(asked) != 4 || listener.Addr().String() != "127.0.0.1:20004" {
		t.Fatalf("in-use ports: asked %v, err %v", asked, err)
	}

	refused := errors.New("permission denied")
	asked = nil
	if _, err := (picker{candidate: sequence(20001), probe: free}).listen(func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		return nil, refused
	}, "tcp", "127.0.0.1:0"); !errors.Is(err, refused) || len(asked) != 1 {
		t.Fatalf("other bind errors must not be retried: asked %v, err %v", asked, err)
	}
}

// A candidate the probe finds in use is never bound and counts as an
// attempt; a probe error of another kind leaves the answer to the bind.
func TestSkipsACandidateTheProbeFindsInUse(t *testing.T) {
	var probed, asked []string
	listener, err := picker{candidate: sequence(20001, 20002, 20003), probe: func(network, address string) error {
		probed = append(probed, address)
		if len(probed) <= 2 {
			return addressInUse(address)
		}
		return nil
	}}.listen(func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		return addressListener{address}, nil
	}, "tcp", "0.0.0.0:0")
	if err != nil || len(probed) != 3 || len(asked) != 1 || listener.Addr().String() != "0.0.0.0:20003" {
		t.Fatalf("probed %v, asked %v, err %v", probed, asked, err)
	}

	probed, asked = nil, nil
	if _, err := (picker{candidate: sequence(20001), probe: func(network, address string) error {
		probed = append(probed, address)
		return addressInUse(address)
	}}).listen(func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		return addressListener{"127.0.0.1:54321"}, nil
	}, "tcp", "127.0.0.1:0"); err != nil || len(probed) != attempts || len(asked) != 1 || asked[0] != "127.0.0.1:0" {
		t.Fatalf("probe in use every time: probed %d, asked %v, err %v", len(probed), asked, err)
	}

	asked = nil
	refused := errors.New("probe refused")
	if _, err := (picker{candidate: sequence(20001), probe: func(network, address string) error { return refused }}).listen(func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		return addressListener{address}, nil
	}, "tcp", "127.0.0.1:0"); err != nil || len(asked) != 1 || asked[0] != "127.0.0.1:20001" {
		t.Fatalf("other probe errors: asked %v, err %v", asked, err)
	}
}

// When every attempt in the range finds its port in use, the kernel picks
// as it did before: the node still starts, at an ephemeral port, and a
// warning says so. A port found in the range is not reported.
func TestFallsBackToTheKernelWhenAttemptsRunOut(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	var asked []string
	listener, err := picker{candidate: sequence(20001, 20002), probe: free}.listen(func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		if address == "127.0.0.1:0" {
			return addressListener{"127.0.0.1:54321"}, nil
		}
		return nil, addressInUse(address)
	}, "tcp", "127.0.0.1:0")
	if err != nil || listener.Addr().String() != "127.0.0.1:54321" {
		t.Fatalf("fallback: %v %v", listener, err)
	}
	if len(asked) != attempts+1 || asked[len(asked)-1] != "127.0.0.1:0" {
		t.Fatalf("asked %d addresses, last %q; want %d then the kernel", len(asked), asked[len(asked)-1], attempts)
	}
	if text := logged.String(); strings.Count(text, "level=WARN") != 1 || !strings.Contains(text, "127.0.0.1:0") {
		t.Fatalf("fallback warning: %q", text)
	}

	logged.Reset()
	if _, err := (picker{candidate: sequence(20001), probe: free}).listen(func(network, address string) (net.Listener, error) {
		return addressListener{address}, nil
	}, "tcp", "127.0.0.1:0"); err != nil || logged.Len() != 0 {
		t.Fatalf("a port in the range: err %v, logged %q", err, logged.String())
	}
}

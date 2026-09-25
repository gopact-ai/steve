//go:build !unix

package stableport

import (
	"net"
	"testing"
)

// Elsewhere, Listen hands port 0 to the kernel unchanged.
func TestListenHandsZeroToTheKernel(t *testing.T) {
	var asked []string
	if _, err := Listen(func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		return addressListener{address}, nil
	}, "tcp", "127.0.0.1:0"); err != nil || len(asked) != 1 || asked[0] != "127.0.0.1:0" {
		t.Fatalf("asked %v, err %v", asked, err)
	}
}

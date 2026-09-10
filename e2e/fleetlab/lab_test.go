package fleetlab_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/e2e/fleetlab"
	"github.com/gopact-ai/steve/internal/node"
)

// A lab is only useful if it delivers what the scenarios assume: separate
// machines, each reachable at the address the hub would configure, each
// reachable from the others, and each able to lose its node process and
// get it back without losing the work on disk.
func TestALabGivesEachNodeItsOwnMachine(t *testing.T) {
	lab := fleetlab.Start(t,
		fleetlab.Spec{Name: "node-a", Declares: []string{"gpu"}},
		fleetlab.Spec{Name: "node-b", Declares: []string{"network:internal"}},
	)

	a, b := lab.Node("node-a"), lab.Node("node-b")
	if a.Addr == b.Addr {
		t.Fatalf("both nodes answer on %s; they are not separate machines", a.Addr)
	}
	if a.Token == b.Token {
		t.Fatal("both nodes admit the same token")
	}

	// Separate filesystems: a file written on one is not on the other.
	lab.MustExec(t, "node-a", "printf only-here > "+a.Work+"/marker")
	if out, err := lab.Exec("node-b", "cat "+b.Work+"/marker"); err == nil {
		t.Fatalf("node-b can read node-a's file: %q", out)
	}

	// Separate process tables, with this node's own process in each.
	for _, name := range []string{"node-a", "node-b"} {
		out := lab.MustExec(t, name, "pgrep -af steve-node | head -2")
		if !strings.Contains(out, "steve-node -config") {
			t.Fatalf("no node process on %s: %q", name, out)
		}
	}

	// One address for both callers: node-a opens a connection to node-b at
	// the same address the hub was given, which is what a direct artifact
	// transfer between two nodes needs. The node speaks its own protocol,
	// so reaching it is the whole assertion.
	host, port, err := net.SplitHostPort(b.Addr)
	if err != nil {
		t.Fatalf("node-b address %q is not host:port", b.Addr)
	}
	if out, err := lab.Exec("node-a", "exec 3<>/dev/tcp/"+host+"/"+port+" && echo connected"); err != nil || !strings.Contains(out, "connected") {
		t.Fatalf("node-a cannot reach node-b at %s: %v\n%s", b.Addr, err, out)
	}

	// A node can be stopped and started again; the machine and its work
	// stay, which is what the node-goes-away scenarios rely on.
	if err := lab.StopNode("node-a"); err != nil {
		t.Fatal(err)
	}
	reg := node.NewRegistry("hub-lab-test", map[string]node.Config{
		"node-a": {Addr: a.Addr, Token: a.Token},
	})
	t.Cleanup(reg.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := reg.Advert(ctx, "node-a"); err == nil {
		t.Fatal("node-a still serves requests after stop")
	}
	if err := lab.StartNode("node-a"); err != nil {
		t.Fatal(err)
	}
	if out := lab.MustExec(t, "node-a", "cat "+a.Work+"/marker"); out != "only-here" {
		t.Fatalf("the work did not survive the restart: %q", out)
	}
}

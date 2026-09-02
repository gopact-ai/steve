package mesh

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/permission"
)

// requireReal gates the tests that spend real model calls. They are slower,
// cost money, and depend on credentials that live on the machines — so they
// are opt-in on top of the mesh gate.
func requireReal(t *testing.T) {
	t.Helper()
	requireMesh(t)
	if envOr("STEVE_MESH_REAL", "") == "" {
		t.Skip("set STEVE_MESH_REAL=1 to run against real models")
	}
}

func envOr(key, fallback string) string { return env(key, fallback) }

// Real-0: which real harnesses on which nodes actually answer. This is the
// roster's truth beyond "the binary exists": credentials, network, and the
// adapter all have to work for a prompt to come back.
func TestRealHarnessesAnswerOnTheNodes(t *testing.T) {
	requireReal(t)
	reg := registry(t)
	type probe struct{ node, harness string }
	var probes []probe
	for _, nodeName := range []string{nodeA, nodeB} {
		advert, err := reg.Advert(t.Context(), nodeName)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range advert.Harnesses {
			if h.Missing != "" || h.ID == "mock" || h.ID == "absent" {
				continue
			}
			probes = append(probes, probe{nodeName, h.ID})
		}
	}
	if len(probes) == 0 {
		t.Fatal("no real harnesses advertised")
	}

	answered := 0
	for _, p := range probes {
		t.Run(p.node+"/"+p.harness, func(t *testing.T) {
			broker, _ := permission.New("auto")
			host := acphost.New(acphost.Config{Transport: reg.Transport(p.node, p.harness), Permission: broker})
			t.Cleanup(host.Stop)
			// npx cold starts and model latency both live inside this.
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			started := time.Now()
			sid, gen, err := host.OpenSession(ctx, "", acphost.SessionConfig{Workdir: "/home/pengxiang.lpx/steve-work"})
			if err != nil {
				t.Logf("✗ %s/%s: open session: %v", p.node, p.harness, err)
				t.Skip("harness did not start")
			}
			out, _, err := host.Prompt(ctx, sid, gen, "Reply with exactly the single word OK and nothing else.", nil)
			if err != nil {
				t.Logf("✗ %s/%s: prompt: %v", p.node, p.harness, err)
				t.Skip("harness did not answer")
			}
			answer := strings.TrimSpace(out)
			t.Logf("✓ %s/%s answered %q in %s", p.node, p.harness, truncateAnswer(answer), time.Since(started).Round(time.Second))
			if !strings.Contains(strings.ToUpper(answer), "OK") {
				t.Errorf("%s/%s answered but not with OK: %q", p.node, p.harness, truncateAnswer(answer))
			}
			answered++
		})
	}
	if answered == 0 {
		t.Fatal("no real harness on any node answered a prompt")
	}
	t.Logf("%d of %d real harnesses answered", answered, len(probes))
}

func truncateAnswer(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

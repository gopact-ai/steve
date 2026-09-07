package turn

import (
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
)

func TestCoordinatorClassifiesEverySupportedAgentSelector(t *testing.T) {
	catalog, err := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Default: true, Aliases: []string{"codex", "my helper"}}})
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{coordinatorState: &coordinatorState{catalog: catalog}}
	for _, input := range []string{"@codex/cancel", "@codex!/cancel", "@my helper /tasks pause 12", "/use codex/cancel"} {
		address, parsed := c.ParseInput(input)
		if address != "@worker" || !(parsed.Control() || parsed.Interrupt) {
			t.Errorf("%q classified as %q %+v", input, address, parsed)
		}
	}
}

func TestImmediateInputUsesPlatformControlParsing(t *testing.T) {
	for _, input := range []string{"/cancel", "/tasks pause", "/tasks 12 pause", "/tasks 12 取消", "/tasks #12 暂停", "@codex /cancel", "@claude /tasks cancel 12", "/use codex /cancel", "！重新做", "+ /tasks pause 12"} {
		if !ImmediateInput(input) {
			t.Errorf("control would wait behind its target: %q", input)
		}
	}
	for _, input := range []string{"hello", "cancel", "/cancelled", "/tasks resume 12", "/tasks show 12", "/tasks pause invalid", "@codex work", "!", "/use codex"} {
		if ImmediateInput(input) {
			t.Errorf("ordinary input bypassed the queue: %q", input)
		}
	}
}

package plugins

import (
	"reflect"
	"testing"
)

// A cloned origin saves the same: empty lists stay empty, not absent.
func TestAgentOriginCloneKeepsEmptyLists(t *testing.T) {
	origin := &AgentOrigin{
		Applied:  &AppliedPreset{Skills: []string{}, MCPServers: []string{}, Preserved: []string{}},
		Template: AgentPreset{Skills: []string{}, MCPServers: []string{}},
	}
	if clone := origin.Clone(); !reflect.DeepEqual(clone, origin) {
		t.Fatalf("clone = %#v, want %#v", clone, origin)
	}
}

package admin

import (
	"encoding/json"
	"testing"

	"github.com/gopact-ai/steve/internal/agenttools"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/view"
)

type selectorProjectionNodes struct{}

func (selectorProjectionNodes) Statuses() []node.Status {
	return []node.Status{{Name: "worker", Up: true, Advert: nodewire.Advert{Node: "worker", Harnesses: []nodewire.Harness{{ID: "fixture", Command: "not-executed"}}}}}
}
func selectorProjectionModel() *readmodel.Model {
	book := models.New()
	for _, name := range []string{"", "worker"} {
		book.Observe(models.Observation{Node: name, Harness: "fixture", Source: "session", Selectors: models.SelectorsOf([]view.Option{
			{ID: "toggle", Name: "Toggle", Type: "boolean", Category: "vendor/private", Current: "false"},
			{ID: "unset", Name: "Unspecified", Type: "boolean"},
			{ID: "opaque", Name: "Opaque", Type: "select", Current: "false", Choices: []view.Choice{{Value: "false", Label: "Literal false"}}},
		})})
	}
	return readmodel.New(readmodel.Sources{Hub: readmodel.Hub{Node: "hub"}, Nodes: selectorProjectionNodes{}, Models: book,
		HubAdvert: func() nodewire.Advert {
			return nodewire.Advert{Node: "hub", Harnesses: []nodewire.Harness{{ID: "fixture", Command: "not-executed"}}}
		}})
}
func checkSelectorProjectionJSON(t *testing.T, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("missing observed selectors: %s", raw)
	}
	if got[0]["type"] != "boolean" || got[0]["current"] != "false" || got[0]["category"] != "vendor/private" {
		t.Fatalf("boolean false/type/category lost: %s", raw)
	}
	if got[1]["type"] != "boolean" {
		t.Fatalf("unfixed boolean type lost: %s", raw)
	}
	if _, present := got[1]["current"]; present {
		t.Fatalf("missing Actual was invented as false: %s", raw)
	}
	if got[2]["type"] != "select" || got[2]["current"] != "false" {
		t.Fatalf("opaque select false changed type: %s", raw)
	}
}
func TestObservedNodeAgentSelectorWireTypePreservesFalseAndUnset(t *testing.T) {
	for _, place := range []string{"", "worker"} {
		t.Run(place, func(t *testing.T) {
			a := &Service{View: selectorProjectionModel()}
			candidates := []agenttools.Candidate{{Harness: "fixture"}}
			a.describeOffers(t.Context(), place, candidates)
			checkSelectorProjectionJSON(t, candidates[0].Selectors)
		})
	}
}

func TestObservedDesktopAgentSelectorWireTypePreservesFalseAndUnset(t *testing.T) {
	a, _ := desktopAdminFixture(t)
	book := models.New()
	book.Observe(models.Observation{Harness: "grok", Source: "session", Selectors: models.SelectorsOf([]view.Option{
		{ID: "toggle", Name: "Toggle", Type: "boolean", Category: "vendor/private", Current: "false"},
		{ID: "unset", Name: "Unspecified", Type: "boolean"},
		{ID: "opaque", Name: "Opaque", Type: "select", Current: "false", Choices: []view.Choice{{Value: "false", Label: "Literal false"}}},
	})})
	a.View = readmodel.New(readmodel.Sources{Hub: readmodel.Hub{Node: "hub"}, Models: book,
		HubAdvert: func() nodewire.Advert {
			return nodewire.Advert{Node: "hub", Harnesses: []nodewire.Harness{{ID: "grok", Command: "not-executed"}}}
		}})
	found, err := a.DesktopDiscover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range found.Agents {
		if candidate.Harness == "grok" {
			checkSelectorProjectionJSON(t, candidate.Selectors)
			return
		}
	}
	t.Fatal("installed fixture was not discovered")
}

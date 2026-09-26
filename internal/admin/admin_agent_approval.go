package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sort"

	"github.com/gopact-ai/steve/internal/approval"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/models"
)

// SyncAgentApproval makes the whole fleet follow the hub's default approval
// stance. It does that by dropping the approval mode each agent pinned for
// itself rather than by writing the default into every one of them: the
// setting stays the single place the stance is decided, so changing it
// later moves these agents too instead of leaving a fleet of stale copies.
//
// An agent whose AI tool offers no mode at that level is reported rather
// than forced: the owner should hear that their default cannot land there,
// not discover it as silence.
func (a *Service) SyncAgentApproval(ctx context.Context) (consoleapi.ApprovalSync, error) {
	modes := a.agentModes(ctx)
	out := consoleapi.ApprovalSync{}
	err := a.changeAgents(func(c *config.Config) error {
		agents := c.Agents
		out = consoleapi.ApprovalSync{}
		intent := c.Gateway.DefaultApproval
		if intent == "" {
			return errors.New(textFor(ctx).T(i18n.AdminApprovalDefaultUnset))
		}
		out.Intent = intent
		for id, item := range agents {
			seen := modes[id]
			if len(seen.Modes) > 0 {
				if _, ok := approval.Resolve(intent, seen.Modes); !ok {
					out.Unmapped = append(out.Unmapped, id)
				}
			}
			pinned, value := pinnedMode(item.Options, seen.Option)
			if pinned == "" {
				out.Following = append(out.Following, id)
				continue
			}
			options := maps.Clone(item.Options)
			delete(options, pinned)
			if len(options) == 0 {
				options = nil
			}
			item.Options = options
			agents[id] = item
			out.Cleared = append(out.Cleared, consoleapi.ApprovalSyncAgent{Agent: id, Was: value})
		}
		sort.Strings(out.Following)
		sort.Strings(out.Unmapped)
		sort.Slice(out.Cleared, func(i, j int) bool { return out.Cleared[i].Agent < out.Cleared[j].Agent })
		return nil
	})
	if err != nil && !config.Committed(err) {
		return consoleapi.ApprovalSync{}, err
	}
	slog.Info(fmt.Sprintf("steve: %d agents now follow the default approval %s", len(out.Cleared), out.Intent), "approval", out.Intent, "cleared", len(out.Cleared))
	return out, err
}

// agentMode is what one agent was last seen offering: the id of its
// approval selector and the levels it listed under it.
type agentMode struct {
	Option string
	Modes  []approval.Mode
}

// agentModes reads the fleet's approval vocabularies from the read model.
// An agent that has never run reports nothing, which is not an error: it
// simply cannot be checked against the stance until it runs once.
func (a *Service) agentModes(ctx context.Context) map[string]agentMode {
	out := map[string]agentMode{}
	if a.View == nil {
		return out
	}
	for _, item := range a.View.Snapshot(ctx).Agents {
		for _, selector := range item.Selectors {
			if selector.Category != "mode" && selector.ID != "mode" {
				continue
			}
			seen := agentMode{Option: selector.ID}
			for i, value := range selector.Values {
				seen.Modes = append(seen.Modes, approval.Mode{Value: value, Label: label(selector, i)})
			}
			out[item.ID] = seen
			break
		}
	}
	return out
}

func label(selector models.Selector, index int) string {
	if index < len(selector.Choices) {
		return selector.Choices[index]
	}
	return ""
}

// pinnedMode finds the approval mode an agent pinned for itself: under the
// selector id its tool was last seen using, or under the name every tool
// so far has given that selector.
func pinnedMode(options map[string]string, selector string) (string, string) {
	for _, key := range []string{selector, "mode"} {
		if key == "" {
			continue
		}
		if value, ok := options[key]; ok {
			return key, value
		}
	}
	return "", ""
}

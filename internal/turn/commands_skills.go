// The /skills command: listing, adding, updating and removing the skills
// an agent is told about.

package turn

import (
	"context"
	"fmt"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/skills"
	"strings"
)

func (c commands) skillsCmd(ctx context.Context, req Request, selected agent.Agent, rest string) (Result, error) {
	if injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID) != home.ModeOwner {
		return Result{AgentID: selected.ID, Text: c.text.T(i18n.SkillsOwnerOnly)}, nil
	}
	if c.skills == nil || c.skills.Map == nil {
		return Result{AgentID: selected.ID, Text: c.text.T(i18n.SkillsUnconfigured)}, nil
	}
	action, arg := skills.ParseAction(rest)
	switch action {
	case skills.ActionList, skills.ActionHelp:
		return Result{AgentID: selected.ID, Text: c.skillsList()}, nil
	case skills.ActionEnable:
		return c.skillsMutate(selected, arg, c.skills.Enable, i18n.SkillsEnabled)
	case skills.ActionDisable:
		return c.skillsMutate(selected, arg, c.skills.Disable, i18n.SkillsDisabled)
	case skills.ActionSourceAdd:
		if arg == "" {
			return Result{AgentID: selected.ID, Text: c.skillsUsage()}, nil
		}
		src, err := c.skills.AddSource(ctx, arg)
		if err != nil {
			return Result{AgentID: selected.ID, Text: err.Error()}, nil
		}
		return Result{AgentID: selected.ID, Text: c.text.T(i18n.SkillsSourceAdded, src.Slug, src.Head, strings.Join(src.Skills, ", "), protocol.CommandSkills) + "\n\n" + c.skillsList()}, nil
	case skills.ActionUpdate:
		if !c.lockSkills() {
			return Result{AgentID: selected.ID, Text: c.text.T(i18n.SkillsBusy, protocol.CommandCancel)}, nil
		}
		updated, err := c.skills.UpdateSources(ctx)
		c.unlockSkills()
		if err != nil {
			return Result{AgentID: selected.ID, Text: err.Error()}, nil
		}
		failed := ""
		for _, src := range updated {
			if src.Error != "" {
				failed += "\n- " + src.Slug + ": " + src.Error
			}
		}
		return Result{AgentID: selected.ID, Text: c.text.T(i18n.SkillsUpdated, len(updated), failed, protocol.CommandNew) + "\n\n" + c.skillsList()}, nil
	case skills.ActionPathAdd:
		if arg == "" {
			return Result{AgentID: selected.ID, Text: c.skillsUsage()}, nil
		}
		if err := c.skills.AddPath(arg); err != nil {
			return Result{AgentID: selected.ID, Text: err.Error()}, nil
		}
		return Result{AgentID: selected.ID, Text: c.text.T(i18n.SkillsPathAdded) + "\n\n" + c.skillsList()}, nil
	case skills.ActionPathRemove:
		if arg == "" {
			return Result{AgentID: selected.ID, Text: c.skillsUsage()}, nil
		}
		if !c.lockSkills() {
			return Result{AgentID: selected.ID, Text: c.text.T(i18n.SkillsBusy, protocol.CommandCancel)}, nil
		}
		err := c.skills.RemovePath(arg)
		c.unlockSkills()
		if err != nil {
			return Result{AgentID: selected.ID, Text: err.Error()}, nil
		}
		return Result{
			AgentID: selected.ID,
			Text:    c.text.T(i18n.SkillsPathRemoved, protocol.CommandNew) + "\n\n" + c.skillsList(),
		}, nil
	default:
		return Result{AgentID: selected.ID, Text: c.skillsUsage()}, nil
	}
}

func (c commands) skillsMutate(selected agent.Agent, arg string, op func(string) error, key i18n.Key) (Result, error) {
	if arg == "" {
		return Result{AgentID: selected.ID, Text: c.skillsUsage()}, nil
	}
	if !c.lockSkills() {
		return Result{AgentID: selected.ID, Text: c.text.T(i18n.SkillsBusy, protocol.CommandCancel)}, nil
	}
	err := op(arg)
	c.unlockSkills()
	if err != nil {
		return Result{AgentID: selected.ID, Text: err.Error()}, nil
	}
	return Result{
		AgentID: selected.ID,
		Text:    c.text.T(key, arg, protocol.CommandNew) + "\n\n" + c.skillsList(),
	}, nil
}

func (c commands) skillsList() string {
	var b strings.Builder
	enabled, err := c.skills.Map.Enabled()
	if err != nil {
		fmt.Fprintf(&b, "%s\n", c.text.T(i18n.SkillsInvalidEnabled, err))
	} else if len(enabled) == 0 {
		b.WriteString(c.text.T(i18n.SkillsNone) + "\n")
	} else {
		b.WriteString("enabled:\n")
		for _, ref := range enabled {
			fmt.Fprintf(&b, "- %s\n  %s\n", ref.Name, ref.Path)
		}
	}
	avail, err := c.skills.Map.Available()
	if err != nil {
		fmt.Fprintf(&b, "%s\n", c.text.T(i18n.SkillsInvalidAvailable, err))
	} else {
		on := map[string]struct{}{}
		for _, ref := range enabled {
			on[ref.Name] = struct{}{}
		}
		var off []skills.Ref
		for _, ref := range avail {
			if _, ok := on[ref.Name]; !ok {
				off = append(off, ref)
			}
		}
		if len(off) == 0 {
			b.WriteString(c.text.T(i18n.SkillsNoneOff) + "\n")
		} else {
			b.WriteString("available:\n")
			for _, ref := range off {
				fmt.Fprintf(&b, "- %s (off)\n  %s\n", ref.Name, ref.Path)
			}
		}
	}
	paths := c.skills.Map.SearchPaths()
	if len(paths) == 0 {
		b.WriteString(c.text.T(i18n.SkillsSearchNone) + "\n")
	} else {
		b.WriteString("search:\n")
		for _, path := range paths {
			fmt.Fprintf(&b, "- %s\n", path)
		}
	}
	b.WriteString(c.skillsUsage())
	return strings.TrimSpace(b.String())
}

func (c commands) skillStatusLine() string {
	if c.skills == nil || c.skills.Map == nil {
		return ""
	}
	enabled, err := c.skills.Map.Enabled()
	if err != nil {
		return "skills=error"
	}
	if len(enabled) == 0 {
		return "skills=none"
	}
	names := make([]string, 0, len(enabled))
	for _, ref := range enabled {
		names = append(names, ref.Name)
	}
	return "skills=" + strings.Join(names, ",")
}

func (c commands) skillsUsage() string {
	cmd := protocol.CommandSkills
	return c.text.T(i18n.SkillsUsage, cmd, cmd, cmd, cmd, cmd, cmd, protocol.CommandNew)
}

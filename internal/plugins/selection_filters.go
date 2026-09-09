package plugins

import "slices"

type CapabilityFilter struct {
	Skills []string `json:"skills"`
	MCP    []string `json:"mcp"`
}

func (selection Selection) Clone() Selection {
	selection.ExcludedSkills = slices.Clone(selection.ExcludedSkills)
	selection.Deployments = slices.Clone(selection.Deployments)
	if selection.Filters != nil {
		filters := make(map[string]CapabilityFilter, len(selection.Filters))
		for id, filter := range selection.Filters {
			filter.Skills = append([]string{}, filter.Skills...)
			filter.MCP = append([]string{}, filter.MCP...)
			filters[id] = filter
		}
		selection.Filters = filters
	}
	return selection
}

func (selection Selection) Includes(installation, kind, name string) bool {
	filter, limited := selection.Filters[installation]
	if !limited {
		return true
	}
	if kind == "skill" {
		return slices.Contains(filter.Skills, name)
	}
	return slices.Contains(filter.MCP, name)
}

func (selection Selection) validateFilters() error {
	if len(selection.ExcludedSkills) > 64 || duplicate(selection.ExcludedSkills) {
		return ErrInvalid
	}
	for _, name := range selection.ExcludedSkills {
		if !nameShape.MatchString(name) {
			return ErrInvalid
		}
	}
	for id, filter := range selection.Filters {
		if !nameShape.MatchString(id) || duplicate(filter.Skills) || duplicate(filter.MCP) {
			return ErrInvalid
		}
		for _, name := range append(append([]string{}, filter.Skills...), filter.MCP...) {
			if !nameShape.MatchString(name) {
				return ErrInvalid
			}
		}
	}
	return nil
}

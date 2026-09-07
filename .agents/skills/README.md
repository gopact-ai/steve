# Steve workbench skills

These skills are installed at repository scope. They are development guidance
for working on Steve; they are separate from the skills served to Steve's agents
by the running hub.

| Skill | Role | Upstream |
| --- | --- | --- |
| [interface-design](interface-design/SKILL.md) | Product interface design and visual craft | [Dammyjay93/interface-design](https://github.com/Dammyjay93/interface-design) |
| [web-design-guidelines](web-design-guidelines/SKILL.md) | Accessibility and interaction review | [vercel-labs/agent-skills](https://github.com/vercel-labs/agent-skills) |
| [vercel-react-best-practices](react-best-practices/SKILL.md) | React client performance | [vercel-labs/agent-skills](https://github.com/vercel-labs/agent-skills) |

See [the workbench instructions](../../web/console/AGENTS.md) for how these fit
the existing stack. Exact upstream revisions and skill hashes are recorded in
[sources.json](sources.json). Keep upstream skill files intact when updating.

The Web Interface Guidelines skill intentionally fetches its current checklist
from the official Vercel source at review time. Installing these skills adds no
application dependencies, hooks, browser extensions, or services.

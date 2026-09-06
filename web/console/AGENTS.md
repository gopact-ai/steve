# Workbench UI

For design, UI implementation, or interaction review in this directory, use the
project-local skills in `../../.agents/skills`:

- `interface-design`: product UI hierarchy, density, navigation, states, tokens,
  typography, and visual refinement.
- `web-design-guidelines`: accessibility, focus, forms, feedback, responsive
  layout, and keyboard interaction review.
- `vercel-react-best-practices` (directory `react-best-practices`): React rendering,
  client requests, effect dependencies, bundle size, and measured performance.

Steve is an operational workbench. Design around completing tasks, reading live
progress, switching conversations, and managing projects and agents.

Inspect the running UI and existing tokens/components before changing it. Reuse
React Aria controls, the existing Untitled UI components/icons, and Tailwind
conventions. This is a React/Vite client with a Go backend: Next.js, RSC, and
server-action examples apply only when their underlying concept actually fits.
Do not add a framework or library merely to follow a skill example.

Use the skills as guidance within the user's brief. Do not replace a font,
palette, navigation pattern, or data density merely to avoid a named aesthetic.
Apply English typography and casing advice only to relevant English text.

Verify meaningful interaction changes in an actual browser, including narrow
windows, keyboard focus, long content, loading/error states, and repeated
actions. Automated regressions supplement this check. Keep design exploration
and review notes outside the source tree; preserve current usage documentation,
tokens, and component behavior in their existing locations.

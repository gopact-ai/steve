import { fontStack, resolveAppearance, type Appearance } from "./appearance.ts";
import { COLOR_KEYS } from "./themes.ts";

export function appearanceStyle(
  a: Appearance,
  systemDark: boolean,
): Record<string, string> {
  const { palette } = resolveAppearance(a, systemDark);
  const vars: Record<string, string> = {
    "--font-body": fontStack(a.fonts.ui.family, "ui"),
    "--font-display": fontStack(a.fonts.heading.family, "heading"),
    "--font-reading": fontStack(a.fonts.reading.family, "reading"),
    "--font-mono": fontStack(a.fonts.code.family, "code"),
    "--ui-font-size": `${a.fonts.ui.size}px`,
    "--reading-font-size": `${a.fonts.reading.size}px`,
    "--code-font-size": `${a.fonts.code.size}px`,
    "--heading-scale": String(a.fonts.heading.scale),
  };
  // Built-ins use generated CSS; only custom palettes need inline seeds.
  // This also lets static style previews inspect all built-ins by data-theme.
  if (palette.id.startsWith("custom-"))
    for (const key of COLOR_KEYS) vars[`--seed-${key}`] = palette.colors[key];
  return vars;
}
export function applyAppearance(
  a: Appearance,
  systemDark: boolean,
  root: HTMLElement = document.documentElement,
  darkModeClass = "dark-mode",
) {
  const { scheme, palette } = resolveAppearance(a, systemDark);
  root.classList.toggle(darkModeClass, scheme === "dark");
  root.style.colorScheme = scheme;
  if (palette.id === "light" || palette.id === "dark")
    delete root.dataset.theme;
  else root.dataset.theme = palette.id;
  for (const key of COLOR_KEYS) root.style.removeProperty(`--seed-${key}`);
  for (const [key, value] of Object.entries(appearanceStyle(a, systemDark)))
    root.style.setProperty(key, value);
  if (root === document.documentElement)
    document
      .querySelector('meta[name="theme-color"]')
      ?.setAttribute("content", palette.colors.bg);
}

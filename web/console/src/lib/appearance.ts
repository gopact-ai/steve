import {
  COLOR_KEYS,
  DEFAULT_PALETTES,
  PALETTES,
  paletteOf,
  type Palette,
  type PaletteColors,
  type Scheme,
} from "./themes.ts";

export const APPEARANCE_KEY = "steve.ui.appearance";
export const MAX_PALETTES = 32;
export type Mode = Scheme | "system";
export type FontRole = "ui" | "heading" | "reading" | "code";
export interface FontChoice {
  family: string;
  size: number;
}
export interface Appearance {
  version: 1;
  mode: Mode;
  light: string;
  dark: string;
  fonts: {
    ui: FontChoice;
    reading: FontChoice;
    code: FontChoice;
    heading: { family: string; scale: number };
  };
  custom: Palette[];
}
export const FONT_PRESETS = ["system", "serif", "mono"] as const;
export const FONT_LIMITS = {
  ui: [12, 18],
  reading: [12, 24],
  code: [11, 22],
} as const;
export function defaultAppearance(): Appearance {
  return {
    version: 1,
    mode: "system",
    light: "light",
    dark: "dark",
    fonts: {
      ui: { family: "system", size: 14 },
      heading: { family: "system", scale: 1 },
      reading: { family: "system", size: 16 },
      code: { family: "mono", size: 13 },
    },
    custom: [],
  };
}
const object = (v: unknown): Record<string, unknown> =>
  v && typeof v === "object" && !Array.isArray(v)
    ? (v as Record<string, unknown>)
    : {};
export const validFamily = (v: unknown): v is string =>
  typeof v === "string" &&
  v.trim().length > 0 &&
  v.length <= 100 &&
  /^[\p{L}\p{N} _-]+$/u.test(v);
const stacks = {
  system:
    '-apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", sans-serif',
  serif: '"Songti SC", "Noto Serif CJK SC", Georgia, serif',
  mono: '"SFMono-Regular", Consolas, "Liberation Mono", monospace',
};
export function fontStack(family: string, role: FontRole): string {
  const fallback = role === "code" ? stacks.mono : stacks.system;
  if (!validFamily(family)) return fallback;
  return Object.hasOwn(stacks, family)
    ? stacks[family as keyof typeof stacks]
    : `"${family.trim()}", ${fallback}`;
}
function validPalette(value: unknown): Palette | null {
  const p = object(value),
    colors = object(p.colors);
  if (
    typeof p.id !== "string" ||
    !/^custom-[a-zA-Z0-9-]{1,80}$/.test(p.id) ||
    typeof p.name !== "string" ||
    !p.name.trim() ||
    p.name.length > 60 ||
    /[\u0000-\u001f]/.test(p.name) ||
    (p.scheme !== "light" && p.scheme !== "dark")
  )
    return null;
  if (
    COLOR_KEYS.some(
      (k) =>
        typeof colors[k] !== "string" ||
        !/^#[0-9a-fA-F]{6}$/.test(colors[k] as string),
    )
  )
    return null;
  const safe = Object.fromEntries(
    COLOR_KEYS.map((k) => [k, (colors[k] as string).toLowerCase()]),
  ) as PaletteColors;
  return {
    id: p.id,
    name: p.name.trim(),
    scheme: p.scheme,
    colors: safe,
    swatch: [safe.bg, safe.accent, safe.keyword],
  };
}
export function paletteChoices(a: Appearance, scheme: Scheme): Palette[] {
  return [...DEFAULT_PALETTES, ...PALETTES, ...a.custom].filter(
    (p) => p.scheme === scheme,
  );
}
export function normalizeAppearance(value: unknown): Appearance {
  const d = defaultAppearance(),
    v = object(value);
  if (v.version !== 1) return d;
  const ids = new Set<string>();
  const custom = (
    Array.isArray(v.custom) ? v.custom.slice(0, MAX_PALETTES) : []
  ).flatMap((item) => {
    const p = validPalette(item);
    if (!p || ids.has(p.id)) return [];
    ids.add(p.id);
    return [p];
  });
  const a: Appearance = {
    ...d,
    custom,
    mode: v.mode === "light" || v.mode === "dark" ? v.mode : "system",
  };
  for (const s of ["light", "dark"] as const)
    if (paletteChoices(a, s).some((p) => p.id === v[s])) a[s] = v[s] as string;
  const fonts = object(v.fonts);
  for (const role of ["ui", "reading", "code"] as const) {
    const f = object(fonts[role]),
      [min, max] = FONT_LIMITS[role];
    a.fonts[role] = {
      family: validFamily(f.family) ? f.family.trim() : d.fonts[role].family,
      size:
        typeof f.size === "number" &&
        Number.isInteger(f.size) &&
        f.size >= min &&
        f.size <= max
          ? f.size
          : d.fonts[role].size,
    };
  }
  const h = object(fonts.heading);
  a.fonts.heading = {
    family: validFamily(h.family) ? h.family.trim() : d.fonts.heading.family,
    scale:
      typeof h.scale === "number" && [0.9, 1, 1.15].includes(h.scale)
        ? h.scale
        : 1,
  };
  return a;
}
export function migrateTheme(saved: string | null): Appearance {
  const a = defaultAppearance(),
    p = saved ? paletteOf(saved) : undefined;
  if (p) {
    a.mode = p.scheme;
    a[p.scheme] = p.id;
  } else if (saved === "light" || saved === "dark") a.mode = saved;
  return a;
}
export function readAppearance(storage: Pick<Storage, "getItem">): Appearance {
  try {
    const raw = storage.getItem(APPEARANCE_KEY);
    if (raw !== null) return normalizeAppearance(JSON.parse(raw));
    return migrateTheme(storage.getItem("ui-theme"));
  } catch {
    return defaultAppearance();
  }
}
export function choosePalette(
  a: Appearance,
  scheme: Scheme,
  id: string,
): Appearance {
  return paletteChoices(a, scheme).some((p) => p.id === id)
    ? { ...a, [scheme]: id }
    : a;
}
export function removePalette(a: Appearance, id: string): Appearance {
  return normalizeAppearance({
    ...a,
    custom: a.custom.filter((p) => p.id !== id),
  });
}
export function resolveAppearance(a: Appearance, systemDark: boolean) {
  const scheme: Scheme =
    a.mode === "system" ? (systemDark ? "dark" : "light") : a.mode;
  return {
    scheme,
    palette:
      paletteChoices(a, scheme).find((p) => p.id === a[scheme]) ??
      DEFAULT_PALETTES.find((p) => p.scheme === scheme)!,
  };
}
export function exportPalette(p: Palette): string {
  return JSON.stringify(
    {
      version: 1,
      palette: { name: p.name, scheme: p.scheme, colors: p.colors },
    },
    null,
    2,
  );
}
export function importPalette(text: string): Palette {
  if (text.length > 65536) throw new Error("invalid-palette");
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    throw new Error("invalid-palette");
  }
  const doc = object(raw),
    p = validPalette({ ...object(doc.palette), id: "custom-import" });
  if (doc.version !== 1 || !p) throw new Error("invalid-palette");
  return p;
}
export function contrastRatio(a: string, b: string): number {
  const luminance = (hex: string) => {
    const rgb = [1, 3, 5]
      .map((i) => parseInt(hex.slice(i, i + 2), 16) / 255)
      .map((c) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
    return rgb[0] * 0.2126 + rgb[1] * 0.7152 + rgb[2] * 0.0722;
  };
  const x = luminance(a),
    y = luminance(b);
  return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05);
}

// The palettes offered beyond the built-in light and dark schemes. Their
// colours live in styles/themes.css; what a palette needs here is its
// name, whether it reads as light or dark, and the three colours the menu
// shows: background, accent, and the colour its keywords are written in,
// which is what makes Monokai and Dracula tellable apart at swatch size.
export type Scheme = "light" | "dark";
export type ThemeId = "system" | "light" | "dark" | PaletteId;
export type PaletteId =
    | "vscode-dark" | "one-dark" | "one-light" | "dracula" | "nord"
    | "tokyo-night" | "solarized-dark" | "solarized-light" | "gruvbox-dark" | "monokai";

export interface Palette { id: PaletteId; name: string; scheme: Scheme; swatch: [string, string, string] }

export const PALETTES: Palette[] = [
    { id: "vscode-dark", name: "VS Code Dark+", scheme: "dark", swatch: ["#1f1f1f", "#3794ff", "#569cd6"] },
    { id: "one-dark", name: "Atom One Dark", scheme: "dark", swatch: ["#282c34", "#61afef", "#c678dd"] },
    { id: "one-light", name: "Atom One Light", scheme: "light", swatch: ["#fafafa", "#4078f2", "#a626a4"] },
    { id: "dracula", name: "Dracula", scheme: "dark", swatch: ["#282a36", "#bd93f9", "#ff79c6"] },
    { id: "nord", name: "Nord", scheme: "dark", swatch: ["#2e3440", "#88c0d0", "#a3be8c"] },
    { id: "tokyo-night", name: "Tokyo Night", scheme: "dark", swatch: ["#1a1b26", "#7aa2f7", "#bb9af7"] },
    { id: "solarized-dark", name: "Solarized Dark", scheme: "dark", swatch: ["#002b36", "#268bd2", "#859900"] },
    { id: "solarized-light", name: "Solarized Light", scheme: "light", swatch: ["#fdf6e3", "#268bd2", "#859900"] },
    { id: "gruvbox-dark", name: "Gruvbox Dark", scheme: "dark", swatch: ["#282828", "#83a598", "#fb4934"] },
    { id: "monokai", name: "Monokai", scheme: "dark", swatch: ["#272822", "#66d9ef", "#f92672"] },
];

export const paletteOf = (theme: string): Palette | undefined => PALETTES.find((p) => p.id === theme);
export const isTheme = (value: string): value is ThemeId => value === "system" || value === "light" || value === "dark" || !!paletteOf(value);

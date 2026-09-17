import type { ReactNode } from "react";
import { createContext, useContext, useEffect, useState } from "react";

import { isTheme, paletteOf, type Scheme, type ThemeId } from "@/lib/themes";

type Theme = ThemeId;

interface ThemeContextType {
    theme: Theme;
    /** Light or dark, after "system" and any palette have been resolved. */
    scheme: Scheme;
    setTheme: (theme: Theme) => void;
}

const ThemeContext = createContext<ThemeContextType | undefined>(undefined);

export const useTheme = (): ThemeContextType => {
    const context = useContext(ThemeContext);

    if (context === undefined) {
        throw new Error("useTheme must be used within a ThemeProvider");
    }

    return context;
};

interface ThemeProviderProps {
    children: ReactNode;
    /**
     * The class to add to the root element when the theme reads as dark
     * @default "dark-mode"
     */
    darkModeClass?: string;
    /**
     * The default theme to use if no theme is stored in localStorage
     * @default "system"
     */
    defaultTheme?: Theme;
    /**
     * The key to use to store the theme in localStorage
     * @default "ui-theme"
     */
    storageKey?: string;
}

// The scheme is stored next to the theme so the boot script in index.html
// can paint the right background before this module is even parsed,
// without carrying a copy of the palette table.
export const schemeKey = "ui-theme-scheme";

const systemScheme = (): Scheme => (window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
const schemeOf = (theme: Theme): Scheme => (theme === "system" ? systemScheme() : theme === "light" || theme === "dark" ? theme : paletteOf(theme)?.scheme ?? "light");

export const ThemeProvider = ({ children, defaultTheme = "system", storageKey = "ui-theme", darkModeClass = "dark-mode" }: ThemeProviderProps) => {
    const [theme, setTheme] = useState<Theme>(() => {
        if (typeof window !== "undefined") {
            const saved = localStorage.getItem(storageKey);
            return saved && isTheme(saved) ? saved : defaultTheme;
        }
        return defaultTheme;
    });
    const [scheme, setScheme] = useState<Scheme>(() => (typeof window === "undefined" ? "light" : schemeOf(theme)));

    useEffect(() => {
        const applyTheme = () => {
            const root = window.document.documentElement;
            const palette = paletteOf(theme);
            const resolved = schemeOf(theme);

            if (palette) root.dataset.theme = palette.id;
            else delete root.dataset.theme;
            root.classList.toggle(darkModeClass, resolved === "dark");
            setScheme(resolved);

            if (theme === "system") localStorage.removeItem(storageKey);
            else localStorage.setItem(storageKey, theme);
            localStorage.setItem(schemeKey, resolved);
            document.querySelector('meta[name="theme-color"]')?.setAttribute("content", getComputedStyle(root).getPropertyValue("--color-bg-primary").trim() || (resolved === "dark" ? "#232428" : "#f5f5f7"));
        };

        applyTheme();

        // Listen for system theme changes
        const mediaQuery = window.matchMedia("(prefers-color-scheme: dark)");

        const handleChange = () => {
            if (theme === "system") {
                applyTheme();
            }
        };

        mediaQuery.addEventListener("change", handleChange);
        return () => mediaQuery.removeEventListener("change", handleChange);
    }, [theme]);

    return <ThemeContext.Provider value={{ theme, scheme, setTheme }}>{children}</ThemeContext.Provider>;
};

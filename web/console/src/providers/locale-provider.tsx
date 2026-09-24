import { createContext, use, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { I18nProvider } from "react-aria";
import { intlLocale, loadLocale, localeLoaded, resolveLocale, setCurrentLocale, translate, type Locale, type LocalePreference, type Translator } from "@/lib/i18n";
import { setRequestLocale } from "@/lib/http";

export const localeStorageKey = "steve.ui.locale";
interface LocaleContextValue { locale: Locale; preference: LocalePreference; setLocale: (locale: LocalePreference) => void; t: Translator }
const LocaleContext = createContext<LocaleContextValue | null>(null);
const validPreference = (value: string | null): LocalePreference => value === "zh" || value === "en" ? value : "system";

function savedPreference(): LocalePreference {
    try { return validPreference(localStorage.getItem(localeStorageKey)); } catch { return "system"; }
}

export function LocaleProvider({ children }: { children: ReactNode }) {
    const [preference, setPreference] = useState(savedPreference);
    const [languages, setLanguages] = useState(() => [...navigator.languages]);
    const locale = resolveLocale(preference, languages);
    // Only the first paint waits for its messages. A later change of
    // language fetches first and switches after (see apply), so the page
    // stays drawn in the old language rather than falling back to nothing.
    if (!localeLoaded(locale)) use(loadLocale(locale));
    // The last change asked for wins, even if an earlier fetch lands later.
    const asked = useRef(0);
    const chosen = useRef(preference);
    chosen.current = preference;
    const apply = useCallback((next: LocalePreference, browser: readonly string[], commit: () => void) => {
        const turn = ++asked.current;
        loadLocale(resolveLocale(next, browser)).then(() => { if (turn === asked.current) commit(); }, () => { /* Unavailable messages leave the current language in place. */ });
    }, []);
    // Children read and write during their own effects, which React runs
    // before this provider's. The language a request asks for is therefore
    // settled here, in render, so the first read of a page already carries
    // the language the page is about to be drawn in.
    setRequestLocale(locale);
    setCurrentLocale(locale);

    useLayoutEffect(() => {
        document.documentElement.lang = intlLocale(locale);
        document.title = translate(locale, "app.title");
    }, [locale]);

    useEffect(() => {
        const onStorage = (event: StorageEvent) => {
            if (event.key !== localeStorageKey && event.key !== null) return;
            try { if (event.storageArea !== localStorage) return; } catch { return; }
            const next = validPreference(event.newValue);
            apply(next, navigator.languages, () => setPreference(next));
        };
        const onLanguage = () => { const next = [...navigator.languages]; apply(chosen.current, next, () => setLanguages(next)); };
        window.addEventListener("storage", onStorage);
        window.addEventListener("languagechange", onLanguage);
        return () => { window.removeEventListener("storage", onStorage); window.removeEventListener("languagechange", onLanguage); };
    }, [apply]);

    const value = useMemo<LocaleContextValue>(() => ({
        locale, preference,
        setLocale(next) {
            apply(next, navigator.languages, () => {
                setRequestLocale(resolveLocale(next, navigator.languages));
                setPreference(next);
                try { localStorage.setItem(localeStorageKey, next); } catch { /* The current window can still change language. */ }
            });
        },
        t: (key, ...args) => translate(locale, key, ...args),
    }), [locale, preference, apply]);

    return <LocaleContext.Provider value={value}><I18nProvider locale={intlLocale(locale)}>{children}</I18nProvider></LocaleContext.Provider>;
}

// Markdown also renders outside the app shell — a detached preview, a test
// harness — where there is no provider to ask. Those readers still deserve
// words in a language, so they fall back to the browser's.
export function useTranslator(): Translator {
    const value = useContext(LocaleContext);
    const browser = useMemo(() => resolveLocale("system", navigator.languages), []);
    if (!value && !localeLoaded(browser)) use(loadLocale(browser));
    return value ? value.t : (key, ...args) => translate(browser, key, ...args);
}

export function useI18n(): LocaleContextValue {
    const value = useContext(LocaleContext);
    if (!value) throw new Error("useI18n must be used within LocaleProvider");
    return value;
}

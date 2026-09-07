import { createContext, useContext, useEffect, useLayoutEffect, useMemo, useState, type ReactNode } from "react";
import { I18nProvider } from "react-aria";
import { intlLocale, resolveLocale, translate, type Locale, type LocalePreference, type Translator } from "@/lib/i18n";
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

    useLayoutEffect(() => {
        document.documentElement.lang = intlLocale(locale);
        document.title = translate(locale, "app.title");
        setRequestLocale(locale);
    }, [locale]);

    useEffect(() => {
        const onStorage = (event: StorageEvent) => {
            if (event.key !== localeStorageKey && event.key !== null) return;
            try { if (event.storageArea !== localStorage) return; } catch { return; }
            setPreference(validPreference(event.newValue));
        };
        const onLanguage = () => setLanguages([...navigator.languages]);
        window.addEventListener("storage", onStorage);
        window.addEventListener("languagechange", onLanguage);
        return () => { window.removeEventListener("storage", onStorage); window.removeEventListener("languagechange", onLanguage); };
    }, []);

    const value = useMemo<LocaleContextValue>(() => ({
        locale, preference,
        setLocale(next) {
            setRequestLocale(resolveLocale(next, navigator.languages));
            setPreference(next);
            try { localStorage.setItem(localeStorageKey, next); } catch { /* The current window can still change language. */ }
        },
        t: (key, ...args) => translate(locale, key, ...args),
    }), [locale, preference]);

    return <LocaleContext.Provider value={value}><I18nProvider locale={intlLocale(locale)}>{children}</I18nProvider></LocaleContext.Provider>;
}

export function useI18n(): LocaleContextValue {
    const value = useContext(LocaleContext);
    if (!value) throw new Error("useI18n must be used within LocaleProvider");
    return value;
}

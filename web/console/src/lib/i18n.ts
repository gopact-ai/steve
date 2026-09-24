import type { zh } from "./i18n/catalog-zh.ts";

export type Locale = "zh" | "en";
export type LocalePreference = Locale | "system";
export type MessageKey = keyof typeof zh;
type ParametersIn<Text extends string> = Text extends `${string}{${infer Name}}${infer Rest}` ? Name | ParametersIn<Rest> : never;
export type TranslationArgs<Key extends MessageKey> = [ParametersIn<(typeof zh)[Key]>] extends [never]
    ? [params?: Record<string, string | number>]
    : [params: Record<ParametersIn<(typeof zh)[Key]>, string | number>];
export type Translator = <Key extends MessageKey>(key: Key, ...args: TranslationArgs<Key>) => string;

// Each language is its own chunk, fetched when it is first needed, so a
// page carries the messages of the language it is drawn in and no other.
// translate is synchronous: whoever picks a language loads it first.
const catalogs: Partial<Record<Locale, Record<MessageKey, string>>> = {};
const loading: Partial<Record<Locale, Promise<void>>> = {};
const failed = new Set<Locale>();
const importers: Record<Locale, () => Promise<Record<MessageKey, string>>> = {
    zh: () => import("./i18n/catalog-zh.ts").then((module) => module.zh),
    en: () => import("./i18n/catalog-en.ts").then((module) => module.en),
};
let current: Locale | undefined;

export const localeLoaded = (locale: Locale) => !!catalogs[locale];

// A view waiting with use() is handed the same promise on every render, a
// failed one included: a fresh promise per render would suspend again
// instead of reaching the error boundary.
export function loadLocale(locale: Locale): Promise<void> {
    return loading[locale] ??= importers[locale]().then((messages) => { catalogs[locale] = messages; }, (error) => { failed.add(locale); throw error; });
}

/** A deliberate switch asks again for messages whose last fetch failed.
 *  Whether the module is fetched again is the browser's choice; some keep
 *  a failed import for the life of the page, and only a reload helps. */
export function requestLocale(locale: Locale): Promise<void> {
    if (failed.delete(locale)) delete loading[locale];
    return loadLocale(locale);
}

/** The language the page is drawn in, for text produced away from a view. */
export function setCurrentLocale(locale: Locale) { current = locale; }

// Keep local validation errors as messages until a view chooses its language.
// The plain message is in the page's language, or any loaded one.
export class LocalizedError extends Error {
    readonly render: (locale: Locale) => string;
    constructor(render: (locale: Locale) => string) {
        const locale = current && catalogs[current] ? current : catalogs.zh ? "zh" : catalogs.en ? "en" : undefined;
        super(locale ? render(locale) : "");
        this.render = render;
    }
}
export const errorText = (error: unknown, locale: Locale): string => error instanceof LocalizedError ? error.render(locale) : error instanceof Error ? error.message : String(error);

export function normalizeLocale(value?: string | null): Locale | undefined {
    const language = value?.toLowerCase().replace(/_/g, "-").split(/[.-]/)[0];
    return language === "zh" || language === "en" ? language : undefined;
}

export function resolveLocale(preference: LocalePreference, languages: readonly string[] = []): Locale {
    if (preference !== "system") return preference;
    for (const language of languages) {
        const locale = normalizeLocale(language);
        if (locale) return locale;
    }
    return "en";
}

export const intlLocale = (locale: Locale) => locale === "zh" ? "zh-CN" : "en";

export function translate<Key extends MessageKey>(locale: Locale, key: Key, ...[params]: TranslationArgs<Key>): string {
    const messages = catalogs[locale];
    if (!messages) throw new Error(`Messages for ${locale} are not loaded`);
    const message: string = messages[key];
    return message.replace(/\{(\w+)\}/g, (_, name: string) => {
        const value = (params as Record<string, string | number> | undefined)?.[name];
        if (value === undefined) throw new Error(`Missing translation parameter ${key}.${name}`);
        return String(value);
    });
}

import { catalogs } from "./i18n/catalog.ts";

export type Locale = "zh" | "en";
export type LocalePreference = Locale | "system";
export type MessageKey = keyof typeof catalogs.zh;
type ParametersIn<Text extends string> = Text extends `${string}{${infer Name}}${infer Rest}` ? Name | ParametersIn<Rest> : never;
export type TranslationArgs<Key extends MessageKey> = [ParametersIn<(typeof catalogs.zh)[Key]>] extends [never]
    ? [params?: Record<string, string | number>]
    : [params: Record<ParametersIn<(typeof catalogs.zh)[Key]>, string | number>];
export type Translator = <Key extends MessageKey>(key: Key, ...args: TranslationArgs<Key>) => string;

// Keep local validation errors as messages until a view chooses its language.
export class LocalizedError extends Error {
    readonly render: (locale: Locale) => string;
    constructor(render: (locale: Locale) => string) {
        super(render("zh"));
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
    const message: string = catalogs[locale][key];
    return message.replace(/\{(\w+)\}/g, (_, name: string) => {
        const value = (params as Record<string, string | number> | undefined)?.[name];
        if (value === undefined) throw new Error(`Missing translation parameter ${key}.${name}`);
        return String(value);
    });
}

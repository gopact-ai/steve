import type { EventDefinition, EventText } from "./assistant-types";

// Resolve each field independently. Missing text stays absent, while an
// available translation remains discoverable even without a default string.
export function eventDefinitionText(definition: EventDefinition | undefined, locale: string): EventText {
    if (!definition) return {};
    const language = (locale === "zh" ? "zh-CN" : locale).toLowerCase();
    const translations = new Map(Object.entries(definition.i18n || {}).map(([tag, text]) => [tag.toLowerCase(), text]));
    const choices = [translations.get(language), translations.get(language.split("-")[0]), definition, translations.get("en"), ...[...translations].sort(([a], [b]) => a.localeCompare(b)).map(([, text]) => text)];
    const text: EventText = {};
    for (const field of ["name", "description"] as const) {
        const value = choices.find((candidate) => candidate?.[field]?.trim())?.[field]?.trim();
        if (value) text[field] = value;
    }
    return text;
}

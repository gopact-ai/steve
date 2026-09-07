import type { HubSettings, SettingsField, SettingsObject } from "./api/settings";
import { LocalizedError, translate } from "./i18n.ts";

export const settingGroups = {
    gateway: ["gateway.locale", "gateway.task_max_turns", "gateway.task_max_elapsed", "gateway.prompt_timeout"],
    execution: ["policies.execution.step_timeout", "policies.execution.verify_timeout"],
    planning: ["policies.planning.timeout", "policies.planning.attempts"],
    snapshot: ["policies.snapshot.max_files", "policies.snapshot.max_bytes", "policies.snapshot.max_file_bytes"],
    review: ["policies.review.max_changes", "policies.review.max_diff_bytes", "policies.review.max_file_bytes", "policies.review.max_entries", "policies.review.timeout"],
} as const;
export type SettingPath = (typeof settingGroups)[keyof typeof settingGroups][number];
export const editableSettings = new Set<string>(Object.values(settingGroups).flat());

export function settingValue(settings: SettingsObject, path: string): string | number | undefined {
    let current: string | number | SettingsObject | undefined = settings;
    for (const part of path.split(".")) {
        if (!current || typeof current !== "object" || !Object.hasOwn(current, part)) return undefined;
        current = current[part];
    }
    return typeof current === "string" || typeof current === "number" ? current : undefined;
}

export function settingsInputs(view: HubSettings): Record<string, string> {
    return Object.fromEntries(view.fields.filter((field) => editableSettings.has(field.path)).map((field) => [field.path, String(settingValue(view.desired, field.path) ?? field.default ?? "")]));
}

function durationNanoseconds(value: string): number | null {
    if (value === "0") return 0;
    const units: Record<string, number> = { ns: 1, us: 1e3, "µs": 1e3, "μs": 1e3, ms: 1e6, s: 1e9, m: 60e9, h: 3600e9 };
    const parts = [...value.matchAll(/(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/g)];
    if (!parts.length || parts.map((part) => part[0]).join("") !== value) return null;
    const amount = parts.reduce((sum, part) => sum + Math.floor(Number(part[1]) * units[part[2]]), 0);
    return Number.isFinite(amount) && amount <= 9223372036854775807 ? amount : null;
}

function parseSetting(field: SettingsField, raw: string): string | number {
    const value = raw.trim();
    if (field.enum) {
        if (!field.enum.includes(value)) throw new LocalizedError((locale) => translate(locale, "settingsPage.invalidChoice", { field: field.path }));
        return value;
    }
    const amount = field.type === "duration" ? durationNanoseconds(value) : field.type === "integer" && /^-?\d+$/.test(value) ? Number(value) : null;
    if (amount === null || (field.type === "integer" && !Number.isSafeInteger(amount))) {
        throw new LocalizedError((locale) => translate(locale, field.type === "duration" ? "settingsPage.invalidDuration" : "settingsPage.invalidInteger", { field: field.path }));
    }
    if ((field.minimum !== undefined && amount < field.minimum) || (field.maximum !== undefined && amount > field.maximum)) {
        throw new LocalizedError((locale) => translate(locale, "settingsPage.outOfRange", { field: field.path, minimum: field.minimum ?? 0, maximum: field.maximum ?? "—" }));
    }
    return field.type === "duration" ? value : amount;
}

// Only changed, explicitly editable fields leave the browser. Identity and
// fields introduced by a newer server are never copied into an update.
export function settingsPatch(view: HubSettings, inputs: Record<string, string>): SettingsObject {
    const patch: SettingsObject = {};
    const baseline = settingsInputs(view);
    for (const field of view.fields) {
        if (!editableSettings.has(field.path) || inputs[field.path] === baseline[field.path]) continue;
        const value = parseSetting(field, inputs[field.path] ?? "");
        const parts = field.path.split(".");
        let target = patch;
        for (const part of parts.slice(0, -1)) { target[part] ??= {}; target = target[part] as SettingsObject; }
        target[parts.at(-1)!] = value;
    }
    const maxFile = Number(settingValue(patch, "policies.snapshot.max_file_bytes") ?? settingValue(view.desired, "policies.snapshot.max_file_bytes"));
    const maxTotal = Number(settingValue(patch, "policies.snapshot.max_bytes") ?? settingValue(view.desired, "policies.snapshot.max_bytes"));
    if (maxFile > maxTotal) throw new LocalizedError((locale) => translate(locale, "settingsPage.snapshotLimit"));
    return patch;
}

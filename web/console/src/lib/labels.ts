import { translate, type Locale, type MessageKey } from "./i18n.ts";
import { number } from "./format.ts";

const labelKeys = {
    status: { unknown: "status.unknown", pending: "status.idle", running: "status.running", needs_you: "status.needsYou", set_aside: "status.setAside", ended: "status.ended" },
    taskState: { draft: "status.draft", running: "status.inProgress", idle: "status.idle", blocked: "status.blocked", review: "status.review", done: "status.done", failed: "status.failed", paused: "status.paused", cancelled: "status.cancelled" },
    stepState: { pending: "status.pending", ready: "status.ready", running: "status.running", verifying: "status.verifying", "awaiting-human": "status.awaitingHuman", done: "status.done", failed: "status.failed", skipped: "status.skipped" },
    repo: { inplace: "repo.inplace", isolated: "repo.isolated" },
    requestType: { writer: "request.writer", disclosure: "request.disclosure", effect: "request.effect", question: "request.question", pairing: "request.pairing" },
    origin: { chat: "origin.chat", plan: "origin.plan", schedule: "origin.schedule", delegate: "origin.delegate", repair: "origin.repair" },
} as const satisfies Record<string, Record<string, MessageKey>>;

export function labelsFor(locale: Locale): Record<keyof typeof labelKeys | "level", Record<string, string>> {
    const groups = Object.fromEntries(Object.entries(labelKeys).map(([group, entries]) => [group, Object.fromEntries(Object.entries(entries).map(([key, message]) => [key, translate(locale, message)]))])) as Record<keyof typeof labelKeys, Record<string, string>>;
    return { ...groups, level: { public: "public", internal: "internal", restricted: "restricted", sealed: "sealed" } };
}

// Existing consumers migrate to labelsFor(locale) as their views subscribe.
export const zh = labelsFor("zh");
export const label = (table: Record<string, string>, key?: string) => (key ? table[key] ?? key : "—");

// Keep K/M units consistent across the workbench; only the number is localized.
export function fmtTokens(n?: number, locale: Locale = "zh"): string {
    if (!n) return "0";
    if (n >= 1_000_000) return number(n / 1_000_000, locale, { minimumFractionDigits: 1, maximumFractionDigits: 1 }) + "M";
    if (n >= 1_000) return number(n / 1_000, locale, { minimumFractionDigits: 1, maximumFractionDigits: 1 }) + "k";
    return number(n, locale);
}

export function spend(t?: { total?: number; context?: number }, locale: Locale = "zh"): string {
    if (t?.total) return `${fmtTokens(t.total, locale)} tok`;
    if (t?.context) return translate(locale, "format.context", { count: fmtTokens(t.context, locale) });
    return translate(locale, "format.unreported");
}

export function fmtSeconds(s?: number, locale: Locale = "zh"): string {
    const seconds = Math.max(0, Math.round(s || 0));
    if (seconds < 60) return translate(locale, "format.seconds", { count: number(seconds, locale) });
    if (seconds < 3600) return translate(locale, "format.minutesSeconds", { minutes: number(Math.floor(seconds / 60), locale), seconds: number(seconds % 60, locale) });
    return translate(locale, "format.hoursMinutes", { hours: number(Math.floor(seconds / 3600), locale), minutes: number(Math.floor((seconds % 3600) / 60), locale) });
}

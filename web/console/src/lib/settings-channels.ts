import type { ChannelPatch, ChannelSettings } from "./api/settings";
import { LocalizedError, translate } from "./i18n.ts";

export interface ChannelDraft { default_channel: "console" | "feishu"; enabled: boolean; app_id: string; domain: "feishu" | "lark"; owner_open_id: string; group_policy: "open" | "allowlist" | "disabled"; allow_unmentioned: boolean; allowed_senders: string; blocked_senders: string; secret: string; clearSecret: boolean }

export function channelInputs(view: ChannelSettings): ChannelDraft {
    const { feishu } = view.desired;
    return { default_channel: view.desired.default_channel, enabled: feishu.enabled, app_id: feishu.app_id || "", domain: feishu.domain || "feishu", owner_open_id: feishu.owner_open_id || "", group_policy: feishu.group_policy || "open", allow_unmentioned: feishu.allow_unmentioned, allowed_senders: (feishu.allowed_senders || []).join("\n"), blocked_senders: (feishu.blocked_senders || []).join("\n"), secret: "", clearSecret: false };
}

export function changedInputs<T extends object>(baseline: T, current: T): Partial<T> {
    return Object.fromEntries(Object.entries(current).filter(([key, value]) => value !== baseline[key as keyof T])) as Partial<T>;
}

export function channelPatch(view: ChannelSettings, draft: ChannelDraft): ChannelPatch {
    if (!draft.enabled && draft.default_channel === "feishu") throw new LocalizedError((locale) => translate(locale, "settingsPage.defaultDisabled"));
    if (draft.enabled && (!draft.app_id.trim() || draft.clearSecret || (!draft.secret.trim() && !view.desired.feishu.app_secret_configured))) throw new LocalizedError((locale) => translate(locale, "settingsPage.credentialsRequired"));
    if (!["console", "feishu"].includes(draft.default_channel) || !["feishu", "lark"].includes(draft.domain) || !["open", "allowlist", "disabled"].includes(draft.group_policy)) throw new LocalizedError((locale) => translate(locale, "settingsPage.channelInvalid"));
    const baseline = channelInputs(view), changes = changedInputs(baseline, draft);
    const patch: ChannelPatch = {}, feishu: NonNullable<ChannelPatch["feishu"]> = {};
    if (changes.default_channel !== undefined) patch.default_channel = draft.default_channel;
    for (const key of ["enabled", "app_id", "domain", "owner_open_id", "group_policy", "allow_unmentioned"] as const) {
        if (changes[key] !== undefined) Object.assign(feishu, { [key]: typeof draft[key] === "string" ? draft[key].trim() : draft[key] });
    }
    for (const key of ["allowed_senders", "blocked_senders"] as const) if (changes[key] !== undefined) feishu[key] = draft[key].split(/\r?\n/).map((line) => line.trim()).filter(Boolean);
    if (draft.clearSecret) feishu.app_secret = { action: "clear" };
    else if (draft.secret) feishu.app_secret = { action: "replace", value: draft.secret };
    if (Object.keys(feishu).length) patch.feishu = feishu;
    return patch;
}

import type { ChannelPatch, ChannelSettings } from "./api/settings";
import { LocalizedError, translate } from "./i18n.ts";

export interface ChannelDraft { default_channel: "console" | "feishu"; enabled: boolean; app_id: string; domain: "feishu" | "lark"; owner_open_id: string; group_policy: "open" | "allowlist" | "disabled"; allow_unmentioned: boolean; allowed_senders: string; blocked_senders: string; secret: string; clearSecret: boolean }

export function channelInputs(view: ChannelSettings): ChannelDraft {
    const { feishu } = view.desired;
    return { default_channel: view.desired.default_channel, enabled: feishu.enabled, app_id: feishu.app_id || "", domain: feishu.domain || "feishu", owner_open_id: feishu.owner_open_id || "", group_policy: feishu.group_policy || "open", allow_unmentioned: feishu.allow_unmentioned, allowed_senders: (feishu.allowed_senders || []).join("\n"), blocked_senders: (feishu.blocked_senders || []).join("\n"), secret: "", clearSecret: false };
}

// A poll takes what changes without a save: the connection status and,
// since a channel that stops no longer applies access rules live, which
// fields apply without a restart. The rest of the view stays as last loaded:
// the drafts are based on it.
export function followChannelStatus(current: ChannelSettings, next: ChannelSettings): ChannelSettings {
    return { ...current, runtime_error: next.runtime_error, startup_retry: next.startup_retry, reconnect: next.reconnect, apply_mode: next.apply_mode, live_fields: next.live_fields };
}

// A retrying startup is read just after each scheduled attempt, within the
// bounds; a reconnect with each failed attempt the official client reports;
// a connected channel as often as the other management views refresh, so a
// later loss shows without a reload.
const retryPollSettle = 1500, retryPollMin = 2000, retryPollMax = 30000, reconnectPoll = 5000, connectedPoll = 30000;

export function channelPollDelay(retryAt: string | undefined, reconnecting: boolean, now = Date.now()): number {
    if (retryAt) {
        const due = Date.parse(retryAt) - now;
        return Number.isNaN(due) ? retryPollMax : Math.min(Math.max(due + retryPollSettle, retryPollMin), retryPollMax);
    }
    return reconnecting ? reconnectPoll : connectedPoll;
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

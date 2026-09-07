import { useEffect, useRef, useState } from "react";
import { useLocation, useSearchParams } from "react-router";
import { Globe01, Settings01, Server01, Sliders04 } from "@untitledui/icons";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { TextArea } from "@/components/base/textarea/textarea";
import { Toggle } from "@/components/base/toggle/toggle";
import { SettingsServices } from "@/components/steve/settings-services";
import { fetchChannels, fetchHubSettings, saveChannels, saveHubSettings, type ChannelSettings, type HubSettings, type SettingsField } from "@/lib/api/settings";
import { channelInputs, channelPatch, changedInputs, type ChannelDraft } from "@/lib/settings-channels";
import { number } from "@/lib/format";
import { HTTPError } from "@/lib/http";
import { errorText, type LocalePreference } from "@/lib/i18n";
import { settingGroups, settingValue, settingsInputs, settingsPatch, type SettingPath } from "@/lib/settings-values";
import { useI18n } from "@/providers/locale-provider";
import { useTheme } from "@/providers/theme-provider";

type Group = "hub" | "channels";
type Section = "general" | "channels" | "policies" | "services";
const sections = ["general", "channels", "policies", "services"] as const;
const icons = { general: Settings01, channels: Globe01, policies: Sliders04, services: Server01 };
// HashRouter's listener can unmount the form synchronously. Register this
// listener before the router mounts so a declined back navigation keeps drafts.
let guardBack: ((event: PopStateEvent) => void) | undefined;
window.addEventListener("popstate", (event) => guardBack?.(event), true);

export function SettingsPage() {
    const { t, locale, preference, setLocale } = useI18n();
    const { theme, setTheme } = useTheme();
    const [params, setParams] = useSearchParams();
    const location = useLocation();
    const chosen = params.get("section");
    const section: Section = sections.includes(chosen as Section) ? chosen as Section : "general";
    const [hub, setHub] = useState<HubSettings | null>(null), [channels, setChannels] = useState<ChannelSettings | null>(null);
    const [inputs, setInputs] = useState<Record<string, string>>({}), [channelDraft, setChannelDraft] = useState<ChannelDraft | null>(null);
    const [loading, setLoading] = useState<Record<Group, boolean>>({ hub: true, channels: true });
    const [errors, setErrors] = useState<Partial<Record<Group, unknown>>>({});
    const [stale, setStale] = useState<Record<Group, boolean>>({ hub: false, channels: false });
    const [notices, setNotices] = useState<Partial<Record<Group, "saved" | "review">>>({});
    const [saving, setSaving] = useState<Group | null>(null);
    const [confirmRead, setConfirmRead] = useState<{ group: Group; keep: boolean } | null>(null);
    const requests = useRef<Record<Group, number>>({ hub: 0, channels: 0 }), alive = useRef(true), sending = useRef(false);
    const dirtyHub = !!hub && Object.keys(changedInputs(settingsInputs(hub), inputs)).length > 0;
    const dirtyChannels = !!channels && !!channelDraft && Object.keys(changedInputs(channelInputs(channels), channelDraft)).length > 0;
    const dirty = dirtyHub || dirtyChannels;
    const latest = useRef({ hub, channels, inputs, channelDraft, dirtyHub, dirtyChannels });
    latest.current = { hub, channels, inputs, channelDraft, dirtyHub, dirtyChannels };
    const allowLeave = useRef(false);
    useEffect(() => {
        const href = window.location.href, historyState = window.history.state;
        const outside = (url: URL) => !/^#\/settings(?:\?|$)/.test(url.hash);
        const hasDrafts = () => latest.current.dirtyHub || latest.current.dirtyChannels;
        const beforeUnload = (event: BeforeUnloadEvent) => { if (hasDrafts() && !allowLeave.current) { event.preventDefault(); event.returnValue = ""; } };
        const click = (event: MouseEvent) => {
            if (!hasDrafts() || event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
            const anchor = (event.target as Element)?.closest?.("a[href]");
            if (!(anchor instanceof HTMLAnchorElement) || anchor.target === "_blank" || anchor.hasAttribute("download")) return;
            const url = new URL(anchor.href, window.location.href);
            if (url.origin === window.location.origin && url.pathname === window.location.pathname && !outside(url)) return;
            if (!window.confirm(t("settingsPage.leaveDrafts"))) { event.preventDefault(); event.stopPropagation(); }
            else allowLeave.current = true;
        };
        const pop = (event: PopStateEvent) => {
            if (!hasDrafts() || allowLeave.current || !outside(new URL(window.location.href))) return;
            if (window.confirm(t("settingsPage.leaveDrafts"))) { allowLeave.current = true; return; }
            event.stopImmediatePropagation();
            window.history.pushState(historyState, "", href);
        };
        guardBack = pop;
        window.addEventListener("beforeunload", beforeUnload); document.addEventListener("click", click, true);
        return () => { window.removeEventListener("beforeunload", beforeUnload); document.removeEventListener("click", click, true); if (guardBack === pop) guardBack = undefined; };
    }, [location.key, t]);

    async function read(group: Group, keep = false) {
        const revision = ++requests.current[group];
        const current = latest.current;
        setLoading((value) => ({ ...value, [group]: true }));
        try {
            if (group === "hub") {
                const next = await fetchHubSettings();
                if (!alive.current || revision !== requests.current[group]) return;
                const changes = keep && current.hub ? changedInputs(settingsInputs(current.hub), current.inputs) : {};
                setHub(next); setInputs(Object.assign(settingsInputs(next), changes));
            } else {
                const next = await fetchChannels();
                if (!alive.current || revision !== requests.current[group]) return;
                const changes = keep && current.channels && current.channelDraft ? changedInputs(channelInputs(current.channels), current.channelDraft) : {};
                setChannels(next); setChannelDraft({ ...channelInputs(next), ...changes });
            }
            setErrors((value) => ({ ...value, [group]: null })); setStale((value) => ({ ...value, [group]: false }));
            setNotices((value) => ({ ...value, [group]: keep ? "review" : undefined }));
        } catch (error) { if (alive.current && revision === requests.current[group]) setErrors((value) => ({ ...value, [group]: error })); }
        finally { if (alive.current && revision === requests.current[group]) setLoading((value) => ({ ...value, [group]: false })); }
    }
    useEffect(() => { alive.current = true; void read("hub"); void read("channels"); return () => { alive.current = false; requests.current.hub++; requests.current.channels++; }; }, []);
    async function save(group: Group) {
        if (sending.current || loading[group] || stale[group]) return;
        sending.current = true; setSaving(group); setErrors((all) => ({ ...all, [group]: null }));
        try {
            if (group === "hub" && hub) {
                const next = await saveHubSettings(hub.revision, settingsPatch(hub, inputs));
                if (!alive.current) return;
                setHub(next); setInputs(settingsInputs(next));
                if (latest.current.dirtyChannels) setStale((value) => ({ ...value, channels: true })); else void read("channels");
            } else if (group === "channels" && channels && channelDraft) {
                const next = await saveChannels(channels.revision, channelPatch(channels, channelDraft));
                if (!alive.current) return;
                setChannels(next); setChannelDraft(channelInputs(next));
                if (latest.current.dirtyHub) setStale((value) => ({ ...value, hub: true })); else void read("hub");
            }
            setNotices((value) => ({ ...value, [group]: "saved" }));
        } catch (error) {
            if (!alive.current) return;
            setErrors((all) => ({ ...all, [group]: error }));
            if (error instanceof HTTPError && error.status === 409) setStale((value) => ({ ...value, [group]: true }));
        } finally { sending.current = false; if (alive.current) setSaving(null); }
    }
    const fields = new Map((hub?.fields || []).map((field) => [field.path, field]));
    const blocked = !!saving || loading.hub || loading.channels;
    const group: Group = section === "channels" ? "channels" : "hub";
    const dirtyGroup = group === "hub" ? dirtyHub : dirtyChannels;
    const requestRead = (keep = false) => dirtyGroup ? setConfirmRead({ group, keep }) : void read(group, keep);
    const update = (path: string, value: string) => { setInputs((all) => ({ ...all, [path]: value })); setNotices((all) => ({ ...all, hub: undefined })); setErrors((all) => ({ ...all, hub: null })); };
    const rows = (paths: readonly SettingPath[]) => hub && paths.filter((path) => fields.has(path)).map((path) => <SettingRow key={path} path={path} field={fields.get(path)!} view={hub} value={inputs[path] ?? ""} disabled={blocked} onChange={(value) => update(path, value)} />);
    const groupStatus = section !== "services" && <>
        {!!errors[group] && <p role="alert" className="settings-alert">{errorText(errors[group], locale)}</p>}
        {stale[group] && <div role="alert" className="settings-conflict"><p>{t("settingsPage.sharedRevision")}</p><div className="settings-actions"><Button size="sm" color="secondary" isDisabled={blocked} onClick={() => requestRead(true)}>{t("settingsPage.reviewLatest")}</Button><Button size="sm" color="link-gray" isDisabled={blocked} onClick={() => requestRead()}>{t("settingsPage.discardReload")}</Button></div></div>}
        {notices[group] && <p role="status" className="settings-note">{t(notices[group] === "saved" ? "settingsPage.savedNotice" : "settingsPage.reviewedNotice")}</p>}
        {(group === "hub" ? hub?.warning : channels?.warning) && <p role="status" className="settings-conflict">{group === "hub" ? hub?.warning : channels?.warning}</p>}
    </>;
    const saveBar = (hub || channels) && <div className="settings-savebar"><span>{t(dirtyGroup ? "settingsPage.unsaved" : "settingsPage.serverSettingsHint")}</span><Button size="sm" color="secondary" isDisabled={blocked} onClick={() => requestRead()}>{t("settingsPage.reload")}</Button><Button size="sm" color="primary" isLoading={saving === group} isDisabled={!dirtyGroup || blocked || stale[group]} onClick={() => void save(group)}>{t(group === "hub" ? "settingsPage.saveHub" : "settingsPage.saveChannels")}</Button></div>;
    return <div className="workbench-page settings-page">
        <header className="settings-header"><h1>{t("settingsPage.centerTitle")}</h1><div role="status">{dirty && <span>{t("settingsPage.unsaved")}</span>}{(hub?.pending_restart || channels?.pending_restart) && <Badge size="sm" color="warning">{t("settingsPage.pendingRestart")}</Badge>}</div></header>
        <div className="settings-layout"><nav aria-label={t("settingsPage.categories")} className="settings-nav">{sections.map((item) => { const Icon = icons[item]; return <a key={item} aria-label={t(`settingsPage.section.${item}`)} href={`#/settings?section=${item}`} aria-current={item === section ? "page" : undefined} onClick={(event) => { event.preventDefault(); setParams({ section: item }); }}><Icon aria-hidden="true" /><span className="settings-nav-label">{t(`settingsPage.section.${item}`)}</span>{(item === "channels" ? dirtyChannels : item === "policies" || item === "general" ? dirtyHub : false) && <span className="settings-dirty-dot" aria-label={t("settingsPage.unsaved")} />}</a>; })}</nav>
        <main className="settings-content" aria-label={t(`settingsPage.section.${section}`)}>
            {groupStatus}
            {section === "general" && <>
                <div className="settings-section-heading"><div><h2>{t("settingsPage.section.general")}</h2><p>{t("settingsPage.localPreferences")}</p></div></div>
                <div className="settings-field"><div><label>{t("settings.language")}</label><p>{t("settingsPage.localLanguageHint")}</p></div><Select size="sm" aria-label={t("settings.language")} selectedKey={preference} onSelectionChange={(key) => { if (key) setLocale(String(key) as LocalePreference); }} items={[{ id: "system", label: t("settings.system") }, { id: "zh", label: "简体中文" }, { id: "en", label: "English" }]}>{(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}</Select></div>
                <div className="settings-field"><label>{t("settings.appearance")}</label><Select size="sm" aria-label={t("settings.appearance")} selectedKey={theme} onSelectionChange={(key) => { if (key) setTheme(String(key) as "system" | "light" | "dark"); }} items={(["system", "light", "dark"] as const).map((id) => ({ id, label: t(`settings.${id}`) }))}>{(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}</Select></div>
                <section className="settings-subsection"><h3>{t("settingsPage.hubDefaults")}</h3>{rows(["gateway.locale"])}</section>
                <details className="settings-advanced"><summary>{t("settingsPage.identityDetails")}</summary><dl><dt>{t("settingsPage.owner")}</dt><dd><code>{hub ? settingValue(hub.effective, "gateway.owner_id") || "—" : "—"}</code></dd><dt>{t("settingsPage.revision")}</dt><dd><code>{hub?.revision || "—"}</code></dd></dl><p>{t("settingsPage.ownerHint")}</p></details>
                {saveBar}
            </>}
            {section === "channels" && <><div className="settings-section-heading"><div><h2>{t("settingsPage.section.channels")}</h2><p>{t("settingsPage.channelScopeHint")}</p></div></div>{channels && channelDraft ? <ChannelForm view={channels} draft={channelDraft} disabled={blocked} onChange={(next) => { setChannelDraft(next); setNotices((all) => ({ ...all, channels: undefined })); setErrors((all) => ({ ...all, channels: null })); }} /> : <p className="settings-note">{t(loading.channels ? "common.loading" : "settingsPage.channelsUnavailable")}</p>}{saveBar}</>}
            {section === "policies" && <><div className="settings-section-heading"><div><h2>{t("settingsPage.section.policies")}</h2><p>{t("settingsPage.policyScopeHint")}</p></div></div><section><h3>{t("settingsPage.gateway")}</h3>{rows(settingGroups.gateway.filter((path) => path !== "gateway.locale"))}</section>{(["execution", "planning", "snapshot", "review"] as const).map((name) => <details key={name} className="settings-advanced"><summary>{t(`settingsPage.${name}`)}</summary>{rows(settingGroups[name])}</details>)}{saveBar}</>}
            {section === "services" && <SettingsServices onRestarted={(service) => { if (service === "hub") { if (!latest.current.dirtyHub) void read("hub"); if (!latest.current.dirtyChannels) void read("channels"); } }} />}
            {section !== "services" && !hub && <p role="status" className="settings-note">{t(loading.hub ? "common.loading" : "settingsPage.readFailed")}</p>}
        </main></div>
        {confirmRead && <ModalOverlay isOpen isDismissable onOpenChange={(open) => { if (!open) setConfirmRead(null); }}><Modal className="max-w-md"><Dialog aria-label={t(confirmRead.keep ? "settingsPage.reviewLatest" : "settingsPage.reloadTitle")}><div className="settings-dialog"><h2>{t(confirmRead.keep ? "settingsPage.reviewLatest" : "settingsPage.reloadTitle")}</h2><p>{t(confirmRead.keep ? "settingsPage.mergeHint" : "settingsPage.reloadHint")}</p><div className="settings-actions"><Button size="sm" color="secondary" onClick={() => setConfirmRead(null)}>{t("common.cancel")}</Button><Button size="sm" color="primary" onClick={() => { const action = confirmRead; setConfirmRead(null); void read(action.group, action.keep); }}>{t(confirmRead.keep ? "settingsPage.keepAndRead" : "settingsPage.confirmReload")}</Button></div></div></Dialog></Modal></ModalOverlay>}
    </div>;
}

function SettingRow({ path, field, view, value, disabled, onChange }: { path: SettingPath; field: SettingsField; view: HubSettings; value: string; disabled: boolean; onChange: (value: string) => void }) {
    const { t, locale } = useI18n();
    const id = "setting-" + path.replaceAll(".", "-"), label = t(`settingsPage.${path}`);
    const saved = settingValue(view.desired, path), effective = settingValue(view.effective, path), edited = value !== String(saved ?? "");
    const format = (item: string | number | undefined) => item === "" ? t("settingsPage.automaticLocale") : typeof item === "number" ? number(item, locale) : item ?? "—";
    return <div className="settings-field" data-setting={path}>
        <div><label htmlFor={id}>{label}</label>{path.startsWith("gateway.") && <p>{t(`settingsPage.${path as Extract<SettingPath, `gateway.${string}`>}.hint`)}</p>}{(saved !== effective || edited) && <p className="settings-difference">{t("settingsPage.saved")}: <span data-desired>{format(saved)}</span>{saved !== effective && <> · {t("settingsPage.effective")}: <span data-effective>{format(effective)}</span></>}</p>}</div>
        <div>{field.enum ? <Select size="sm" id={id} aria-label={label} selectedKey={value || "__default"} isDisabled={disabled} onSelectionChange={(key) => { if (key) onChange(key === "__default" ? "" : String(key)); }} items={field.enum.map((option) => ({ id: option || "__default", label: option === "" ? t("settingsPage.automaticLocale") : option === "zh" ? "简体中文" : option === "en" ? "English" : option }))}>{(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}</Select> : <Input size="sm" id={id} aria-label={label} type="text" inputMode={field.type === "integer" ? "numeric" : "text"} autoComplete="off" spellCheck="false" value={value} isDisabled={disabled} onChange={onChange} />}
        <details className="settings-field-help"><summary>{t("settingsPage.fieldHelp")}</summary><p>{field.type === "duration" ? t(field.minimum === 0 ? "settingsPage.nonnegativeDuration" : "settingsPage.positiveDuration") : field.type === "integer" ? t("settingsPage.range", { minimum: number(field.minimum ?? 0, locale), maximum: field.maximum === undefined ? "—" : number(field.maximum, locale) }) : ""}{field.unit === "bytes" ? " · " + t("settingsPage.bytes") : ""}</p>{field.default !== undefined && <p>{t("settingsPage.default", { value: format(field.default) })}</p>}</details></div>
    </div>;
}

function ChannelForm({ view, draft, disabled, onChange }: { view: ChannelSettings; draft: ChannelDraft; disabled: boolean; onChange: (value: ChannelDraft) => void }) {
    const { t } = useI18n();
    const update = (patch: Partial<ChannelDraft>) => onChange({ ...draft, ...patch });
    const field = (name: "app_id" | "owner_open_id" | "allowed_senders" | "blocked_senders") => <div className="settings-field"><div><label htmlFor={`channel-${name}`}>{t(`settingsPage.channel.${name}`)}</label>{name.endsWith("senders") && <p>{t("settingsPage.senderHint")}</p>}</div>{name.endsWith("senders") ? <TextArea id={`channel-${name}`} aria-label={t(`settingsPage.channel.${name}`)} value={draft[name]} onChange={(value) => update({ [name]: value })} rows={3} isDisabled={disabled} /> : <Input id={`channel-${name}`} aria-label={t(`settingsPage.channel.${name}`)} value={draft[name]} onChange={(value) => update({ [name]: value })} size="sm" isDisabled={disabled} autoComplete="off" />}</div>;
    return <>
        <div className="settings-channel-summary"><div><strong>Console</strong><p>{t("settingsPage.consoleAlways")}</p></div><Badge size="sm" color="gray">{t("settingsPage.enabled")}</Badge></div>
        {view.runtime_error && <div className="settings-conflict" role="alert"><p>{t("settingsPage.channelStartupFailed")}</p><details><summary>{t("settingsPage.errorDetails")}</summary><p>{view.runtime_error}</p></details></div>}
        <div className="settings-field"><div><label>{t("settingsPage.defaultChannel")}</label><p>{t("settingsPage.defaultChannelHint")}</p></div><Select size="sm" aria-label={t("settingsPage.defaultChannel")} selectedKey={draft.default_channel} isDisabled={disabled} onSelectionChange={(key) => { if (key) update({ default_channel: String(key) as "console" | "feishu" }); }} items={[{ id: "console", label: "Console" }, { id: "feishu", label: "Feishu / Lark" }]}>{(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}</Select></div>
        <section className="settings-subsection"><div className="settings-channel-summary"><div><h3>Feishu / Lark</h3><p>{t(view.effective.feishu.enabled ? "settingsPage.channelEffectiveEnabled" : "settingsPage.channelEffectiveDisabled")}{view.desired.feishu.enabled !== view.effective.feishu.enabled && " · " + t("settingsPage.pendingRestart")}</p></div><Toggle size="sm" aria-label={t("settingsPage.enableFeishu")} isSelected={draft.enabled} isDisabled={disabled} onChange={(enabled) => update({ enabled })} /></div>
        <div className="settings-field"><label>{t("settingsPage.channel.domain")}</label><Select size="sm" aria-label={t("settingsPage.channel.domain")} selectedKey={draft.domain} isDisabled={disabled} onSelectionChange={(key) => { if (key) update({ domain: String(key) as "feishu" | "lark" }); }} items={[{ id: "feishu", label: "Feishu · open.feishu.cn" }, { id: "lark", label: "Lark · open.larksuite.com" }]}>{(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}</Select></div>
        {field("app_id")}
        <div className="settings-field"><div><label htmlFor="channel-secret">App Secret</label><p>{t(view.desired.feishu.app_secret_configured ? "settingsPage.secretConfigured" : "settingsPage.secretMissing")}</p><p>{t("settingsPage.secretHint")}</p></div><div><Input id="channel-secret" aria-label="App Secret" size="sm" type="password" value={draft.secret} isDisabled={disabled || draft.clearSecret} onChange={(secret) => update({ secret })} autoComplete="new-password" /><Toggle size="sm" className="mt-3" label={t("settingsPage.clearSecret")} isSelected={draft.clearSecret} isDisabled={disabled || !view.desired.feishu.app_secret_configured} onChange={(clearSecret) => update({ clearSecret, secret: "" })} /></div></div>
        {field("owner_open_id")}
        <details className="settings-advanced"><summary>{t("settingsPage.accessRules")}</summary><div className="settings-field"><label>{t("settingsPage.channel.group_policy")}</label><Select size="sm" aria-label={t("settingsPage.channel.group_policy")} selectedKey={draft.group_policy} isDisabled={disabled} onSelectionChange={(key) => { if (key) update({ group_policy: String(key) as ChannelDraft["group_policy"] }); }} items={(["open", "allowlist", "disabled"] as const).map((id) => ({ id, label: t(`settingsPage.groupPolicy.${id}`) }))}>{(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}</Select></div><div className="settings-field"><div><label>{t("settingsPage.allowUnmentioned")}</label><p>{t("settingsPage.unmentionedHint")}</p></div><Toggle size="sm" aria-label={t("settingsPage.allowUnmentioned")} isSelected={draft.allow_unmentioned} isDisabled={disabled} onChange={(allow_unmentioned) => update({ allow_unmentioned })} /></div>{field("allowed_senders")}{field("blocked_senders")}</details>
        </section>
    </>;
}

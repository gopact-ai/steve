import { useEffect, useRef, useState } from "react";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { KeyValue, PageBody, PageHeader } from "@/components/steve/page";
import { useResourceRead } from "@/hooks/use-resource-read";
import { fetchHubSettings, fetchVersions, saveHubSettings, type HubSettings, type SettingsField, type Versions } from "@/lib/api/settings";
import { number } from "@/lib/format";
import { HTTPError } from "@/lib/http";
import { errorText } from "@/lib/i18n";
import { editableSettings, settingGroups, settingValue, settingsInputs, settingsPatch, type SettingPath } from "@/lib/settings-values";
import { useI18n } from "@/providers/locale-provider";

export function SettingsPage() {
    const { t, locale } = useI18n();
    const [view, setView] = useState<HubSettings | null>(null);
    const [inputs, setInputs] = useState<Record<string, string>>({});
    const [error, setError] = useState<unknown>(null);
    const [loading, setLoading] = useState(true);
    const [saving, setSaving] = useState(false);
    const [saved, setSaved] = useState(false);
    const [conflict, setConflict] = useState(false);
    const [confirmReload, setConfirmReload] = useState(false);
    const sending = useRef(false);
    const read = useResourceRead("hub-settings", fetchHubSettings, (next) => {
        setView(next); setInputs(settingsInputs(next)); setConflict(false); setError(null); setLoading(false);
    }, (failure) => { setError(failure); setLoading(false); });
    useEffect(() => { void read(); }, [read]);
    const baseline = view ? settingsInputs(view) : {};
    const dirty = Object.keys(inputs).some((path) => inputs[path] !== baseline[path]);
    useEffect(() => {
        if (!dirty) return;
        const warn = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ""; };
        window.addEventListener("beforeunload", warn);
        return () => window.removeEventListener("beforeunload", warn);
    }, [dirty]);
    function reload() { setLoading(true); setSaved(false); setConfirmReload(false); void read(); }
    async function save() {
        if (!view || !dirty || conflict || sending.current) return;
        setError(null); setSaved(false);
        try {
            const patch = settingsPatch(view, inputs);
            sending.current = true; setSaving(true);
            const next = await saveHubSettings(view.revision, patch);
            setView(next); setInputs(settingsInputs(next)); setSaved(true);
        } catch (failure) {
            setError(failure);
            if (failure instanceof HTTPError && failure.status === 409) setConflict(true);
        } finally { sending.current = false; setSaving(false); }
    }
    const fields = new Map((view?.fields || []).filter((field) => editableSettings.has(field.path)).map((field) => [field.path, field]));
    return <div className="workbench-page flex min-w-0 flex-col">
        <PageHeader title={t("settingsPage.title")} description={t("settingsPage.description")} actions={<>
            <Button size="sm" color="secondary" isDisabled={saving || loading} onClick={() => dirty ? setConfirmReload(true) : reload()}>{t("settingsPage.reload")}</Button>
            <Button size="sm" color="primary" isDisabled={!view || !dirty || conflict || loading} isLoading={saving} onClick={() => void save()}>{t("settingsPage.save")}</Button>
        </>} />
        <PageBody className="max-w-5xl">
            <p className="text-sm leading-6 text-tertiary">{t("settingsPage.restartHint")}</p>
            {conflict && <div role="alert" className="rounded-lg bg-warning-primary p-4 text-sm text-warning-primary">{t("settingsPage.conflict")}</div>}
            {!!error && <div role="alert" className="flex flex-wrap items-center gap-3 text-sm text-error-primary"><span>{view ? "" : t("settingsPage.readFailed") + ": "}{errorText(error, locale)}</span>{!view && <Button size="sm" color="secondary" isLoading={loading} onClick={reload}>{t("common.retry")}</Button>}</div>}
            {saved && <p role="status" className="text-sm text-secondary">{t("settingsPage.savedNotice")}</p>}
            {view?.warning && <p role="status" className="rounded-lg bg-warning-primary p-3 text-sm text-warning-primary">{view.warning}</p>}
            {view ? <>
                <div className="flex flex-wrap items-center gap-3 border-b border-secondary pb-4 text-xs text-tertiary">
                    <Badge size="sm" color={view.pending_restart ? "warning" : "gray"}>{t(view.pending_restart ? "settingsPage.pendingRestart" : "settingsPage.inSync")}</Badge>
                    {dirty && <span>{t("settingsPage.unsaved")}</span>}
                    <span className="ml-auto min-w-0 truncate" title={view.revision}>{t("settingsPage.revision")} <code>{view.revision.slice(0, 12)}</code></span>
                </div>
                <form onSubmit={(event) => { event.preventDefault(); void save(); }}>
                    <fieldset disabled={saving || loading} className="flex min-w-0 flex-col gap-8">
                        {Object.entries(settingGroups).map(([group, paths]) => {
                            const shown = paths.filter((path) => fields.has(path));
                            return shown.length ? <section key={group} className="min-w-0">
                                <h2 className="mb-1 text-base font-semibold text-primary">{t(`settingsPage.${group as keyof typeof settingGroups}`)}</h2>
                                <div className="divide-y divide-secondary">{shown.map((path) => <SettingRow key={path} path={path} field={fields.get(path)!} view={view} value={inputs[path] ?? ""} disabled={saving || loading} onChange={(value) => { setInputs((current) => ({ ...current, [path]: value })); setError(null); setSaved(false); }} />)}</div>
                            </section> : null;
                        })}
                        {!fields.size && <p className="text-sm text-tertiary">{t("settingsPage.noFields")}</p>}
                    </fieldset>
                </form>
                <section className="border-t border-secondary pt-5"><h2 className="text-sm font-semibold text-primary">{t("settingsPage.owner")}</h2><code className="mt-2 block break-all text-sm text-secondary">{settingValue(view.effective, "gateway.owner_id") || "—"}</code><p className="mt-1 text-xs text-tertiary">{t("settingsPage.ownerHint")}</p></section>
            </> : loading && <p role="status" className="text-sm text-tertiary">{t("settingsPage.loading")}</p>}
            <VersionSection />
        </PageBody>
        {confirmReload && <ModalOverlay isOpen isDismissable onOpenChange={(open) => setConfirmReload(open)}><Modal className="max-w-md"><Dialog aria-label={t("settingsPage.reloadTitle")}>
            <div className="w-full rounded-xl bg-primary p-6 shadow-lg"><h2 className="text-base font-semibold text-primary">{t("settingsPage.reloadTitle")}</h2><p className="mt-3 text-sm leading-6 text-secondary">{t("settingsPage.reloadHint")}</p><div className="mt-5 flex flex-wrap justify-end gap-2"><Button size="sm" color="secondary" onClick={() => setConfirmReload(false)}>{t("common.cancel")}</Button><Button size="sm" color="primary" onClick={reload}>{t("settingsPage.confirmReload")}</Button></div></div>
        </Dialog></Modal></ModalOverlay>}
    </div>;
}

function SettingRow({ path, field, view, value, disabled, onChange }: { path: SettingPath; field: SettingsField; view: HubSettings; value: string; disabled: boolean; onChange: (value: string) => void }) {
    const { t, locale } = useI18n();
    const label = t(`settingsPage.${path}`);
    const id = "setting-" + path.replaceAll(".", "-");
    const stored = settingValue(view.desired, path);
    const effective = settingValue(view.effective, path);
    const format = (item: string | number | undefined) => item === "" ? t("settingsPage.automaticLocale") : typeof item === "number" ? number(item, locale) : item ?? "—";
    const hint = path.startsWith("gateway.") ? t(`settingsPage.${path as Extract<SettingPath, `gateway.${string}`>}.hint`) : "";
    const unit = field.unit === "bytes" ? t("settingsPage.bytes") : field.unit === "count" ? t("settingsPage.count") : "";
    const range = field.type === "duration" ? t(field.minimum === 0 ? "settingsPage.nonnegativeDuration" : "settingsPage.positiveDuration") : field.type === "integer" ? t("settingsPage.range", { minimum: number(field.minimum ?? 0, locale), maximum: field.maximum === undefined ? "—" : number(field.maximum, locale) }) : "";
    return <div className="grid min-w-0 gap-3 py-4 sm:grid-cols-[minmax(0,1fr)_minmax(14rem,20rem)] sm:gap-6" data-setting={path}>
        <div className="min-w-0"><label htmlFor={id} className="text-sm font-medium text-primary">{label}</label>{hint && <p className="mt-1 text-xs leading-5 text-tertiary">{hint}</p>}<p className="mt-2 break-words text-xs text-tertiary">{t("settingsPage.effective")}: <span data-effective className="font-mono text-secondary">{format(effective)}</span>{stored !== effective && <> · {t("settingsPage.saved")}: <span data-desired className="font-mono text-secondary">{format(stored)}</span></>}</p></div>
        <div className="min-w-0">
            {field.enum ? <Select size="sm" id={id} aria-label={label} selectedKey={value || "__default"} isDisabled={disabled} onSelectionChange={(key) => onChange(key === "__default" ? "" : String(key))} items={field.enum.map((option) => ({ id: option || "__default", label: option === "" ? t("settingsPage.automaticLocale") : option === "zh" ? "简体中文" : option === "en" ? "English" : option }))}>{(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}</Select>
                : <Input size="sm" id={id} aria-label={label} type="text" inputMode={field.type === "integer" ? "numeric" : "text"} autoComplete="off" spellCheck="false" value={value} isDisabled={disabled} onChange={onChange} />}
            <div className="mt-1 flex flex-col gap-1 text-xs leading-5 text-tertiary">{range && <span>{range}{unit ? " · " + unit : ""}</span>}{field.default !== undefined && <span>{t("settingsPage.default", { value: format(field.default) })}</span>}</div>
        </div>
    </div>;
}

function VersionSection() {
    const { t } = useI18n();
    const [view, setView] = useState<Versions | null>(null);
    const [error, setError] = useState("");
    const [loading, setLoading] = useState(true);
    const read = useResourceRead("versions", fetchVersions, (next) => { setView(next); setError(""); setLoading(false); }, (failure) => { setError(failure instanceof Error ? failure.message : String(failure)); setLoading(false); });
    useEffect(() => { void read(); }, [read]);
    return <section className="min-w-0 border-t border-secondary pt-6">
        <div className="flex items-center justify-between gap-4"><h2 className="text-base font-semibold text-primary">{t("settingsPage.versions")}</h2><Button size="sm" color="secondary" isLoading={loading} onClick={() => { setLoading(true); void read(); }}>{t("settingsPage.reload")}</Button></div>
        {error && <p role="alert" className="mt-3 text-sm text-error-primary">{t("settingsPage.versionsFailed")}: {error}</p>}
        {view && <>
            <div className="my-4"><KeyValue rows={[{ k: t("settingsPage.hubId"), v: <code>{view.hub_id}</code> }, { k: t("settingsPage.hubVersion"), v: <code>{view.hub || "—"}</code> }, { k: t("settingsPage.protocol"), v: `${view.protocol_min}–${view.protocol_max}` }]} /></div>
            {!view.automatic && <p className="mb-2 text-xs leading-5 text-tertiary">{t("settingsPage.manualUpdates")}</p>}
            <p className="mb-4 text-xs leading-5 text-tertiary">{t(view.discovery_configured ? "settingsPage.discoveryConfigured" : "settingsPage.discoveryUnavailable")}</p>
            {view.nodes?.length ? <div className="min-w-0 overflow-x-auto"><table className="w-full text-left text-sm" aria-label={t("settingsPage.versions")}><thead className="border-b border-secondary text-xs text-tertiary"><tr>{["node", "version", "protocol", "system"].map((key) => <th key={key} className="px-3 py-2 font-medium">{t(`settingsPage.${key as "node" | "version" | "protocol" | "system"}`)}</th>)}</tr></thead><tbody>{view.nodes.map((node) => <tr key={node.name} className="border-b border-secondary"><th scope="row" className="px-3 py-3 font-medium text-primary">{node.name}<span className="mt-1 block text-xs font-normal text-tertiary">{t(node.online ? "settingsPage.online" : "settingsPage.offline")}</span></th><td className="px-3 py-3"><code>{node.version || "—"}</code><span className={`mt-1 block text-xs ${node.matches_hub ? "text-tertiary" : "text-warning-primary"}`}>{t(node.matches_hub ? "settingsPage.matches" : "settingsPage.differs")}</span></td><td className="px-3 py-3" title={node.features?.join(", ")}>{node.protocol || "—"}</td><td className="px-3 py-3 text-xs text-tertiary">{node.os} / {node.arch}</td></tr>)}</tbody></table></div> : <p className="text-sm text-tertiary">{t("settingsPage.noNodes")}</p>}
            {!!view.latest?.length && <div className="mt-4"><h3 className="mb-2 text-sm font-medium text-primary">{t("settingsPage.latest")}</h3><ul className="space-y-1 text-xs text-secondary">{view.latest.map((release) => <li key={[release.component, release.version, release.os, release.arch].join(":")}>{release.component} · {release.version} · {release.os}/{release.arch}</li>)}</ul></div>}
            <details className="mt-5 border-t border-secondary pt-4">
                <summary className="cursor-pointer text-sm font-medium text-primary">{t("settingsPage.ownership")}</summary>
                <p className="my-3 text-xs leading-5 text-tertiary">{t("settingsPage.ownershipHint")} <a href="https://github.com/gopact-ai/steve/blob/master/docs/operations.md" target="_blank" rel="noreferrer" className="underline">{t("settingsPage.migrationGuide")}</a></p>
                {view.projects?.length ? <div className="min-w-0 overflow-x-auto"><table className="w-full text-left text-sm" aria-label={t("settingsPage.ownership")}><thead className="border-b border-secondary text-xs text-tertiary"><tr>{["project", "managingHub", "epoch", "ownershipState", "targetHub"].map((key) => <th key={key} className="px-3 py-2 font-medium">{t(`settingsPage.${key as "project" | "managingHub" | "epoch" | "ownershipState" | "targetHub"}`)}</th>)}</tr></thead><tbody>{view.projects.map((owner) => <tr key={owner.project} className="border-b border-secondary"><th scope="row" className="px-3 py-3 font-medium text-primary">{owner.project}</th><td className="px-3 py-3"><code>{owner.hub_id || "—"}</code></td><td className="px-3 py-3 font-mono">{owner.epoch}</td><td className="px-3 py-3" title={owner.state}>{owner.state === "active" || owner.state === "released" || owner.state === "importing" ? t(`settingsPage.owner.${owner.state}`) : owner.state || t("common.unknown")}</td><td className="px-3 py-3"><code>{owner.target_hub || "—"}</code>{owner.transfer_id && <span className="mt-1 block text-xs text-tertiary">{t("settingsPage.transfer")}: <code>{owner.transfer_id}</code></span>}</td></tr>)}</tbody></table></div> : <p className="text-sm text-tertiary">{t(view.projects == null ? "settingsPage.ownershipUnavailable" : "settingsPage.noOwnership")}</p>}
            </details>
            <details className="mt-4 border-t border-secondary pt-4">
                <summary className="cursor-pointer text-sm font-medium text-primary">{t("settingsPage.peers")}</summary>
                <p className="my-3 text-xs leading-5 text-tertiary">{t("settingsPage.peersHint")}</p>
                {view.peers?.length ? <ul className="divide-y divide-secondary">{view.peers.map((peer) => <li key={peer.id} className="flex min-w-0 flex-col gap-1 py-3"><div className="flex flex-wrap items-baseline gap-2"><code className="text-sm text-primary">{peer.id}</code>{peer.name && <span className="text-xs text-secondary">{peer.name}</span>}</div><code className="break-all text-xs text-tertiary">{peer.url}</code></li>)}</ul> : <p className="text-sm text-tertiary">{t(view.peers == null ? "settingsPage.peersUnavailable" : "settingsPage.noPeers")}</p>}
            </details>
        </>}
    </section>;
}

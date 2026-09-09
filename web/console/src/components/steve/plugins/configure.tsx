import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/base/buttons/button";
import { Checkbox } from "@/components/base/checkbox/checkbox";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { Toggle } from "@/components/base/toggle/toggle";
import { fetchPluginSecrets, fetchPlugins, savePlugin } from "@/lib/api/plugins";
import type { PluginConfiguration, PluginInstallation, PluginInstallationView, PluginPackage, SecretInfo } from "@/lib/plugin-types";
import { useI18n } from "@/providers/locale-provider";
import { CapabilitySummary, ManifestDetails, PluginSheet, pluginError, sameDeclaration } from "./shared";

export function PluginConfigure({ packages, item, initial, revision, nodes, onClose, onSaved }: { packages: PluginPackage[]; item?: PluginInstallationView; initial: PluginPackage; revision: string; nodes: { id: string; label: string }[]; onClose: () => void; onSaved: (warning?: string) => void }) {
    const { t } = useI18n(); const [baseRevision, setBaseRevision] = useState(revision); const [id, setId] = useState(item?.id ?? initial.manifest.id.replaceAll("/", "-"));
    const [draft, setDraft] = useState<PluginInstallation>(() => item ? structuredClone(item.installation) : { package_id: initial.manifest.id, digest: initial.digest, enabled: false, projects: [initial.project], targets: {} });
    const [busy, setBusy] = useState(false); const running = useRef(false); const [error, setError] = useState("");
    const [reviewedDigest, setReviewedDigest] = useState("");
    const current = packages.find((p) => p.digest === draft.digest) ?? initial;
    const versions = [...new Map(packages.filter((p) => p.manifest.id === initial.manifest.id).map((p) => [p.digest, p])).values()];
    const projects = [...new Set(packages.filter((p) => p.digest === draft.digest).map((p) => p.project))];
    const dirty = !sameDeclaration(item?.installation, draft) || id !== item?.id;
    const changeNode = (node: string, selected: boolean) => setDraft((before) => { const targets = { ...before.targets }; if (selected) targets[node] = {}; else delete targets[node]; return { ...before, targets }; });
    async function save() {
        if (running.current) return;
        if (!draft.projects.length || !Object.keys(draft.targets).length) { setError(t("plugins.selectRequired")); return; }
        if (item && draft.digest !== item.installation.digest && draft.enabled && reviewedDigest !== draft.digest) { setError(t("plugins.reviewRequired")); return; }
        running.current = true; setBusy(true); setError("");
        try { const result = await savePlugin(id, baseRevision, draft); onSaved(result.warning); }
        catch (error) {
            try { const latest = await fetchPlugins(); const saved = latest.installations.find((entry) => entry.id === id); if (saved && sameDeclaration(saved.installation, draft)) { onSaved(); return; } } catch { /* Keep the draft and original revision until the result can be verified. */ }
            setError(pluginError(error));
        } finally { running.current = false; setBusy(false); }
    }
    return <PluginSheet title={t("plugins.configure")} dirty={dirty} busy={busy} onClose={onClose}>
        <form className="flex min-w-0 flex-col gap-5" onSubmit={(event) => { event.preventDefault(); void save(); }}>
            <fieldset disabled={busy} className="flex min-w-0 flex-col gap-4">
                <Input name="installation-id" autoComplete="off" spellCheck="false" label={t("plugins.installationId")} hint={t("plugins.installationHint")} value={id} onChange={setId} isRequired isDisabled={!!item} />
                <Select size="sm" label={t("plugins.version")} hint={t("plugins.versionHint")} selectedKey={draft.digest} onSelectionChange={(key) => setDraft((before) => {
                    const next = packages.find((item) => item.digest === key); const settings = next?.manifest.settings ?? {};
                    const targets = Object.fromEntries(Object.entries(before.targets).map(([node, cfg]) => [node, { values: Object.fromEntries(Object.entries(cfg.values ?? {}).filter(([name]) => settings[name] && !settings[name].secret)), secrets: Object.fromEntries(Object.entries(cfg.secrets ?? {}).filter(([name]) => settings[name]?.secret)) }]));
                    return { ...before, digest: String(key), targets, projects: [...new Set(packages.filter((p) => p.digest === key && before.projects.includes(p.project)).map((p) => p.project))] };
                })} items={versions.map((p) => ({ id: p.digest, label: p.manifest.version, supportingText: p.digest.slice(0, 12) }))}>{(item) => <Select.Item {...item} />}</Select>
                <CapabilitySummary manifest={current.manifest} />
                <ManifestDetails manifest={current.manifest} />
                {item && draft.digest !== item.installation.digest ? <div className="flex min-w-0 flex-col gap-3 rounded-lg border border-secondary p-3">
                    <PackageChanges before={initial.manifest} after={current.manifest} />
                    <Checkbox label={t("plugins.reviewUpgrade")} isSelected={reviewedDigest === draft.digest} onChange={(value) => setReviewedDigest(value ? draft.digest : "")} />
                </div> : null}
                <fieldset className="flex flex-col gap-2"><legend className="mb-2 text-sm font-medium text-secondary">{t("plugins.projects")}</legend>{projects.map((project) => <Checkbox key={project} label={project} isSelected={draft.projects.includes(project)} onChange={(selected) => setDraft((before) => ({ ...before, projects: selected ? [...before.projects, project] : before.projects.filter((id) => id !== project) }))} />)}</fieldset>
                <fieldset className="flex flex-col gap-3"><legend className="mb-2 text-sm font-medium text-secondary">{t("plugins.machines")}</legend>{nodes.map((node) => <div key={node.id} className="flex min-w-0 flex-col gap-3 rounded-lg border border-secondary p-3"><Checkbox label={node.label} isSelected={Object.hasOwn(draft.targets, node.id)} onChange={(selected) => changeNode(node.id, selected)} />{Object.hasOwn(draft.targets, node.id) ? <NodeConfiguration node={node.id} settings={current.manifest.settings ?? {}} configuration={draft.targets[node.id]} onChange={(next) => setDraft((before) => ({ ...before, targets: { ...before.targets, [node.id]: next } }))} /> : null}</div>)}</fieldset>
                <Toggle label={t("plugins.enable")} hint={t("plugins.installationsHint")} isSelected={draft.enabled} onChange={(enabled) => setDraft((before) => ({ ...before, enabled }))} />
            </fieldset>
            {error ? <div role="alert" className="rounded-lg bg-error-primary p-3 text-sm text-error-primary">{error}</div> : null}
            {error ? <Button size="sm" color="secondary" onClick={() => { if (window.confirm(t("plugins.unsaved"))) void fetchPlugins().then((latest) => { const current = latest.installations.find((entry) => entry.id === id); if (current) setDraft(structuredClone(current.installation)); setBaseRevision(latest.revision); setError(""); }).catch((error) => setError(pluginError(error))); }}>{t("plugins.refresh")}</Button> : null}
            <Button size="sm" type="submit" color="primary" isLoading={busy}>{t("plugins.save")}</Button>
        </form>
    </PluginSheet>;
}

function NodeConfiguration({ node, settings, configuration, onChange }: { node: string; settings: NonNullable<PluginPackage["manifest"]["settings"]>; configuration: PluginConfiguration; onChange: (value: PluginConfiguration) => void }) {
    const { t } = useI18n(); const [secrets, setSecrets] = useState<SecretInfo[]>([]); const [error, setError] = useState(""); const [refresh, setRefresh] = useState(0);
    const needsSecrets = Object.values(settings).some((field) => field.secret);
    useEffect(() => { if (!needsSecrets) return; const controller = new AbortController(); void fetchPluginSecrets(node, controller.signal).then((items) => { if (!controller.signal.aborted) { setSecrets(items); setError(""); } }).catch((error) => { if (!controller.signal.aborted) setError(pluginError(error)); }); return () => controller.abort(); }, [node, needsSecrets, refresh]);
    return <div className="flex min-w-0 flex-col gap-3 border-t border-secondary pt-3">
        {Object.entries(settings).map(([name, field]) => field.secret ? <Select key={name} size="sm" label={name} hint={field.description} isRequired={field.required} placeholder={t("plugins.selectSecret")} selectedKey={configuration.secrets?.[name] ? `${configuration.secrets[name].name}:${configuration.secrets[name].revision}` : null} onSelectionChange={(key) => { const selected = secrets.find((entry) => `${entry.reference.name}:${entry.reference.revision}` === key); const next = { ...configuration.secrets }; if (selected) next[name] = selected.reference; else delete next[name]; onChange({ ...configuration, secrets: next }); }} items={secrets.map((item) => ({ id: `${item.reference.name}:${item.reference.revision}`, label: item.reference.name, supportingText: item.reference.revision.slice(0, 12) }))}>{(item) => <Select.Item {...item} />}</Select> : <Input key={name} name={`plugin-setting-${name}`} autoComplete="off" spellCheck="false" label={name} hint={field.description} isRequired={field.required} value={configuration.values?.[name] ?? field.default ?? ""} onChange={(value) => onChange({ ...configuration, values: { ...configuration.values, [name]: value } })} />)}
        {!Object.keys(settings).length ? <p className="text-xs text-tertiary">{t("plugins.noSettings")}</p> : <p className="text-xs text-tertiary">{t("plugins.settingsHint")}</p>}
        {needsSecrets ? <><p className="text-xs text-tertiary">{t("plugins.secretHint")}</p>{!secrets.length ? <p className="text-xs text-warning-primary">{t("plugins.noSecrets")}</p> : null}<Button size="sm" color="link-gray" onClick={() => setRefresh((value) => value + 1)}>{t("plugins.refresh")}</Button></> : null}
        {error ? <div role="alert" className="text-xs text-error-primary">{error}</div> : null}
    </div>;
}

function PackageChanges({ before, after }: { before: PluginPackage["manifest"]; after: PluginPackage["manifest"] }) {
    const { t } = useI18n();
    const changes = (["skills", "mcp", "agents", "settings"] as const).flatMap((kind) => [...new Set([...Object.keys(before[kind] ?? {}), ...Object.keys(after[kind] ?? {})])].sort().filter((name) => !sameDeclaration(before[kind]?.[name], after[kind]?.[name])).map((name) => ({ kind, name, old: before[kind]?.[name], next: after[kind]?.[name] })));
    return <div className="flex min-w-0 flex-col gap-2"><p className="text-sm font-medium text-secondary">{t("plugins.versionChanges", { before: before.version, after: after.version })}</p><p className="text-xs text-tertiary">{t("plugins.contentChanged")}</p>{changes.map((change) => <details key={`${change.kind}/${change.name}`} className="min-w-0 text-xs"><summary className="cursor-pointer break-words text-secondary focus-visible:outline-2">{change.kind} · {change.name}</summary><pre className="mt-2 whitespace-pre-wrap break-all text-tertiary">{JSON.stringify(change.old ?? null, null, 2)} → {JSON.stringify(change.next ?? null, null, 2)}</pre></details>)}</div>;
}

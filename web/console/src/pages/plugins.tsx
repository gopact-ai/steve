import { useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router";
import { Package, Plus, RefreshCw01 } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Chips, PageBody, PageHeader, Panel } from "@/components/steve/page";
import { Nothing } from "@/components/steve/ui";
import { PluginUsageDrawer } from "@/components/steve/plugins/usage";
import { PluginImport } from "@/components/steve/plugins/import";
import { PluginConfigure } from "@/components/steve/plugins/configure";
import { PluginPresetEditor } from "@/components/steve/plugins/preset";
import { CapabilitySummary, ManifestDetails, PluginStatus, pluginError } from "@/components/steve/plugins/shared";
import { useResourceRead } from "@/hooks/use-resource-read";
import { fetchPlugins, preparePlugin, savePlugin } from "@/lib/api/plugins";
import { useFleet } from "@/lib/fleet";
import { useCoordination } from "@/lib/coordination";
import { when } from "@/lib/format";
import type { PluginInstallationView, PluginPackage, PluginsView } from "@/lib/plugin-types";
import { useI18n } from "@/providers/locale-provider";

export function PluginsPage() {
    const { t, locale } = useI18n(); const { snap } = useFleet(); const { view: coordination } = useCoordination();
    const [params, setParams] = useSearchParams(); const [view, setView] = useState<PluginsView | null>(null);
    const [readError, setReadError] = useState(""); const [error, setError] = useState(""); const [notice, setNotice] = useState("");
    const [busy, setBusy] = useState(""); const pending = useRef(false);
    const load = useResourceRead("plugins", fetchPlugins, (next) => { setView(next); setReadError(""); }, (error) => setReadError(pluginError(error)));
    useEffect(() => { void load(); }, [load, snap.at]);
    const records = view?.packages ?? []; const installations = view?.installations ?? [];
    const nodeMap = new Map<string, string>();
    if (!coordination?.enabled) nodeMap.set("", `${t("plugins.local")} · ${snap.hub.node || "hub"}`);
    for (const node of snap.nodes) if (coordination?.enabled || node.name !== snap.hub.node) nodeMap.set(node.name, node.name);
    for (const item of installations) for (const node of Object.keys(item.installation.targets)) if (!nodeMap.has(node)) nodeMap.set(node, node || t("plugins.local"));
    const nodes = [...nodeMap].map(([id, label]) => ({ id, label }));
    const usageItem=installations.find((item)=>item.id===params.get("usage"));
    const editing = installations.find((item) => item.id === params.get("edit"));
    const initial = records.find((p) => p.digest === (editing?.installation.digest ?? params.get("configure")));
    const presetItem = installations.find((item) => item.id === params.get("preset"));
    const presetRecord = records.find((p) => p.digest === presetItem?.installation.digest);
    const close = () => setParams({}, { replace: true });
    async function run(id: string, action: () => Promise<unknown>) {
        if (pending.current || readError) return; pending.current = true; setBusy(id); setError(""); setNotice("");
        try { await action(); await load(); } catch (error) { setError(pluginError(error)); } finally { pending.current = false; setBusy(""); }
    }
    const saved = (message: string) => { close(); setNotice(message); void load(); };
    const setEnabled = (item: PluginInstallationView) => { if (!view || !window.confirm(t(item.installation.enabled ? "plugins.confirmDisable" : "plugins.confirmEnable"))) return; void run(item.id, () => savePlugin(item.id, view.revision, { ...item.installation, enabled: !item.installation.enabled })); };
    return <div className="workbench-page flex min-w-0 flex-col">
        <PageHeader title={t("plugins.title")} description={t("plugins.description")} actions={<><Button size="sm" color="secondary" iconLeading={RefreshCw01} onClick={() => void load()}>{t("plugins.refresh")}</Button><Button size="sm" color="primary" iconLeading={Plus} isDisabled={!!readError} onClick={() => setParams({ import: "1" })}>{t("plugins.import")}</Button></>} />
        <PageBody>
            {(error || readError) ? <div role="alert" className="rounded-lg bg-error-primary p-3 text-sm text-error-primary">{readError ? `${t("plugins.couldNotLoad")} ${readError}` : error}</div> : null}
            {(notice || view?.warning) ? <div role="status" className="rounded-lg bg-secondary p-3 text-sm text-secondary">{notice || view?.warning}</div> : null}
            <Panel title={t("plugins.installations")} description={t("plugins.installationsHint")}>
                {!view ? <p role="status" className="text-sm text-tertiary">{t("common.loading")}</p> : installations.length === 0 ? <p className="py-3 text-sm text-tertiary">{t("plugins.noInstallations")}</p> : <ul className="flex min-w-0 flex-col divide-y divide-secondary">{installations.map((item) => <li key={item.id} className="flex min-w-0 flex-col gap-3 py-4 first:pt-0 last:pb-0">
                    <div className="flex min-w-0 flex-wrap items-start justify-between gap-3"><div className="min-w-0"><div className="flex flex-wrap items-center gap-2"><h3 className="break-all text-sm font-semibold text-primary" translate="no">{item.id}</h3><Badge type="pill-color" size="sm" color={item.installation.enabled ? "success" : "gray"}>{t(item.installation.enabled ? "plugins.enabled" : "plugins.disabled")}</Badge></div><p className="mt-1 break-all text-xs text-tertiary" translate="no">{item.installation.package_id} · {records.find((p) => p.digest === item.installation.digest)?.manifest.version ?? item.installation.digest.slice(0, 12)}</p></div><div className="flex flex-wrap gap-2"><Button size="sm" color="secondary" isDisabled={!!busy || !!readError} onClick={() => setParams({ edit: item.id })}>{t("plugins.edit")}</Button><Button size="sm" color="secondary" isLoading={busy === item.id} isDisabled={!!busy || !!readError} onClick={() => void run(item.id, () => preparePlugin(item.id))}>{t("plugins.prepare")}</Button><Button size="sm" color="secondary" isDisabled={!!busy || !!readError} onClick={() => setEnabled(item)}>{t(item.installation.enabled ? "plugins.disable" : "plugins.enable")}</Button></div></div>
                    <div className="flex flex-wrap items-center gap-2 text-xs"><span className="text-tertiary">{t("plugins.projects")}</span><Chips items={item.installation.projects.map((id) => ({ id }))} /></div>
                    <ul className="flex min-w-0 flex-col gap-2">{item.targets.map((target) => <li key={target.node} className="flex min-w-0 flex-wrap items-baseline gap-2 text-xs"><span className="font-medium text-secondary">{nodeMap.get(target.node) ?? target.node}</span><PluginStatus state={target.state} />{target.receipt ? <span className="text-quaternary">{when(target.receipt.prepared_at, locale)}</span> : null}{target.error ? <span className="break-all text-error-primary">{target.error}</span> : null}</li>)}</ul>
                    <div><Button size="sm" color="link-gray" isDisabled={!!readError} onClick={() => setParams({ usage: item.id })}>{t("plugins.usage")}</Button></div>
                    {Object.keys(records.find((p) => p.digest === item.installation.digest)?.manifest.agents ?? {}).length ? <div><Button size="sm" color="link-gray" isDisabled={!!readError} onClick={() => setParams({ preset: item.id })}>{t("plugins.createAgent")}</Button></div> : null}
                </li>)}</ul>}
            </Panel>
            <Panel title={t("plugins.library")}>
                {!view ? <p className="text-sm text-tertiary">{t("common.loading")}</p> : !records.length ? <Nothing icon={Package} title={t("plugins.empty")}>{t("plugins.emptyHint")}</Nothing> : <ul className="grid min-w-0 gap-4 xl:grid-cols-2">{records.map((record) => <PackageCard key={`${record.project}/${record.digest}`} record={record} disabled={!!readError} onConfigure={() => setParams({ configure: record.digest })} />)}</ul>}
            </Panel>
            {view?.operations?.length ? <Panel title={t("plugins.operations")} description={t("plugins.operationsHint")}><ul className="flex flex-col gap-2">{[...view.operations].sort((a,b) => b.updated_at.localeCompare(a.updated_at)).slice(0,10).map((operation) => <li key={operation.id} className="flex min-w-0 flex-wrap items-center gap-2 text-xs"><span className="font-medium text-secondary">{operation.kind}</span><code className="break-all text-tertiary">{operation.id}</code><span className="text-tertiary">{operation.state}</span>{operation.error ? <span className="break-all text-error-primary">{operation.error}</span> : null}</li>)}</ul></Panel> : null}
        </PageBody>
        {usageItem && view ? <PluginUsageDrawer item={usageItem} revision={view.revision} onClose={close} onRemoved={()=>saved(t("plugins.removed"))} /> : null}
        {params.has("import") ? <PluginImport projects={snap.projects.filter((p) => !p.home)} onClose={close} onImported={() => saved(t("plugins.imported"))} /> : null}
        {initial && view ? <PluginConfigure key={editing?.id ?? initial.digest} packages={records} item={editing} initial={initial} revision={view.revision} nodes={nodes} onClose={close} onSaved={(warning) => saved(warning || t("plugins.saved"))} /> : null}
        {presetItem && presetRecord ? <PluginPresetEditor agents={view?.agents ?? []} installation={presetItem} record={presetRecord} nodes={nodes} onClose={close} onSaved={(warning) => saved(warning || t("plugins.agentSaved"))} /> : null}
    </div>;
}

function PackageCard({ record, disabled, onConfigure }: { record: PluginPackage; disabled: boolean; onConfigure: () => void }) {
    const { t } = useI18n();
    return <li className="flex min-w-0 flex-col gap-3 rounded-lg border border-secondary p-4"><div className="flex flex-wrap items-center gap-2"><h3 className="break-all text-sm font-semibold text-primary" translate="no">{record.manifest.id}</h3><Badge type="modern" size="sm" color="gray">{record.manifest.version}</Badge></div><p className="text-sm text-secondary">{record.manifest.description}</p><CapabilitySummary manifest={record.manifest} /><ManifestDetails manifest={record.manifest} /><div className="flex flex-wrap items-center justify-between gap-3"><span className="break-all text-xs text-tertiary">{t("plugins.projects")} · {record.project}</span><Button size="sm" color="secondary" isDisabled={disabled} onClick={onConfigure}>{t("plugins.configure")}</Button></div><code className="break-all text-xs text-quaternary" translate="no">{record.digest}</code></li>;
}

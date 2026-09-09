import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/base/buttons/button";
import { Badge } from "@/components/base/badges/badges";
import { closePluginRuntime, fetchPluginUsage, removePlugin } from "@/lib/api/plugins";
import type { PluginInstallationView, PluginUsage } from "@/lib/plugin-types";
import { useI18n } from "@/providers/locale-provider";
import { PluginSheet, pluginError } from "./shared";

export function PluginUsageDrawer({ item, revision, onClose, onRemoved }: { item: PluginInstallationView; revision: string; onClose: () => void; onRemoved: () => void }) {
    const { t } = useI18n(); const [usage, setUsage] = useState<PluginUsage | null>(null); const [error, setError] = useState(""); const [readError, setReadError] = useState(""); const [busy, setBusy] = useState(false); const pending = useRef(false);
    const [refresh, setRefresh] = useState(0);
    useEffect(() => { const controller = new AbortController(); void fetchPluginUsage(item.id, controller.signal).then((value) => { if (!controller.signal.aborted) { setUsage(value); setReadError(""); } }).catch((error) => { if (!controller.signal.aborted) setReadError(pluginError(error)); }); return () => controller.abort(); }, [item.id, refresh]);
    async function closeRuntime(id: string) {
        if (pending.current || !window.confirm(t("plugins.confirmCloseRuntime"))) return;
        pending.current = true; setBusy(true); setError("");
        try { setUsage(await closePluginRuntime(item.id, id)); } catch (error) { setError(pluginError(error)); } finally { pending.current = false; setBusy(false); }
    }
    const canRemove = usage && !item.installation.enabled && !usage.references.length && !Object.keys(usage.errors).length && !usage.runtimes.some((runtime) => runtime.uses.some((use) => !use.stopped)) && !readError;
    async function remove() {
        if (pending.current || !canRemove || !window.confirm(t("plugins.confirmRemove"))) return;
        pending.current = true; setBusy(true); setError("");
        try { await removePlugin(item.id, revision); onRemoved(); } catch (error) { setError(pluginError(error)); setRefresh((value) => value + 1); } finally { pending.current = false; setBusy(false); }
    }
    return <PluginSheet title={t("plugins.usage")} busy={busy} onClose={onClose}>
        <p className="break-all font-medium text-primary">{item.id}</p><p className="text-sm text-tertiary">{t("plugins.usageHint")}</p>
        <Button size="sm" color="secondary" isDisabled={busy} onClick={() => setRefresh((value) => value + 1)}>{t("plugins.refresh")}</Button>
        {(error || readError) ? <div role="alert" className="rounded-lg bg-error-primary p-3 text-sm text-error-primary">{error || readError}</div> : null}
        {!usage ? <p role="status">{t("common.loading")}</p> : <>
            {Object.entries(usage.errors).map(([node, error]) => <p key={node} className="break-words text-sm text-error-primary">{node}: {error}</p>)}
            <ul className="flex min-w-0 flex-col gap-3">{usage.runtimes.map((runtime) => {
                const references = usage.references.filter((entry) => entry.runtime.id === runtime.ref.id);
                const active = references.some((ref) => ref.kind === "attempt" || ref.kind === "preparing");
                return <li key={runtime.ref.id} className="flex min-w-0 flex-col gap-2 rounded-lg border border-secondary p-3">
                    <div className="flex flex-wrap gap-2 text-sm"><span className="font-medium text-primary">{runtime.ref.selection.node || t("plugins.local")}</span><span className="text-tertiary">{runtime.ref.selection.project} · {runtime.ref.selection.harness}</span><Badge type="pill-color" size="sm" color={active ? "warning" : "gray"}>{t(active ? "plugins.inUse" : "plugins.retained")}</Badge></div>
                    {runtime.packages?.map((pkg) => <p key={pkg.digest} className="break-all text-xs text-secondary">{pkg.id} · {pkg.version}</p>)}
                    <code className="break-all text-xs text-quaternary">{runtime.ref.id}</code>
                    {references.map((ref, index) => <p key={`${ref.kind}/${ref.owner}/${index}`} className="break-all text-xs text-secondary">{ref.kind} · {ref.owner}</p>)}
                    <Button size="sm" color="secondary" isDisabled={busy || active || !!readError} onClick={() => void closeRuntime(runtime.ref.id)}>{t("plugins.closeRuntime")}</Button>
                </li>;
            })}</ul>
            {usage.references.filter((ref) => !ref.runtime.id).map((ref) => <p key={ref.owner} className="text-sm text-warning-primary">{t("plugins.inUse")} · {ref.owner}</p>)}
            {!usage.runtimes.length && !usage.references.length ? <p className="text-sm text-tertiary">{t("plugins.noReferences")}</p> : null}
        </>}
        <div className="mt-3 flex flex-col gap-2 border-t border-secondary pt-4"><p className="text-xs text-tertiary">{t("plugins.removeHint")}</p><Button size="sm" color="primary-destructive" isDisabled={!canRemove || busy} isLoading={busy} onClick={() => void remove()}>{t("plugins.remove")}</Button></div>
    </PluginSheet>;
}

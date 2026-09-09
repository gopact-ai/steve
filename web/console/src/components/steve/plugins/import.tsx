import { useRef, useState } from "react";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { KeyValue } from "@/components/steve/page";
import { useI18n } from "@/providers/locale-provider";
import { importPlugin, previewPlugin } from "@/lib/api/plugins";
import type { PluginPreview, PluginSource } from "@/lib/plugin-types";
import { CapabilitySummary, ManifestDetails, PluginSheet, pluginError } from "./shared";

const pendingKey = "steve.plugins.pending-import.v1";
type PendingImport = { preview: PluginPreview; body: { command_id: string; project: string; digest: string; source: PluginSource } };
function savedImport(): PendingImport | null {
    try { const raw = localStorage.getItem(pendingKey); if (!raw) return null; const value = JSON.parse(raw) as PendingImport; return value.body?.command_id && value.preview?.digest === value.body.digest ? value : null; } catch { return null; }
}
export function PluginImport({ projects, onClose, onImported }: { projects: { id: string }[]; onClose: () => void; onImported: () => void }) {
    const { t } = useI18n();
    const [pending, setPending] = useState(savedImport);
    const [source, setSource] = useState<PluginSource>(() => pending?.body.source ?? { kind: "directory", location: "" });
    const [preview, setPreview] = useState<PluginPreview | null>(() => pending?.preview ?? null);
    const [project, setProject] = useState(() => pending?.body.project ?? projects[0]?.id ?? "");
    const [busy, setBusy] = useState(false); const running = useRef(false); const [error, setError] = useState("");
    const update = (next: PluginSource) => { setSource(next); setPreview(null); setError(""); };
    async function inspect() {
        if (running.current) return; running.current = true; setBusy(true); setError("");
        try { setPreview(await previewPlugin(source)); } catch (error) { setError(pluginError(error)); } finally { running.current = false; setBusy(false); }
    }
    async function save() {
        if (running.current || !preview || !project) return;
        running.current = true; setBusy(true); setError("");
        try {
            const next = pending ?? { preview, body: { command_id: crypto.randomUUID(), project, digest: preview.digest, source: preview.source } };
            localStorage.setItem(pendingKey, JSON.stringify(next)); setPending(next);
            await importPlugin(next.body); localStorage.removeItem(pendingKey); setPending(null); onImported();
        } catch (error) { setError(pluginError(error)); } finally { running.current = false; setBusy(false); }
    }
    return <PluginSheet title={t("plugins.import")} dirty={source.location !== ""} busy={busy} onClose={onClose}>
        <form className="flex min-w-0 flex-col gap-4" onSubmit={(event) => { event.preventDefault(); void (preview ? save() : inspect()); }}>
            <fieldset disabled={busy || pending !== null} className="flex min-w-0 flex-col gap-4">
                <Select size="sm" label={t("plugins.source")} selectedKey={source.kind} onSelectionChange={(key) => update({ kind: key === "git" ? "git" : "directory", location: source.location })} items={[{ id: "directory", label: t("plugins.directory") }, { id: "git", label: t("plugins.git") }]}>{(item) => <Select.Item {...item} />}</Select>
                <Input name="plugin-source" autoComplete="off" spellCheck="false" label={t("plugins.location")} hint={t("plugins.sourceHint")} value={source.location} onChange={(location) => update({ ...source, location })} isRequired />
                {source.kind === "git" ? <><Input name="plugin-commit" autoComplete="off" spellCheck="false" label={t("plugins.commit")} value={source.commit ?? ""} onChange={(commit) => update({ ...source, commit })} isRequired /><Input name="plugin-subdir" autoComplete="off" spellCheck="false" label={t("plugins.subdir")} value={source.subdir ?? ""} onChange={(subdir) => update({ ...source, subdir })} /></> : null}
                <Select size="sm" label={t("plugins.importProject")} placeholder={t("plugins.selectProject")} selectedKey={project || null} onSelectionChange={(key) => setProject(String(key ?? ""))} items={projects.map((p) => ({ id: p.id, label: p.id }))} isRequired>{(item) => <Select.Item {...item} />}</Select>
            </fieldset>
            {preview ? <section className="flex min-w-0 flex-col gap-3 rounded-lg bg-secondary p-4">
                <KeyValue dense rows={[{ k: t("plugins.package"), v: <span translate="no">{preview.manifest.id}</span> }, { k: t("plugins.version"), v: preview.manifest.version }, { k: t("plugins.digest"), v: <code className="break-all text-xs" translate="no">{preview.digest}</code> }]} />
                <p className="text-sm text-secondary">{preview.manifest.description}</p><CapabilitySummary manifest={preview.manifest} /><ManifestDetails manifest={preview.manifest} /><p className="text-xs text-tertiary">{t("plugins.previewHint")}</p>
            </section> : null}
            {error ? <div role="alert" className="rounded-lg bg-error-primary p-3 text-sm text-error-primary">{error}</div> : null}
            {error && pending ? <Button color="secondary" size="sm" isDisabled={busy} onClick={() => { if (window.confirm(t("plugins.reviewAgainHint"))) { localStorage.removeItem(pendingKey); setPending(null); setPreview(null); setError(""); } }}>{t("plugins.reviewAgain")}</Button> : null}
            <Button type="submit" size="sm" color="primary" isLoading={busy}>{pending ? t("plugins.retry") : preview ? t("plugins.importConfirm") : t("plugins.preview")}</Button>
        </form>
    </PluginSheet>;
}

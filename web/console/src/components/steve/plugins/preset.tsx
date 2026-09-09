import { useRef, useState } from "react";
import { Checkbox } from "@/components/base/checkbox/checkbox";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { TextArea } from "@/components/base/textarea/textarea";
import { KeyValue } from "@/components/steve/page";
import { applyPluginPreset, previewPluginPreset } from "@/lib/api/plugins";
import type { PluginAdoption, PluginAgentView, PluginInstallationView, PluginPackage, PluginPresetPreview, PluginPresetRequest } from "@/lib/plugin-types";
import { useI18n } from "@/providers/locale-provider";
import { PluginSheet, pluginError } from "./shared";

const pendingPresetKey = "steve.plugins.pending-preset.v1";
type PendingPreset = { installation: string; request: PluginPresetRequest; preview: PluginPresetPreview };
function readPendingPreset(id: string): PendingPreset | null {
    try { const raw = localStorage.getItem(pendingPresetKey); if (!raw) return null; const value = JSON.parse(raw) as PendingPreset; return value.installation === id && value.request?.command_id && value.preview?.revision ? value : null; } catch { return null; }
}
export function PluginPresetEditor({ agents, installation, record, nodes, onClose, onSaved }: { agents: PluginAgentView[]; installation: PluginInstallationView; record: PluginPackage; nodes: { id: string; label: string }[]; onClose: () => void; onSaved: (warning?: string) => void }) {
    const { t } = useI18n(); const [saved] = useState(() => readPendingPreset(installation.id)); const presets = Object.keys(record.manifest.agents ?? {});
    const [preset, setPreset] = useState(saved?.request.preset ?? presets[0] ?? ""); const [agent, setAgent] = useState(saved?.request.agent_id ?? "");
    const targets = Object.keys(installation.installation.targets); const [node, setNode] = useState(saved?.request.node ?? targets[0] ?? "");
    const [adopt, setAdopt] = useState<PluginAdoption | null>(saved?.request.adopt ?? null);
    const existing = agents.find((item) => item.id === agent);
    const [model, setModel] = useState(saved?.request.model ?? ""); const [instructions, setInstructions] = useState(saved?.request.system_prompt ?? "");
    const [preview, setPreview] = useState<PluginPresetPreview | null>(saved?.preview ?? null); const [submitted, setSubmitted] = useState<PluginPresetRequest | null>(saved?.request ?? null);
    const [busy, setBusy] = useState(false); const running = useRef(false); const [error, setError] = useState("");
    const edit = (set: (value: string) => void) => (value: string) => { set(value); setPreview(null); };
    async function run() {
        if (running.current) return; running.current = true; setBusy(true); setError("");
        try {
            if (!preview) { setPreview(await previewPluginPreset(installation.id, { command_id: "", base_revision: "", agent_id: agent, node, preset, digest: record.digest, ...(adopt ? { adopt } : {}), ...(model ? { model } : {}), ...(instructions ? { system_prompt: instructions } : {}) })); }
            else {
                const body = submitted ?? { command_id: crypto.randomUUID(), base_revision: preview.revision, agent_id: agent, node, preset, digest: record.digest, ...(adopt ? { adopt } : {}), ...(model ? { model } : {}), ...(instructions ? { system_prompt: instructions } : {}) };
                localStorage.setItem(pendingPresetKey, JSON.stringify({ installation: installation.id, request: body, preview })); setSubmitted(body); const result = await applyPluginPreset(installation.id, body); localStorage.removeItem(pendingPresetKey); onSaved(result.warning);
            }
        } catch (error) { setError(pluginError(error)); } finally { running.current = false; setBusy(false); }
    }
    return <PluginSheet title={t("plugins.createAgent")} dirty={agent !== ""} busy={busy} onClose={onClose}>
        <form className="flex min-w-0 flex-col gap-4" onSubmit={(event) => { event.preventDefault(); void run(); }}>
            <fieldset disabled={busy || submitted !== null} className="flex min-w-0 flex-col gap-4">
                <Select size="sm" label={t("plugins.preset")} selectedKey={preset} onSelectionChange={(key) => edit(setPreset)(String(key))} items={presets.map((id) => ({ id, label: id }))}>{(item) => <Select.Item {...item} />}</Select>
                <Input name="plugin-agent-id" autoComplete="off" spellCheck="false" label={t("plugins.agentId")} value={agent} onChange={(value) => { edit(setAgent)(value); if (adopt) setAdopt({}); }} isRequired />
                <Select size="sm" label={t("plugins.machines")} selectedKey={node || "@local"} onSelectionChange={(key) => edit(setNode)(key === "@local" ? "" : String(key))} items={targets.map((id) => ({ id: id || "@local", label: nodes.find((node) => node.id === id)?.label ?? id }))}>{(item) => <Select.Item {...item} />}</Select>
                <Input name="plugin-model" autoComplete="off" spellCheck="false" label={t("plugins.model")} value={model} onChange={edit(setModel)} />
                <TextArea name="plugin-instructions" autoComplete="off" label={t("plugins.instructions")} value={instructions} onChange={edit(setInstructions)} rows={4} />
            </fieldset>
            {!submitted && existing && !existing.origin ? <div className="flex flex-col gap-3 rounded-lg border border-secondary p-3">
                <Checkbox label={t("plugins.adoptAgent")} isDisabled={busy} isSelected={adopt !== null} onChange={(value) => { setAdopt(value ? {} : null); setPreview(null); }} />
                {adopt ? <><p className="text-xs text-tertiary">{t("plugins.adoptHint")}</p><AdoptionFields existing={existing} preset={record.manifest.agents?.[preset]} value={adopt} disabled={busy} onChange={(next) => { setAdopt(next); setPreview(null); }} /></> : null}
            </div> : null}
            {preview ? <section className="flex min-w-0 flex-col gap-3 rounded-lg bg-secondary p-4"><KeyValue dense rows={[{ k: t("plugins.agentId"), v: preview.proposed.id }, { k: t("plugins.harness"), v: preview.proposed.harness }, { k: t("plugins.model"), v: preview.proposed.model || "—" }]} />{adopt && preview.existing ? <p className="text-xs text-secondary">{t("plugins.replacedAttachments", { skills: String(Object.keys(adopt.skills ?? {}).length), tools: String(Object.keys(adopt.mcp ?? {}).length) })}</p> : null}{preview.existing ? <p className="text-sm text-secondary">{t("plugins.existingAgent")}</p> : null}{preview.preserved.length ? <p className="text-xs text-tertiary">{t("plugins.preserved", { fields: preview.preserved.join(", ") })}</p> : null}<p className="whitespace-pre-wrap break-words text-xs text-secondary">{preview.proposed.system_prompt}</p></section> : null}
            {error ? <div role="alert" className="rounded-lg bg-error-primary p-3 text-sm text-error-primary">{error}</div> : null}
            {error && submitted ? <Button color="secondary" size="sm" isDisabled={busy} onClick={() => { if (window.confirm(t("plugins.reviewAgainHint"))) { localStorage.removeItem(pendingPresetKey); setSubmitted(null); setPreview(null); setError(""); } }}>{t("plugins.reviewAgain")}</Button> : null}
            <Button type="submit" color="primary" size="sm" isLoading={busy}>{submitted ? t("plugins.retry") : preview ? t("plugins.applyAgent") : t("plugins.previewAgent")}</Button>
        </form>
    </PluginSheet>;
}

function AdoptionFields({ existing, preset, value, disabled, onChange }: { existing: PluginAgentView; preset?: import("@/lib/plugin-types").PluginPreset; value: PluginAdoption; disabled: boolean; onChange: (next: PluginAdoption) => void }) {
    const { t } = useI18n();
    return <>{(["skills", "mcp"] as const).flatMap((kind) => (kind === "skills" ? existing.skills ?? [] : existing.mcp_servers ?? []).map((name) => {
        const options = kind === "skills" ? preset?.skills ?? [] : preset?.mcp_servers ?? [];
        return <Select key={`${kind}/${name}`} size="sm" label={`${t(kind === "skills" ? "plugins.skills" : "plugins.tools")} · ${name}`} isDisabled={disabled} selectedKey={value[kind]?.[name] ?? "@keep"} items={[{ id: "@keep", label: t("plugins.keepAttachment") }, ...options.map((id) => ({ id, label: id }))]} onSelectionChange={(key) => { const mapped = { ...value[kind] }; if (key === "@keep") delete mapped[name]; else mapped[name] = String(key); onChange({ ...value, [kind]: mapped }); }}>{(item) => <Select.Item {...item} />}</Select>;
    }))}</>;
}

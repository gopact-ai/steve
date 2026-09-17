import { useEffect, useRef, useState } from "react";
import { Radio, RadioGroup } from "react-aria-components";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Button } from "@/components/base/buttons/button";
import { Checkbox } from "@/components/base/checkbox/checkbox";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { useResourceRead } from "@/hooks/use-resource-read";
import { useI18n } from "@/providers/locale-provider";
import { discoverNodeAgents, enrollNodeAgents, type NodeAgentCandidate, type NodeAgentChoice, type NodeAgentDiscovery, type NodeAgentEnrollment, type NodeAgentRequest } from "@/lib/api/node-agents";

const agentName = /^[a-z0-9][a-z0-9._-]{0,63}$/;
interface EnrollmentDraft { selected: string[]; names?: Record<string, string>; about?: Record<string, string>; models?: Record<string, string>; options?: Record<string, Record<string, string>>; primary?: string; pending?: NodeAgentRequest }

function readDraft(key: string): EnrollmentDraft {
    try {
        const value = JSON.parse(localStorage.getItem(key) || "null");
        const selected = Array.isArray(value?.selected) ? value.selected.filter((item: unknown) => typeof item === "string") : [];
        return { ...value, selected, ...(value?.pending?.agents?.length ? { pending: value.pending } : { pending: undefined }) };
    } catch { return { selected: [] }; }
}

function usable(candidate: NodeAgentCandidate) { return candidate.installed && !candidate.requires?.length && !candidate.registered; }

// slug turns a machine's display name into a name an agent can answer to.
// A name written in another script leaves nothing usable, and then the
// tool's own id is the readable choice.
function slug(value: string) { return value.toLowerCase().replace(/[^a-z0-9._-]+/g, "-").replace(/^[^a-z0-9]+/, "").replace(/[-_.]+$/, "").slice(0, 40); }
function suggestedName(machine: string, candidate: string) {
    const prefix = slug(machine);
    return prefix && prefix !== candidate ? `${prefix}-${candidate}`.slice(0, 64) : candidate;
}

export function NodeAgentEnrollment({ node, name = node, onClose, onRegistered }: { node: string; name?: string; onClose: () => void; onRegistered: () => void }) {
    const { t } = useI18n();
    const key = `steve.node-agent.enrollment:${new URL(".", window.location.href).href}:${encodeURIComponent(node)}`;
    const [draft, setDraft] = useState(() => readDraft(key));
    const [discovery, setDiscovery] = useState<NodeAgentDiscovery>({ revision: "", agents: [] });
    const [loading, setLoading] = useState(true);
    const [readError, setReadError] = useState("");
    const [error, setError] = useState("");
    const [busy, setBusy] = useState(false);
    const [result, setResult] = useState<NodeAgentEnrollment | null>(null);
    const acting = useRef(false);
    const form = useRef<HTMLDivElement>(null);
    const load = useResourceRead(`node-agents:${node}`, (signal) => discoverNodeAgents(node, signal), (value) => { setDiscovery({ ...value, agents: value.agents || [] }); setReadError(""); setLoading(false); }, (error) => { setReadError(error instanceof Error ? error.message : String(error)); setLoading(false); });
    useEffect(() => { void load(); }, [load]);
    function save(next: EnrollmentDraft) {
        setDraft(next);
        try { localStorage.setItem(key, JSON.stringify(next)); return true; }
        catch { setError(t("nodeAgents.storageError")); return false; }
    }
    function choose(id: string, selected: boolean) {
        setError("");
        const chosen = selected ? [...draft.selected.filter((item) => item !== id), id] : draft.selected.filter((item) => item !== id);
        save({ ...draft, selected: chosen, names: { ...draft.names, [id]: draft.names?.[id] ?? suggestedName(name, id) }, ...(draft.primary === id && !selected ? { primary: undefined } : {}) });
    }
    function edit(next: Partial<EnrollmentDraft>) { setError(""); save({ ...draft, ...next }); }
    const chosen = draft.selected.filter((id) => discovery.agents.some((item) => item.id === id && usable(item)));
    const nameOf = (id: string) => draft.names?.[id] ?? suggestedName(name, id);
    const shown = (id: string) => nameOf(id).trim().toLowerCase() || id;
    const primary = chosen.includes(draft.primary || "") ? draft.primary! : "";
    const locked = busy || !!draft.pending;
    function focusField(id: string) { form.current?.querySelector<HTMLInputElement>(`input[name="node-agent-name-${id}"]`)?.focus(); }
    // build turns the selection into what the server registers, refusing a
    // name it could not answer to and a name claimed twice in one go.
    function build(): NodeAgentChoice[] | null {
        const taken = new Set<string>();
        const out: NodeAgentChoice[] = [];
        for (const id of chosen) {
            const agent = nameOf(id).trim().toLowerCase();
            if (!agentName.test(agent)) { setError(t("nodeAgents.invalidName")); focusField(id); return null; }
            if (taken.has(agent)) { setError(t("nodeAgents.duplicateName", { name: agent })); focusField(id); return null; }
            taken.add(agent);
            const about = (draft.about?.[id] || "").trim();
            const model = draft.models?.[id] || "";
            const options = Object.fromEntries(Object.entries(draft.options?.[id] || {}).filter(([, value]) => value));
            out.push({ candidate_id: id, agent_id: agent, ...(about ? { about } : {}), ...(model ? { model } : {}), ...(Object.keys(options).length ? { options } : {}), ...(id === primary ? { default: true } : {}) });
        }
        return out;
    }
    async function register() {
        if (acting.current || result) return;
        const agents = draft.pending?.agents || build();
        if (!agents) return;
        if (!agents.length) { setError(t("nodeAgents.selectRequired")); form.current?.querySelector<HTMLInputElement>('input[type="checkbox"]:not(:disabled)')?.focus(); return; }
        const body: NodeAgentRequest = { agents, expected_revision: draft.pending?.expected_revision || discovery.revision };
        if (!body.expected_revision || !save({ ...draft, pending: body })) return;
        acting.current = true; setBusy(true); setError("");
        try {
            let current = discovery;
            if (draft.pending) {
                current = await discoverNodeAgents(node);
                if (!current.revision || !Array.isArray(current.agents)) throw new Error(t("nodeAgents.invalidResponse"));
                setDiscovery(current); setReadError("");
            }
            const request = { ...body, expected_revision: current.revision };
            if (!save({ ...draft, pending: request })) return;
            const receipt = await enrollNodeAgents(node, request);
            if (!receipt || receipt.registered !== true || !receipt.revision) throw new Error(t("nodeAgents.invalidResponse"));
            setResult(receipt);
            try { localStorage.removeItem(key); } catch { /* The acknowledged registration remains authoritative. */ }
            onRegistered();
        } catch (error) {
            setError(error instanceof Error ? error.message : String(error));
            await load();
        } finally { acting.current = false; setBusy(false); }
    }
    const registered = result?.agents?.length ? result.agents.join("、") : result?.agent_id || "";
    const radio = "group flex min-h-8 cursor-pointer items-center gap-2 rounded-md border border-secondary px-3 py-1.5 text-sm text-primary data-selected:border-brand data-selected:bg-secondary data-focus-visible:outline-2 data-focus-visible:outline-focus-ring";
    return <ModalOverlay isOpen isDismissable={!busy} isKeyboardDismissDisabled={busy} onOpenChange={(open) => { if (!open && !busy) onClose(); }} className="z-[100] motion-reduce:animate-none motion-reduce:duration-0"><Modal className="max-w-xl motion-reduce:animate-none motion-reduce:duration-0"><Dialog aria-label={t("nodeAgents.title", { node: name })} className="block overflow-hidden rounded-xl bg-primary p-0 ring-1 ring-secondary"><div className="max-h-[min(800px,85dvh)] overflow-y-auto overscroll-contain p-5 sm:p-6" ref={form}>
        <header className="mb-5 space-y-2"><h2 className="break-words text-md font-semibold text-primary">{t("nodeAgents.title", { node: name })}</h2><p className="text-sm leading-6 text-secondary">{t("nodeAgents.description")}</p><p className="text-xs leading-5 text-tertiary">{t("nodeAgents.presenceHint")}</p></header>
        {result ? <div className="space-y-3"><h3 role="status" className="text-base font-semibold text-primary">{t("nodeAgents.success")}</h3><p className="break-words text-sm leading-6 text-secondary">{t("nodeAgents.successHint", { agent: registered, node: name })}</p><Button size="md" onClick={onClose}>{t("nodeAgents.done")}</Button></div> : <div className="space-y-4">
            {loading && <p role="status" className="text-sm text-tertiary">{t("nodeAgents.loading")}</p>}
            {!loading && !readError && !discovery.agents.some((candidate) => candidate.installed) && <div className="space-y-1 rounded-lg bg-secondary p-3"><p className="text-sm font-medium text-primary">{t("nodeAgents.empty")}</p><p className="text-xs leading-5 text-tertiary">{t("nodeAgents.emptyHint")}</p></div>}
            <fieldset className="min-w-0 space-y-2" disabled={locked}>
                <legend className="mb-2 text-sm font-semibold text-primary">{t("nodeAgents.tools")}</legend>
                {discovery.agents.map((candidate) => <Checkbox key={candidate.id} aria-label={candidate.name} name="node-agent" value={candidate.id} isSelected={candidate.registered || draft.selected.includes(candidate.id)} isDisabled={locked || !usable(candidate)} onChange={(selected) => choose(candidate.id, selected)}
                    className="min-h-10 min-w-0 rounded-md border border-secondary px-3 py-2 data-selected:bg-secondary [&>div:last-child]:min-w-0" label={<span className="flex min-w-0 flex-col gap-1"><span className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1"><span>{candidate.name}</span><span className="text-xs font-normal text-tertiary">{t(candidate.registered ? "nodeAgents.registered" : candidate.configured ? "nodeAgents.configured" : candidate.installed ? "nodeAgents.detected" : "nodeAgents.notInstalled")}</span></span>
                        {candidate.requires?.length ? <span className="text-xs font-normal text-error-primary">{t("nodeAgents.requires", { tools: candidate.requires.join(", ") })}</span> : candidate.executable ? <span className="max-w-full break-all font-mono text-xs font-normal text-tertiary">{candidate.executable}</span> : null}
                    </span>} />)}
            </fieldset>
            {chosen.length > 0 && <div className="space-y-3">
                {chosen.map((id) => {
                    const candidate = discovery.agents.find((item) => item.id === id);
                    const models = candidate?.models || [];
                    const selectors = candidate?.selectors || [];
                    return <fieldset key={id} disabled={locked} className="min-w-0 space-y-3 rounded-lg border border-secondary p-3">
                        <legend className="px-1 text-sm font-medium text-primary">{candidate?.name || id}</legend>
                        <Input size="sm" label={t("nodeAgents.agentName")} name={`node-agent-name-${id}`} autoComplete="off" spellCheck="false" hint={t("nodeAgents.agentHint")} maxLength={64} value={nameOf(id)} isDisabled={locked} onChange={(value) => edit({ names: { ...draft.names, [id]: value } })} />
                        <Input size="sm" label={t("nodeAgents.agentAbout")} name={`node-agent-about-${id}`} autoComplete="off" placeholder={t("nodeAgents.agentAboutPlaceholder")} hint={t("nodeAgents.agentAboutHint")} maxLength={120} value={draft.about?.[id] || ""} isDisabled={locked} onChange={(value) => edit({ about: { ...draft.about, [id]: value } })} />
                        {models.length > 0 ? <Select size="sm" label={t("nodeAgents.model")} hint={t("nodeAgents.modelHint")} selectedKey={draft.models?.[id] || "__default"} isDisabled={locked} onSelectionChange={(selection) => { if (selection) edit({ models: { ...draft.models, [id]: String(selection) === "__default" ? "" : String(selection) } }); }}
                            items={[{ id: "__default", label: t("nodeAgents.modelDefault", { model: candidate?.model || "—" }) }, ...models.map((model) => ({ id: model, label: model }))]}>{(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}</Select>
                            : <p className="text-xs leading-5 text-tertiary">{t("nodeAgents.modelUnknown")}</p>}
                        {selectors.map((selector) => <Select key={selector.id} size="sm" label={selector.name || selector.id} selectedKey={draft.options?.[id]?.[selector.id] || "__default"} isDisabled={locked} onSelectionChange={(selection) => { if (selection) edit({ options: { ...draft.options, [id]: { ...draft.options?.[id], [selector.id]: String(selection) === "__default" ? "" : String(selection) } } }); }}
                            items={[{ id: "__default", label: t("nodeAgents.optionDefault", { value: selector.current || "—" }) }, ...(selector.values || []).map((value, index) => ({ id: value, label: selector.choices?.[index] || value }))]}>{(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}</Select>)}
                    </fieldset>;
                })}
                {chosen.length > 1 && <RadioGroup aria-label={t("nodeAgents.defaultAgent")} value={primary} isDisabled={locked} onChange={(value) => edit({ primary: value })} className="space-y-2">
                    <span className="text-sm font-medium text-primary">{t("nodeAgents.defaultAgent")}</span>
                    <div className="flex flex-wrap gap-2">{chosen.map((id) => <Radio key={id} value={id} className={radio}>{shown(id)}</Radio>)}</div>
                    <span className="block text-xs leading-5 text-tertiary">{t("nodeAgents.defaultAgentHint")}</span>
                </RadioGroup>}
            </div>}
            {draft.pending && <p role="status" className="text-xs leading-5 text-tertiary">{t("nodeAgents.uncertain")}</p>}
            {readError && <p role="alert" className="break-words text-sm text-error-primary">{readError}</p>}
            {error && <p role="alert" className="break-words text-sm text-error-primary">{error}</p>}
            {busy && <p role="status" className="text-sm text-tertiary">{t("nodeAgents.registering")}</p>}
            <div className="flex flex-wrap gap-2"><Button size="md" isLoading={busy} isDisabled={loading || !!readError} onClick={() => void register()}>{t(draft.pending ? "nodeAgents.retry" : "nodeAgents.register")}</Button><Button size="md" color="secondary" isDisabled={busy} onClick={() => { setLoading(true); void load(); }}>{t("nodeAgents.refresh")}</Button><Button size="md" color="tertiary" isDisabled={busy} onClick={onClose}>{t("nodeAgents.close")}</Button></div>
        </div>}
    </div></Dialog></Modal></ModalOverlay>;
}

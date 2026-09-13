import { useRef, useState } from "react";
import { X } from "@untitledui/icons";
import { Radio, RadioGroup } from "react-aria-components";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { Sheet } from "@/components/steve/drawer";
import { request } from "@/lib/http";
import { relative } from "@/lib/format";
import type { Agent, Node, Project } from "@/lib/types";
import { useI18n } from "@/providers/locale-provider";

interface HistoryEntry {
    native_id: string; harness: string; source_home: string; workdir: string;
    title?: string; updated_at: string; revision: string;
}

export function NativeSessionImport({ agents, nodes, projects, onClose, onImported }: { agents: Agent[]; nodes: Node[]; projects: Project[]; onClose: () => void; onImported: (conversation: string) => void }) {
    const { t, locale } = useI18n();
    const available = agents.filter((a) => ["codex", "claude-code", "grok", "dsh"].includes(a.harness) && nodes.some((n) => n.name === a.node && n.up && n.features?.includes("native_history.v1")));
    const [agentID, setAgentID] = useState(available[0]?.id || "");
    const agent = available.find((a) => a.id === agentID);
    const [home, setHome] = useState("");
    const [entries, setEntries] = useState<HistoryEntry[] | null>(null);
    const [selected, setSelected] = useState("");
    const [projectID, setProjectID] = useState("");
    const [query, setQuery] = useState("");
    const [busy, setBusy] = useState<"read" | "import" | null>(null);
    const pending = useRef(false);
    const [error, setError] = useState("");
    const entry = entries?.find((e) => e.revision === selected);
    const matching = entry && agent ? projects.filter((p) => p.workspaces.some((w) => w.node === agent.node && cleanNodePath(w.path) === cleanNodePath(entry.workdir) && (!w.state || w.state === "ready"))) : [];
    const project = matching.find((p) => p.id === projectID) || (matching.length === 1 ? matching[0] : undefined);
    const visible = entries?.filter((e) => `${e.title || ""} ${e.native_id} ${e.workdir}`.toLocaleLowerCase().includes(query.toLocaleLowerCase())) || [];
    function reset() { setQuery(""); setEntries(null); setSelected(""); setProjectID(""); setError(""); }
    async function find() {
        if (!agent || pending.current) return;
        pending.current = true; setBusy("read"); setError(""); setSelected("");
        try {
            const params = new URLSearchParams({ harness: agent.harness, home: home.trim() });
            const result = await request<{ entries: HistoryEntry[] | null }>(`/console/nodes/${encodeURIComponent(agent.node!)}/native-history?${params}`);
            setEntries(result.entries || []);
        } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
        finally { pending.current = false; setBusy(null); }
    }
    async function importSession() {
        if (!agent || !entry || !project || pending.current) return;
        pending.current = true; setBusy("import"); setError("");
        try {
            // The same selected snapshot and destination always retry the same
            // command, including after a lost response or a browser reload.
            const command = `native:${entry.revision}:${agent.id}:${project.id}`;
            const result = await request<{ conversation: string }>(`/console/nodes/${encodeURIComponent(agent.node!)}/native-history`, { method: "POST", body: { command_id: command, agent: agent.id, project: project.id, source: { harness: entry.harness, home: entry.source_home }, native_id: entry.native_id, revision: entry.revision } });
            onImported(result.conversation);
        } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
        finally { pending.current = false; setBusy(null); }
    }
    return <Sheet label={t("nativeImport.open")} width={580} onClose={onClose}>
        <div className="workbench-drawer-header">
            <h2 className="flex-1 text-md font-semibold text-primary">{t("nativeImport.open")}</h2>
            <Button color="tertiary" size="sm" iconLeading={X} aria-label={t("nativeImport.close")} onClick={onClose} />
        </div>
        <div className="workbench-drawer-body flex flex-col gap-4">
            <p className="text-sm text-tertiary">{t("nativeImport.hint")}</p>
            {!available.length ? <p role="status" className="text-sm text-tertiary">{t("nativeImport.unavailable")}</p> : <>
                <Select label={t("nativeImport.agent")} selectedKey={agentID} isDisabled={!!busy} onSelectionChange={(key) => { setAgentID(String(key)); reset(); }} items={available.map((a) => ({ id: a.id, label: `${a.id} · ${a.harness} · ${a.node}` }))}>
                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                </Select>
                <Input label={t("nativeImport.source")} hint={t("nativeImport.sourceHint")} value={home} isDisabled={!!busy} onChange={(value) => { setHome(value); reset(); }} />
                <Button color="secondary" isDisabled={!agent || !!busy} isLoading={busy === "read"} onClick={() => void find()}>{busy === "read" ? t("nativeImport.loading") : t("nativeImport.find")}</Button>
                {entries !== null && <>
                    <Input aria-label={t("nativeImport.search")} placeholder={t("nativeImport.search")} value={query} onChange={setQuery} />
                    {!visible.length && <p role="status" className="text-sm text-tertiary">{t("nativeImport.empty")}</p>}
                    <RadioGroup aria-label={t("nativeImport.open")} value={selected} isDisabled={!!busy} onChange={(value) => { setSelected(value); setProjectID(""); setError(""); }} className="flex max-h-80 flex-col gap-2 overflow-y-auto">
                        {visible.map((e) => <Radio key={e.revision} value={e.revision} className="cursor-pointer rounded-lg border border-secondary p-3 text-sm outline-none data-selected:border-brand data-selected:bg-brand-primary/30 data-focus-visible:ring-2 data-focus-visible:ring-brand">
                            <span className="block break-words font-medium text-primary">{e.title || e.native_id}</span>
                            <span className="mt-1 block break-all text-xs text-tertiary">{e.workdir}</span>
                            <span className="mt-1 block break-all text-xs text-quaternary">{e.native_id} · {relative(e.updated_at, locale)}</span>
                        </Radio>)}
                    </RadioGroup>
                </>}
                {entry && (matching.length ? <Select label={t("nativeImport.project")} selectedKey={project?.id || null} isDisabled={!!busy} onSelectionChange={(key) => setProjectID(String(key))} items={matching.map((p) => ({ id: p.id, label: p.id }))}>
                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                </Select> : <p role="status" className="text-sm text-tertiary">{t("nativeImport.noProject")}</p>)}
                <Button isDisabled={!entry || !project || !!busy} isLoading={busy === "import"} onClick={() => void importSession()}>{busy === "import" ? t("nativeImport.importing") : t("nativeImport.import")}</Button>
            </>}
            {error && <p role="alert" className="break-words text-sm text-error-primary">{error}</p>}
        </div>
    </Sheet>;
}

// Native histories currently come from POSIX nodes; match filepath.Clean on
// that node without interpreting backslashes or resolving filesystem symlinks.
function cleanNodePath(path: string): string {
    const parts: string[] = [];
    for (const part of path.split("/")) {
        if (!part || part === ".") continue;
        if (part === "..") parts.pop(); else parts.push(part);
    }
    return (path.startsWith("/") ? "/" : "") + parts.join("/");
}

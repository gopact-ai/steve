import { useEffect, useState } from "react";
import { Plus, Trash01 } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { ButtonUtility } from "@/components/base/buttons/button-utility";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { TextArea } from "@/components/base/textarea/textarea";
import { fetchNodeSettings, saveNodeSettings, type HarnessSetting, type MCPSetting, type NodeSettings } from "@/lib/api";

// SettingsEditor changes what a machine offers, from the page: its AI
// tools, the commands it checks for, its MCP servers, and what it merely
// declares. The machine validates, writes its own file, and reports back
// with a fresh snapshot; nothing here is saved until it says so.
export function SettingsEditor({ node, onClose, onSaved }: { node: string; onClose: () => void; onSaved: () => void }) {
    const [s, setS] = useState<NodeSettings | null>(null);
    const [error, setError] = useState("");
    const [busy, setBusy] = useState(false);
    useEffect(() => {
        void fetchNodeSettings(node).then((d) => setS(d.settings)).catch((e) => setError(String(e).replace(/^Error: /, "")));
    }, [node]);
    if (!s) return <div className="text-sm text-tertiary">{error || "读取中…"}</div>;
    const update = (patch: Partial<NodeSettings>) => setS({ ...s, ...patch });
    async function save() {
        if (!s) return;
        setBusy(true); setError("");
        try {
            await saveNodeSettings(node, s);
            onSaved();
            onClose();
        } catch (e) { setError(String(e).replace(/^Error: /, "")); } finally { setBusy(false); }
    }
    return (
        <div className="flex flex-col gap-5">
            <Section title="AI 工具" hint="每个 AI 工具是一条启动命令；Agent 按名字用它。">
                <KeyedRows
                    entries={s.harnesses}
                    onChange={(harnesses) => update({ harnesses })}
                    blank={{ command: "" }}
                    render={(id, h, set) => (
                        <div className="grid flex-1 grid-cols-[1fr_1fr_1.4fr] gap-2">
                            <Input size="sm" aria-label="名字" placeholder="名字，如 codex" value={id.value} onChange={id.set} />
                            <Input size="sm" aria-label="命令" placeholder="命令，如 npx" value={h.command} onChange={(v) => set({ ...h, command: v })} />
                            <Input size="sm" aria-label="参数" placeholder="参数，空格分隔" value={(h.args || []).join(" ")} onChange={(v) => set({ ...h, args: splitArgs(v) })} />
                        </div>
                    )}
                />
            </Section>
            <Section title="命令" hint="要检查是否在这台机器上的可执行文件，如 docker、gh。">
                <ListEditor items={s.tools} placeholder="docker" onChange={(tools) => update({ tools })} />
            </Section>
            <Section title="MCP 服务器" hint={s.external_broker ? "这台机器的 MCP 由独立的 broker 进程持有，改它的 mcp.json。" : "这台机器能为会话启动的 MCP 服务器；env 和 headers 只留在这台机器上。"}>
                {!s.external_broker && (
                    <KeyedRows
                        entries={s.mcp_servers}
                        onChange={(mcp_servers) => update({ mcp_servers })}
                        blank={{ type: "stdio", command: "" }}
                        render={(id, m, set) => <MCPRow id={id} m={m} set={set} />}
                    />
                )}
            </Section>
            <Section title="声明" hint="没人能从进程里核实的：网络、凭据。写成 kind:id，如 network:office、credential:prod。">
                <ListEditor items={s.declares} placeholder="network:office" onChange={(declares) => update({ declares })} />
            </Section>
            <Section title="标签" hint="自由词，计划步骤用裸词匹配它们，如 gpu、build。">
                <ListEditor items={s.capabilities} placeholder="gpu" onChange={(capabilities) => update({ capabilities })} />
            </Section>
            {error && <div className="text-sm text-error-primary">{error}</div>}
            <div className="flex justify-end gap-2">
                <Button size="sm" color="secondary" onClick={onClose}>取消</Button>
                <Button size="sm" color="primary" isLoading={busy} onClick={() => void save()}>保存到这台机器</Button>
            </div>
        </div>
    );
}

function Section({ title, hint, children }: { title: string; hint?: string; children: React.ReactNode }) {
    return (
        <section className="flex flex-col gap-2">
            <div>
                <div className="text-xs font-medium uppercase tracking-wide text-quaternary">{title}</div>
                {hint && <div className="text-xs text-tertiary">{hint}</div>}
            </div>
            {children}
        </section>
    );
}

// ListEditor is a list of words: one per line, add below, remove beside.
function ListEditor({ items, placeholder, onChange }: { items: string[]; placeholder: string; onChange: (items: string[]) => void }) {
    const [draft, setDraft] = useState("");
    const add = () => {
        const v = draft.trim();
        if (!v || items.includes(v)) return;
        onChange([...items, v]);
        setDraft("");
    };
    return (
        <div className="flex flex-col gap-1.5">
            {items.map((x, i) => (
                <div key={x} className="flex items-center gap-2">
                    <span className="flex-1 rounded-md bg-secondary px-2 py-1 font-mono text-xs text-primary">{x}</span>
                    <ButtonUtility size="xs" color="tertiary" icon={Trash01} tooltip="移除" onClick={() => onChange(items.filter((_, j) => j !== i))} />
                </div>
            ))}
            <div className="flex items-center gap-2">
                <div className="flex-1"><Input size="sm" aria-label="新增" placeholder={placeholder} value={draft} onChange={setDraft} onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); add(); } }} /></div>
                <ButtonUtility size="xs" color="secondary" icon={Plus} tooltip="添加" onClick={add} />
            </div>
        </div>
    );
}

// KeyedRows edits a map of id → value as rows that can be renamed,
// removed, and added; the id is part of the row so a rename is a rename.
function KeyedRows<T>({ entries, onChange, blank, render }: {
    entries: Record<string, T>; onChange: (next: Record<string, T>) => void; blank: T;
    render: (id: { value: string; set: (v: string) => void }, value: T, set: (v: T) => void) => React.ReactNode;
}) {
    const [rows, setRows] = useState<{ id: string; value: T }[]>(() => Object.entries(entries).sort(([a], [b]) => a.localeCompare(b)).map(([id, value]) => ({ id, value })));
    const commit = (next: { id: string; value: T }[]) => {
        setRows(next);
        const out: Record<string, T> = {};
        for (const r of next) if (r.id.trim()) out[r.id.trim()] = r.value;
        onChange(out);
    };
    return (
        <div className="flex flex-col gap-1.5">
            {rows.map((r, i) => (
                <div key={i} className="flex items-start gap-2">
                    {render({ value: r.id, set: (v) => commit(rows.map((x, j) => (j === i ? { ...x, id: v } : x))) }, r.value, (v) => commit(rows.map((x, j) => (j === i ? { ...x, value: v } : x))))}
                    <ButtonUtility size="xs" color="tertiary" icon={Trash01} tooltip="移除" className="mt-1.5" onClick={() => commit(rows.filter((_, j) => j !== i))} />
                </div>
            ))}
            <div><Button size="sm" color="link-gray" iconLeading={Plus} onClick={() => commit([...rows, { id: "", value: blank }])}>添加一项</Button></div>
        </div>
    );
}

function MCPRow({ id, m, set }: { id: { value: string; set: (v: string) => void }; m: MCPSetting; set: (v: MCPSetting) => void }) {
    const stdio = !m.type || m.type === "stdio";
    return (
        <div className="flex flex-1 flex-col gap-2 rounded-lg bg-secondary/50 p-2">
            <div className="grid grid-cols-[1fr_120px] gap-2">
                <Input size="sm" aria-label="名字" placeholder="名字，如 github" value={id.value} onChange={id.set} />
                <Select size="sm" aria-label="类型" selectedKey={m.type || "stdio"} onSelectionChange={(k) => k && set({ ...m, type: String(k) })} items={[{ id: "stdio", label: "stdio" }, { id: "http", label: "http" }, { id: "sse", label: "sse" }]}>
                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                </Select>
            </div>
            {stdio ? (
                <div className="grid grid-cols-[1fr_1.4fr] gap-2">
                    <Input size="sm" aria-label="命令" placeholder="命令" value={m.command || ""} onChange={(v) => set({ ...m, command: v })} />
                    <Input size="sm" aria-label="参数" placeholder="参数，空格分隔" value={(m.args || []).join(" ")} onChange={(v) => set({ ...m, args: splitArgs(v) })} />
                </div>
            ) : (
                <Input size="sm" aria-label="地址" placeholder="https://…" value={m.url || ""} onChange={(v) => set({ ...m, url: v })} />
            )}
            <TextArea aria-label={stdio ? "环境变量" : "请求头"} rows={2} placeholder={stdio ? "环境变量，每行一个 KEY=value（只留在这台机器）" : "请求头，每行一个 Name: value（只留在这台机器）"}
                value={stdio ? kvLines(m.env, "=") : kvLines(m.headers, ": ")}
                onChange={(v) => set(stdio ? { ...m, env: parseKV(v, "=") } : { ...m, headers: parseKV(v, ":") })} />
        </div>
    );
}

function splitArgs(v: string): string[] { return v.split(/\s+/).map((x) => x.trim()).filter(Boolean); }
function kvLines(m: Record<string, string> | undefined, sep: string): string { return Object.entries(m || {}).map(([k, v]) => `${k}${sep}${v}`).join("\n"); }
function parseKV(text: string, sep: string): Record<string, string> {
    const out: Record<string, string> = {};
    for (const line of text.split("\n")) {
        const i = line.indexOf(sep);
        if (i <= 0) continue;
        out[line.slice(0, i).trim()] = line.slice(i + sep.length).trim();
    }
    return out;
}

export type { HarnessSetting };

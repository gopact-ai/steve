import { useEffect, useRef, useState } from "react";
import { Plus, Trash01 } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { ButtonUtility } from "@/components/base/buttons/button-utility";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { TextArea } from "@/components/base/textarea/textarea";
import { fetchNodeSettings, saveNodeSettings, type HarnessSetting } from "@/lib/api/fleet";
import { changeMapFormat, settingsDraft, parseSettings, type MapFormat, type MCPDraft, type SettingRow, type SettingsDraft } from "@/lib/settings-draft";

// SettingsEditor changes what a machine offers, from the page: its AI
// tools, the commands it checks for, its MCP servers, and what it merely
// declares. The machine validates, writes its own file, and reports back
// with a fresh snapshot; nothing here is saved until it says so.
export function SettingsEditor({ node, onClose, onSaved }: { node: string; onClose: () => void; onSaved: () => void }) {
    const [s, setS] = useState<SettingsDraft | null>(null);
    const [error, setError] = useState("");
    const [busy, setBusy] = useState(false);
    const saving = useRef(false);
    useEffect(() => {
        let alive = true;
        const controller = new AbortController();
        setS(null); setError("");
        void fetchNodeSettings(node, controller.signal).then((data) => { if (alive) setS(settingsDraft(data.settings)); }).catch((error) => { if (alive) setError(String(error).replace(/^Error: /, "")); });
        return () => { alive = false; controller.abort(); };
    }, [node]);
    if (!s) return <div className="text-sm text-tertiary">{error || "读取中…"}</div>;
    const update = (patch: Partial<SettingsDraft>) => setS({ ...s, ...patch });
    async function save() {
        if (!s || saving.current) return;
        saving.current = true; setBusy(true); setError("");
        try {
            await saveNodeSettings(node, parseSettings(s));
            onSaved();
            onClose();
        } catch (e) { setError(String(e).replace(/^Error: /, "")); } finally { saving.current = false; setBusy(false); }
    }
    return (
        <fieldset disabled={busy} className="flex min-w-0 flex-col gap-5">
            <Section title="AI 工具" hint="每个 AI 工具是一条启动命令；Agent 按名字用它。">
                <KeyedRows
                    rows={s.harnesses}
                    onChange={(harnesses) => update({ harnesses })}
                    blank={{ command: "" }}
                    render={(id, h, set) => (
                        <div className="grid flex-1 grid-cols-[1fr_1fr_1.4fr] gap-2">
                            <Input size="sm" aria-label="名字" placeholder="名字，如 codex" value={id.value} onChange={id.set} />
                            <Input size="sm" aria-label="命令" placeholder="命令，如 npx" value={h.command} onChange={(v) => set({ ...h, command: v })} />
                            <ArgumentsEditor args={h.args || []} onChange={(args) => set({ ...h, args })} />
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
                        rows={s.mcp_servers}
                        onChange={(mcp_servers) => update({ mcp_servers })}
                        blank={{ type: "stdio", command: "", envText: "", headersText: "", envFormat: "lines", headersFormat: "lines" }}
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
            {error && <div role="alert" className="text-sm text-error-primary">{error}</div>}
            <div className="flex justify-end gap-2">
                <Button size="sm" color="secondary" onClick={onClose}>取消</Button>
                <Button size="sm" color="primary" isLoading={busy} onClick={() => void save()}>保存到这台机器</Button>
            </div>
        </fieldset>
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
export function ListEditor({ items, placeholder, onChange }: { items: string[]; placeholder: string; onChange: (items: string[]) => void }) {
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
function KeyedRows<T>({ rows, onChange, blank, render }: {
    rows: SettingRow<T>[]; onChange: (next: SettingRow<T>[]) => void; blank: T;
    render: (id: { value: string; set: (value: string) => void }, value: T, set: (value: T) => void) => React.ReactNode;
}) {
    const nextKey = useRef(Math.max(-1, ...rows.map((row) => row.key)) + 1);
    return <div className="flex flex-col gap-1.5">
        {rows.map((row) => <div key={row.key} className="flex items-start gap-2">
            {render({ value: row.id, set: (id) => onChange(rows.map((item) => item.key === row.key ? { ...item, id } : item)) }, row.value,
                (value) => onChange(rows.map((item) => item.key === row.key ? { ...item, value } : item)))}
            <ButtonUtility size="xs" color="tertiary" icon={Trash01} tooltip="移除" className="mt-1.5" onClick={() => onChange(rows.filter((item) => item.key !== row.key))} />
        </div>)}
        <div><Button size="sm" color="link-gray" iconLeading={Plus} onClick={() => onChange([...rows, { key: nextKey.current++, id: "", value: blank }])}>添加一项</Button></div>
    </div>;
}

function ArgumentsEditor({ args, onChange }: { args: string[]; onChange: (args: string[]) => void }) {
    return <div className="flex min-w-0 flex-col gap-1.5">
        {args.map((argument, index) => <div key={index} className="flex min-w-0 items-center gap-1">
            <Input size="sm" aria-label={`参数 ${index + 1}`} value={argument} onChange={(value) => onChange(args.map((item, i) => i === index ? value : item))} />
            <ButtonUtility size="xs" color="tertiary" icon={Trash01} tooltip={`移除参数 ${index + 1}`} onClick={() => onChange(args.filter((_, i) => i !== index))} />
        </div>)}
        <Button size="sm" color="link-gray" className="self-start" iconLeading={Plus} onClick={() => onChange([...args, ""])}>添加参数</Button>
    </div>;
}

function MCPRow({ id, m, set }: { id: { value: string; set: (v: string) => void }; m: MCPDraft; set: (v: MCPDraft) => void }) {
    const stdio = !m.type || m.type === "stdio";
    const [formatError, setFormatError] = useState("");
    const mapLabel = stdio ? "环境变量" : "请求头";
    const format = stdio ? m.envFormat : m.headersFormat;
    function changeFormat(next: MapFormat) {
        setFormatError("");
        try {
            const text = changeMapFormat(stdio ? m.envText : m.headersText, format, next, stdio ? "=" : ":", mapLabel);
            set(stdio ? { ...m, envText: text, envFormat: next } : { ...m, headersText: text, headersFormat: next });
        } catch (error) { setFormatError(error instanceof Error ? error.message : String(error)); }
    }
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
                    <ArgumentsEditor args={m.args || []} onChange={(args) => set({ ...m, args })} />
                </div>
            ) : (
                <Input size="sm" aria-label="地址" placeholder="https://…" value={m.url || ""} onChange={(v) => set({ ...m, url: v })} />
            )}
            <Select size="sm" label={`${mapLabel}格式`} selectedKey={format} onSelectionChange={(key) => key && changeFormat(String(key) as MapFormat)}
                items={[{ id: "lines", label: stdio ? "每行 KEY=value" : "每行 Name: value" }, { id: "json", label: "JSON 对象（支持多行值）" }]}>
                {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
            </Select>
            <TextArea aria-label={mapLabel} rows={format === "json" ? 5 : 2} textAreaClassName="font-mono text-xs"
                hint={format === "json" ? "值使用 JSON 字符串；换行写为 \\n，双引号写为 \\\"。保存前会校验，不会改动未完成的输入。" : "每行一个名称和值；需要多行值时切换为 JSON 对象。内容只保留在这台机器。"}
                placeholder={format === "json" ? '{"KEY": "first\\nsecond"}' : stdio ? "KEY=value" : "Name: value"}
                value={stdio ? m.envText : m.headersText}
                onChange={(value) => { setFormatError(""); set(stdio ? { ...m, envText: value } : { ...m, headersText: value }); }} />
            {formatError && <p role="alert" className="text-xs text-error-primary">{formatError}</p>}
        </div>
    );
}

export type { HarnessSetting };

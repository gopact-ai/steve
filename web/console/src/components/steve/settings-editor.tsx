import { useI18n } from "@/providers/locale-provider";
import { errorText } from "@/lib/i18n";
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
    const { t: tr, locale } = useI18n();
    const [s, setS] = useState<SettingsDraft | null>(null);
    const [error, setError] = useState<Error | string>("");
    const [busy, setBusy] = useState(false);
    const saving = useRef(false);
    useEffect(() => {
        let alive = true;
        const controller = new AbortController();
        setS(null); setError("");
        void fetchNodeSettings(node, controller.signal).then((data) => { if (alive) setS(settingsDraft(data.settings)); }).catch((error) => { if (alive) setError(String(error).replace(/^Error: /, "")); });
        return () => { alive = false; controller.abort(); };
    }, [node]);
    if (!s) return <div className="text-sm text-tertiary">{error ? errorText(error, locale) : tr("settingsEditor.loading")}</div>;
    const update = (patch: Partial<SettingsDraft>) => setS({ ...s, ...patch });
    async function save() {
        if (!s || saving.current) return;
        saving.current = true; setBusy(true); setError("");
        try {
            await saveNodeSettings(node, parseSettings(s));
            onSaved();
            onClose();
        } catch (e) { setError(e instanceof Error ? e : String(e)); } finally { saving.current = false; setBusy(false); }
    }
    return (
        <fieldset disabled={busy} className="flex min-w-0 flex-col gap-5">
            <Section title={tr("settingsEditor.harnesses")} hint={tr("settingsEditor.harnessHint")}>
                <KeyedRows
                    rows={s.harnesses}
                    onChange={(harnesses) => update({ harnesses })}
                    blank={{ command: "" }}
                    render={(id, h, set) => (
                        <div className="grid flex-1 grid-cols-[1fr_1fr_1.4fr] gap-2">
                            <Input size="sm" aria-label={tr("settingsEditor.name")} placeholder={tr("settingsEditor.harnessPlaceholder")} value={id.value} onChange={id.set} />
                            <Input size="sm" aria-label={tr("settingsEditor.command")} placeholder={tr("settingsEditor.commandPlaceholder")} value={h.command} onChange={(v) => set({ ...h, command: v })} />
                            <ArgumentsEditor args={h.args || []} onChange={(args) => set({ ...h, args })} />
                        </div>
                    )}
                />
            </Section>
            <Section title={tr("settingsEditor.command")} hint={tr("settingsEditor.commandsHint")}>
                <ListEditor items={s.tools} placeholder="docker" onChange={(tools) => update({ tools })} />
            </Section>
            <Section title={tr("settingsEditor.mcpServers")} hint={s.external_broker ? tr("settingsEditor.brokerHint") : tr("settingsEditor.mcpHint")}>
                {!s.external_broker && (
                    <KeyedRows
                        rows={s.mcp_servers}
                        onChange={(mcp_servers) => update({ mcp_servers })}
                        blank={{ type: "stdio", command: "", envText: "", headersText: "", envFormat: "lines", headersFormat: "lines" }}
                        render={(id, m, set) => <MCPRow id={id} m={m} set={set} />}
                    />
                )}
            </Section>
            <Section title={tr("settingsEditor.declares")} hint={tr("settingsEditor.declaresHint")}>
                <ListEditor items={s.declares} placeholder="network:office" onChange={(declares) => update({ declares })} />
            </Section>
            <Section title={tr("settingsEditor.labels")} hint={tr("settingsEditor.labelsHint")}>
                <ListEditor items={s.capabilities} placeholder="gpu" onChange={(capabilities) => update({ capabilities })} />
            </Section>
            {error && <div role="alert" className="text-sm text-error-primary">{errorText(error, locale)}</div>}
            <div className="flex justify-end gap-2">
                <Button size="sm" color="secondary" onClick={onClose}>{tr("common.cancel")}</Button>
                <Button size="sm" color="primary" isLoading={busy} onClick={() => void save()}>{tr("settingsEditor.saveMachine")}</Button>
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
    const { t: tr } = useI18n();
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
                    <ButtonUtility size="xs" color="tertiary" icon={Trash01} tooltip={tr("common.remove")} onClick={() => onChange(items.filter((_, j) => j !== i))} />
                </div>
            ))}
            <div className="flex items-center gap-2">
                <div className="flex-1"><Input size="sm" aria-label={tr("settingsEditor.new")} placeholder={placeholder} value={draft} onChange={setDraft} onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); add(); } }} /></div>
                <ButtonUtility size="xs" color="secondary" icon={Plus} tooltip={tr("common.add")} onClick={add} />
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
    const { t: tr } = useI18n();
    const nextKey = useRef(Math.max(-1, ...rows.map((row) => row.key)) + 1);
    return <div className="flex flex-col gap-1.5">
        {rows.map((row) => <div key={row.key} className="flex items-start gap-2">
            {render({ value: row.id, set: (id) => onChange(rows.map((item) => item.key === row.key ? { ...item, id } : item)) }, row.value,
                (value) => onChange(rows.map((item) => item.key === row.key ? { ...item, value } : item)))}
            <ButtonUtility size="xs" color="tertiary" icon={Trash01} tooltip={tr("common.remove")} className="mt-1.5" onClick={() => onChange(rows.filter((item) => item.key !== row.key))} />
        </div>)}
        <div><Button size="sm" color="link-gray" iconLeading={Plus} onClick={() => onChange([...rows, { key: nextKey.current++, id: "", value: blank }])}>{tr("settingsEditor.addItem")}</Button></div>
    </div>;
}

function ArgumentsEditor({ args, onChange }: { args: string[]; onChange: (args: string[]) => void }) {
    const { t: tr } = useI18n();
    return <div className="flex min-w-0 flex-col gap-1.5">
        {args.map((argument, index) => <div key={index} className="flex min-w-0 items-center gap-1">
            <Input size="sm" aria-label={tr("settingsEditor.argument", { index: index + 1 })} value={argument} onChange={(value) => onChange(args.map((item, i) => i === index ? value : item))} />
            <ButtonUtility size="xs" color="tertiary" icon={Trash01} tooltip={tr("settingsEditor.removeArgument", { index: index + 1 })} onClick={() => onChange(args.filter((_, i) => i !== index))} />
        </div>)}
        <Button size="sm" color="link-gray" className="self-start" iconLeading={Plus} onClick={() => onChange([...args, ""])}>{tr("settingsEditor.addArgument")}</Button>
    </div>;
}

function MCPRow({ id, m, set }: { id: { value: string; set: (v: string) => void }; m: MCPDraft; set: (v: MCPDraft) => void }) {
    const { t: tr, locale } = useI18n();
    const stdio = !m.type || m.type === "stdio";
    const [formatError, setFormatError] = useState<Error | string>("");
    const mapLabel = stdio ? tr("settingsEditor.environment") : tr("settingsEditor.headers");
    const format = stdio ? m.envFormat : m.headersFormat;
    function changeFormat(next: MapFormat) {
        setFormatError("");
        try {
            const text = changeMapFormat(stdio ? m.envText : m.headersText, format, next, stdio ? "=" : ":", { kind: stdio ? "environment" : "headers" });
            set(stdio ? { ...m, envText: text, envFormat: next } : { ...m, headersText: text, headersFormat: next });
        } catch (error) { setFormatError(error instanceof Error ? error : String(error)); }
    }
    return (
        <div className="flex flex-1 flex-col gap-2 rounded-lg bg-secondary/50 p-2">
            <div className="grid grid-cols-[1fr_120px] gap-2">
                <Input size="sm" aria-label={tr("settingsEditor.name")} placeholder={tr("settingsEditor.mcpPlaceholder")} value={id.value} onChange={id.set} />
                <Select size="sm" aria-label={tr("settingsEditor.type")} selectedKey={m.type || "stdio"} onSelectionChange={(k) => k && set({ ...m, type: String(k) })} items={[{ id: "stdio", label: "stdio" }, { id: "http", label: "http" }, { id: "sse", label: "sse" }]}>
                    {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                </Select>
            </div>
            {stdio ? (
                <div className="grid grid-cols-[1fr_1.4fr] gap-2">
                    <Input size="sm" aria-label={tr("settingsEditor.command")} placeholder={tr("settingsEditor.command")} value={m.command || ""} onChange={(v) => set({ ...m, command: v })} />
                    <ArgumentsEditor args={m.args || []} onChange={(args) => set({ ...m, args })} />
                </div>
            ) : (
                <Input size="sm" aria-label={tr("settingsEditor.address")} placeholder="https://…" value={m.url || ""} onChange={(v) => set({ ...m, url: v })} />
            )}
            <Select size="sm" label={tr("settingsEditor.format", { field: mapLabel })} selectedKey={format} onSelectionChange={(key) => key && changeFormat(String(key) as MapFormat)}
                items={[{ id: "lines", label: stdio ? tr("settingsEditor.lineEnv") : tr("settingsEditor.lineHeader") }, { id: "json", label: tr("settingsEditor.jsonMode") }]}>
                {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
            </Select>
            <TextArea aria-label={mapLabel} rows={format === "json" ? 5 : 2} textAreaClassName="font-mono text-xs"
                hint={format === "json" ? tr("settingsEditor.jsonHint") : tr("settingsEditor.linesHint")}
                placeholder={format === "json" ? '{"KEY": "first\\nsecond"}' : stdio ? "KEY=value" : "Name: value"}
                value={stdio ? m.envText : m.headersText}
                onChange={(value) => { setFormatError(""); set(stdio ? { ...m, envText: value } : { ...m, headersText: value }); }} />
            {formatError && <p role="alert" className="text-xs text-error-primary">{errorText(formatError, locale)}</p>}
        </div>
    );
}

export type { HarnessSetting };

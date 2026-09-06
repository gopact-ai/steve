import { useI18n } from "@/providers/locale-provider";
import { number } from "@/lib/format";
import { useState, type KeyboardEvent, type RefObject } from "react";
import { ArrowUp, ChevronDown, CornerDownRight, DotsHorizontal, Edit05, Folder, MessageChatSquare, Plus, Square, Trash01 } from "@untitledui/icons";
import { Button as AriaButton } from "react-aria-components";
import { Dropdown } from "@/components/base/dropdown/dropdown";
import type { ConversationContext, Exchange, Project, QuoteRef, Selectors, Suggestion, Verb } from "@/lib/types";

// Composer is the console's input, in the proportions of a chat app's:
// a textarea that grows, a row of small round controls under it — the
// verbs, the project, the agent — and a send / stop button. The
// completion popover sits above it. Queue edits stay drafts until saved.
// Queued is a line typed while a turn runs: it waits above the box until
// the turn ends, unless the person steers it in now or takes it back.
export type Queued = Exchange;

export interface ComposerProps {
    // Selectors are fetched when the model chip opens — that may open a
    // session — and a choice is a preference for this thread's agent.
    onSelectors?: () => Promise<Selectors>;
    onPrefer?: (patch: Record<string, string>) => Promise<void>;
    preferenceKey?: string;
    quotes?: QuoteRef[];
    onDropQuote?: (q: QuoteRef) => void;
    queue?: Queued[];
    onSteer?: (q: Queued) => void;
    onEditQueued?: (q: Queued, input: string) => Promise<void>;
    onDropQueued?: (q: Queued) => void;
    onSideChat?: (q: Queued) => void;
    queueing?: boolean;
    onToggleQueueing?: () => void;
    value: string;
    hasMaterials?: boolean;
    onChange: (s: string) => void;
    onSubmit: () => void;
    onStop: () => void;
    busy: boolean;
    pending?: boolean;
    stopping?: boolean;
    disabled?: boolean;
    boxRef: RefObject<HTMLTextAreaElement | null>;
    onKey: (e: KeyboardEvent<HTMLTextAreaElement>) => void;
    suggestions: Suggestion[];
    pick: number;
    onApply: (s: Suggestion) => void;
    verbs: Verb[];
    onVerb: (command: string) => void;
    projects: Project[];
    project?: ConversationContext["project"] | null;
    onProject: (id: string) => void;
    agents: ConversationContext["agents"];
    agent?: ConversationContext["agent"] | null;
    onAgent: (id: string) => void;
}

const chip = "composer-chip";

export function Composer(p: ComposerProps) {
    const { t } = useI18n();
    return (
        <div className="composer">
            {p.suggestions.length > 0 && (
                <div className="absolute bottom-full left-0 z-10 mb-2 w-full max-w-2xl overflow-hidden rounded-xl bg-primary shadow-lg ring-1 ring-secondary">
                    <ul className="max-h-72 overflow-y-auto py-1">
                        {p.suggestions.map((sg, i) => (
                            <li key={sg.insert + i}>
                                <button type="button" onMouseDown={(e) => { e.preventDefault(); p.onApply(sg); }}
                                    className={`flex w-full items-baseline gap-3 px-3 py-1.5 text-left text-sm ${i === p.pick ? "bg-secondary" : "hover:bg-secondary"} ${sg.muted ? "opacity-60" : ""}`}>
                                    <span className="shrink-0 font-mono text-xs text-primary">{sg.label}</span>
                                    {sg.args && <span className="shrink-0 font-mono text-xs text-quaternary">{sg.args}</span>}
                                    <span className="truncate text-xs text-tertiary">{sg.detail}</span>
                                </button>
                            </li>
                        ))}
                    </ul>
                    <div className="border-t border-secondary px-3 py-1 u-meta text-quaternary">{t("consoleChrome.completionKeys")}</div>
                </div>
            )}
            {p.queue && p.queue.length > 0 && (
                <ul className="mb-1 flex flex-col gap-1">
                    {p.queue.map((q) => (
                        <QueuedLine key={q.id} q={q} p={p} />
                    ))}
                </ul>
            )}
            <div className="composer-input">
                {p.quotes && p.quotes.length > 0 && (
                    <ul className="flex flex-wrap gap-1.5 px-3 pt-3">
                        {p.quotes.map((x) => (
                            <li key={x.conversation + x.reply_id} className="flex max-w-full items-center gap-1.5 rounded-lg border-l-2 border-brand bg-secondary px-2 py-1 text-xs text-secondary" title={x.excerpt}>
                                <span className="shrink-0 text-quaternary">{t("consoleChrome.quotedFrom", { title: x.title || x.conversation })}</span>
                                <span className="min-w-0 truncate">{x.excerpt}</span>
                                <button type="button" onClick={() => p.onDropQuote?.(x)} className="shrink-0 text-quaternary hover:text-primary" aria-label={t("consoleChrome.dropQuote")}>×</button>
                            </li>
                        ))}
                    </ul>
                )}
                <textarea
                    ref={p.boxRef}
                    aria-label={t("consoleChrome.message")}
                    value={p.value}
                    rows={1}
                    disabled={p.disabled || (p.busy && p.queueing === false)}
                    placeholder={p.disabled ? t("consoleChrome.preparing") : p.busy ? (p.queueing === false ? t("consoleChrome.processing") : t("consoleChrome.queuePlaceholder")) : t("consoleChrome.placeholder")}
                    onChange={(e) => p.onChange(e.target.value)}
                    onKeyDown={p.onKey}
                    className="composer-textarea"
                />
                <div className="composer-controls"><div className="composer-options">
                    <Dropdown.Root>
                        <AriaButton aria-label={t("consoleChrome.verbs")} className="workbench-icon-button">
                            <Plus className="size-4" />
                        </AriaButton>
                        <Dropdown.Popover placement="top start" className="w-80">
                            <Dropdown.Menu onAction={(k) => p.onVerb(String(k))}>
                                <Dropdown.Section>
                                    <Dropdown.SectionHeader className="px-2 py-1 u-meta text-quaternary">{t("consoleChrome.verbHint")}</Dropdown.SectionHeader>
                                    {p.verbs.map((v) => (
                                        <Dropdown.Item key={v.command} id={v.command} textValue={v.command}>
                                            <div className="flex min-w-0 flex-col">
                                                <span className="font-mono text-xs text-primary">{v.command} <span className="text-quaternary">{v.args || ""}</span></span>
                                                <span className="truncate text-xs text-tertiary">{v.summary}</span>
                                            </div>
                                        </Dropdown.Item>
                                    ))}
                                </Dropdown.Section>
                            </Dropdown.Menu>
                        </Dropdown.Popover>
                    </Dropdown.Root>
                    <Dropdown.Root>
                        <AriaButton isDisabled={p.disabled || p.pending} aria-label={t("console.project")} className={`${chip} text-tertiary hover:text-secondary`}>
                            <Folder className="size-3.5" />
                            <span>{p.project?.id || t("console.project")}</span>
                            <ChevronDown className="size-3 text-fg-quaternary" />
                        </AriaButton>
                        <Dropdown.Popover placement="top start" className="w-80">
                            <Dropdown.Menu onAction={(k) => { if (String(k) !== p.project?.id) p.onProject(String(k)); }}>
                                {p.projects.map((x) => <Dropdown.Item key={x.id} id={x.id} textValue={x.id} label={x.id} addon={x.node} />)}
                            </Dropdown.Menu>
                        </Dropdown.Popover>
                    </Dropdown.Root>
                    <button type="button" onClick={p.onToggleQueueing} aria-pressed={p.queueing !== false} className={`${chip} shrink-0 whitespace-nowrap text-quaternary`} title={p.queueing === false ? t("consoleChrome.enableQueue") : t("consoleChrome.disableQueue")}>
                        <CornerDownRight className="size-3.5" aria-hidden="true" /><span>{p.queueing === false ? t("consoleChrome.noQueue") : t("consoleChrome.queue")}</span>
                    </button>
                    <Dropdown.Root>
                        <AriaButton isDisabled={p.disabled || p.pending} aria-label="Agent" className={`${chip} text-secondary`}>
                            <span>{p.agent?.id || "Agent"}</span>
                            <ChevronDown className="size-3 text-fg-quaternary" />
                        </AriaButton>
                        <Dropdown.Popover placement="top end" className="w-96">
                            <Dropdown.Menu onAction={(k) => { if (String(k) !== p.agent?.id) p.onAgent(String(k)); }}>
                                {(p.agents ?? []).map((a) => (
                                    <Dropdown.Item key={a.id} id={a.id} textValue={a.id} isDisabled={!a.usable}>
                                        <div className="flex min-w-0 flex-col">
                                            <span className="text-sm text-primary">{a.id} <span className="text-xs text-quaternary">{a.node} · {a.harness}{a.model ? " · " + a.model : ""}</span></span>
                                            {!a.usable && <span className="truncate text-xs text-tertiary">{a.because || a.why}</span>}
                                        </div>
                                    </Dropdown.Item>
                                ))}
                            </Dropdown.Menu>
                        </Dropdown.Popover>
                    </Dropdown.Root>
                    {p.agent && p.onSelectors && <PreferenceChips key={p.preferenceKey || p.agent.id} agent={p.agent} load={p.onSelectors} onPrefer={p.onPrefer} />}
                    </div><div className="composer-actions">
                    {p.busy && (
                        <button type="button" aria-label={p.stopping ? t("consoleChrome.stopping") : t("consoleChrome.stop")} title={p.stopping ? t("console.stopping") : t("consoleChrome.stopHint")} disabled={p.stopping} onClick={p.onStop}
                            className="composer-action is-stop">
                            <Square className="size-3.5" />
                        </button>
                    )}
                    {p.busy ? (p.value.trim() || p.hasMaterials) && p.queueing !== false && (
                        <button type="button" aria-label={t("consoleChrome.queue")} title={p.pending ? t("console.sending") : t("consoleChrome.queueHint")} disabled={p.disabled || p.pending || p.stopping} onClick={p.onSubmit}
                            className="composer-action">
                            <CornerDownRight className="size-4" />
                        </button>
                    ) : (
                        <button type="button" aria-label={t("consoleChrome.send")} title={p.pending ? t("console.sending") : t("consoleChrome.sendHint")} disabled={p.disabled || p.pending || (!p.value.trim() && !p.hasMaterials)} onClick={p.onSubmit}
                            className="composer-action is-send">
                            <ArrowUp className="size-4" />
                        </button>
                    )}
                </div></div>
            </div>
        </div>
    );
}


// Editing never removes a durable entry. Another tab may start or delete
// it meanwhile; the API reports that conflict without resubmitting it.
function QueuedLine({ q, p }: { q: Queued; p: ComposerProps }) {
    const { t, locale } = useI18n();
    const [editing, setEditing] = useState(false);
    const [draft, setDraft] = useState(q.input);
    const [saving, setSaving] = useState(false);
    const [error, setError] = useState("");
    const save = async () => {
        if (!draft.trim() || saving || !p.onEditQueued) return;
        setSaving(true);
        try { await p.onEditQueued(q, draft.trim()); setEditing(false); setError(""); }
        catch (e) { setError(String(e).replace(/^Error: /, "")); }
        finally { setSaving(false); }
    };
    return (
        <li className="flex flex-col gap-1 rounded-xl bg-secondary/70 px-3 py-1.5 text-sm ring-1 ring-secondary">
            <div className="flex items-center gap-2">
                <CornerDownRight className="size-3.5 shrink-0 text-fg-quaternary" />
                {editing ? <>
                    <textarea aria-label={t("consoleChrome.editQueued")} value={draft} onChange={(e) => setDraft(e.target.value)} disabled={saving} className="min-w-0 flex-1 rounded bg-primary px-2 py-1 text-primary" />
                    <button type="button" disabled={saving || !draft.trim()} onClick={() => void save()} className="text-xs text-tertiary">{t("common.save")}</button>
                    <button type="button" disabled={saving} onClick={() => { setEditing(false); setError(""); }} className="text-xs text-tertiary">{t("common.cancel")}</button>
                </> : <>
                    <span className="min-w-0 flex-1 truncate text-primary" title={q.input}>{q.input}</span>
                    {!!q.quotes?.length && <span className="text-xs text-quaternary">{t("consoleChrome.quotes", { count: number(q.quotes.length, locale) })}</span>}
                    <button type="button" onClick={() => p.onSteer?.(q)} className="shrink-0 rounded-md px-1.5 py-0.5 text-xs text-tertiary hover:bg-primary hover:text-primary" title={t("consoleChrome.steerHint")}>{t("consoleChrome.steer")}</button>
                    <button type="button" onClick={() => p.onDropQueued?.(q)} className="flex size-6 shrink-0 items-center justify-center rounded-md text-fg-quaternary hover:bg-primary" aria-label={t("common.delete")} title={t("common.delete")}><Trash01 className="size-3.5" /></button>
                    <Dropdown.Root>
                        <AriaButton aria-label={t("consoleChrome.more")} className="flex size-6 shrink-0 items-center justify-center rounded-md text-fg-quaternary outline-none hover:bg-primary"><DotsHorizontal className="size-3.5" /></AriaButton>
                        <Dropdown.Popover placement="top end" className="w-44">
                            <Dropdown.Menu onAction={(k) => {
                                if (k === "edit") { setDraft(q.input); setEditing(true); }
                                else if (k === "side") p.onSideChat?.(q);
                                else if (k === "off") p.onToggleQueueing?.();
                            }}>
                                <Dropdown.Item id="edit" label={t("common.edit")} icon={Edit05} />
                                <Dropdown.Item id="side" label={t("consoleChrome.sideChat")} icon={MessageChatSquare} />
                                <Dropdown.Item id="off" label={p.queueing === false ? t("consoleChrome.enableQueue") : t("consoleChrome.disableQueue")} />
                            </Dropdown.Menu>
                        </Dropdown.Popover>
                    </Dropdown.Root>
                </>}
            </div>
            {error && <span className="text-xs text-error-primary">{error}</span>}
        </li>
    );
}

// PreferenceChips are the model and, when the harness offers one, the
// reasoning level of the current agent in this thread. The choices come
// from the harness itself when the chip opens; picking one is remembered
// for this thread and takes effect from the next turn, in a fresh session.
function PreferenceChips({ agent, load, onPrefer }: { agent: NonNullable<ConversationContext["agent"]>; load: () => Promise<Selectors>; onPrefer?: (patch: Record<string, string>) => Promise<void> }) {
    const { t } = useI18n();
    const [sel, setSel] = useState<Selectors | null>(null);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState("");
    const open = () => { if (busy) return; setError(""); if (sel) return; setBusy(true); load().then(setSel).catch((e) => setError(String(e).replace(/^Error: /, ""))).finally(() => setBusy(false)); };
    const prefer = async (patch: Record<string, string>) => {
        if (busy || !onPrefer) return;
        setBusy(true); setError("");
        try { await onPrefer(patch); setSel(await load()); }
        catch (e) { setError(String(e).replace(/^Error: /, "")); }
        finally { setBusy(false); }
    };
    const reasoning = sel?.options.find((o) => /reason|effort|think/i.test(o.ID + " " + (o.Category || "") + " " + o.Name));
    const modelLabel = sel?.preferred?.model || agent.model || t("consoleChrome.model");
    return (
        <>
            <Dropdown.Root onOpenChange={(isOpen) => { if (isOpen) open(); }}>
                <AriaButton isDisabled={busy} aria-label={t("consoleChrome.model")} className={`${chip} text-quaternary`}>
                    <span className="max-w-40 truncate">{modelLabel}</span>
                    <ChevronDown className="size-3" />
                </AriaButton>
                <Dropdown.Popover placement="top start" className="w-72">
                    {error && !sel ? <div className="px-3 py-2 text-xs text-error-primary">{error}</div> : !sel ? <div className="px-3 py-2 text-xs text-quaternary">{t("consoleChrome.loadingChoices")}</div> : (
                        <Dropdown.Menu onAction={(k) => void prefer({ model: String(k) })}>
                            <Dropdown.Section>
                                <Dropdown.SectionHeader className="px-2 py-1 u-meta text-quaternary">{t("consoleChrome.currentModel", { model: sel.model || t("common.unknown") })}{sel.preferred?.model ? ` · ${t("consoleChrome.preferred", { model: sel.preferred.model })}` : ""}</Dropdown.SectionHeader>
                                {sel.models.length === 0 && <Dropdown.Item id="__none" label={t("consoleChrome.noModelSelector")} isDisabled />}
                                {sel.models.map((c) => <Dropdown.Item key={c.Value} id={c.Value} label={c.Detail ? `${c.Label || c.Value} · ${c.Detail}` : (c.Label || c.Value)} />)}
                            </Dropdown.Section>
                        </Dropdown.Menu>
                    )}
                </Dropdown.Popover>
            </Dropdown.Root>
            {error && <span role="alert" className="text-xs text-error-primary">{error}</span>}
            {reasoning && (
                <Dropdown.Root>
                    <AriaButton isDisabled={busy} aria-label={reasoning.Name} className={`${chip} text-quaternary`}>
                        <span className="max-w-32 truncate">{sel?.preferred?.[reasoning.ID] || reasoning.Current || reasoning.Name}</span>
                        <ChevronDown className="size-3" />
                    </AriaButton>
                    <Dropdown.Popover placement="top start" className="w-60">
                        <Dropdown.Menu onAction={(k) => void prefer({ [reasoning.ID]: String(k) })}>
                            <Dropdown.Section>
                                <Dropdown.SectionHeader className="px-2 py-1 u-meta text-quaternary">{t("consoleChrome.currentOption", { name: reasoning.Name, value: reasoning.Current || t("common.unknown") })}</Dropdown.SectionHeader>
                                {reasoning.Choices.map((c) => <Dropdown.Item key={c.Value} id={c.Value} label={c.Detail ? `${c.Label || c.Value} · ${c.Detail}` : (c.Label || c.Value)} />)}
                            </Dropdown.Section>
                        </Dropdown.Menu>
                    </Dropdown.Popover>
                </Dropdown.Root>
            )}
        </>
    );
}

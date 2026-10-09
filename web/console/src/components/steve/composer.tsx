import { Button } from "@/components/base/buttons/button";
import { IconButton } from "@/components/steve/icon-button";
import { useI18n } from "@/providers/locale-provider";
import { number } from "@/lib/format";
import { lazy, memo, useEffect, useRef, useState, type RefObject } from "react";
import { ArrowUp, ChevronDown, CornerDownRight, DotsHorizontal, Edit05, Folder, MessageChatSquare, Plus, Square, Trash01 } from "@untitledui/icons";
import { Button as AriaButton, Dialog, DialogTrigger, Popover } from "react-aria-components";
import { Dropdown } from "@/components/base/dropdown/dropdown";
import type { ConversationContext, Exchange, Project, QuoteRef, Selectors, Suggestion, Verb } from "@/lib/types";
import { plain } from "@/lib/plain";
import { MarkdownInput, type DraftBox } from "@/components/steve/markdown-input";
import { useNodeLabel } from "@/lib/node-name";
import { LazyRegion } from "@/components/steve/lazy-region";
import { ownPreference, sessionOption, sessionOptionRole } from "@/lib/session-options";

// Composer is the console's input, in the proportions of a chat app's:
// a textarea that grows, a row of small round controls under it — the
// verbs, the project, the agent — and a send / stop button. The
// completion popover sits above it. Queue edits stay drafts until saved.
// Queued is a line typed while a turn runs: it waits above the box until
// the turn ends, unless the person steers it in now or takes it back.
export type Queued = Exchange;

export interface ComposerProps {
    // Selectors are fetched when either preference chip opens — that may open a
    // session — and a choice is a preference for this thread's agent.
    onSelectors?: (scope?: string) => Promise<Selectors>;
    onPrefer?: (patch: Record<string, string>, scope?: string) => Promise<void>;
    preferenceKey?: string;
    quotes?: QuoteRef[];
    onDropQuote?: (q: QuoteRef) => void;
    // Rewriting a line already sent takes the thread back to it, so the
    // box says what sending now will undo before it is sent.
    rewind?: { following: number };
    onCancelRewind?: () => void;
    queue?: Queued[];
    onSteer?: (q: Queued) => void;
    onEditQueued?: (q: Queued, input: string) => Promise<void>;
    onDropQueued?: (q: Queued) => void;
    onSideChat?: (q: Queued) => void;
    queueing?: boolean;
    onToggleQueueing?: () => void;
    value: string;
    hasMaterials?: boolean;
    // A screenshot on the clipboard is an attachment, not text: the page
    // uploads it the way the attach button does. Text pasted alongside it
    // is still typed into the box.
    onPasteFiles?: (files: File[]) => void;
    onDropFiles?: (files: File[]) => void;
    onChange: (s: string) => void;
    onSubmit: () => void;
    onStop: () => void;
    busy: boolean;
    pending?: boolean;
    stopping?: boolean;
    disabled?: boolean;
    boxRef: RefObject<DraftBox | null>;
    onKey: (e: KeyboardEvent) => void;
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
const SessionOptionControl = lazy(() => import("@/components/steve/session-option").then(module => ({ default: module.SessionOptionControl })));

export const Composer = memo(function Composer(p: ComposerProps) {
    const { t } = useI18n();
    const nodeLabelOf = useNodeLabel();
    const inputDisabled = !!p.disabled || (p.busy && p.queueing === false);
    return (
        <div className="composer" onDragOver={(event) => {
            if (!p.onDropFiles || !Array.from(event.dataTransfer.types).includes("Files")) return;
            event.preventDefault();
            event.dataTransfer.dropEffect = inputDisabled ? "none" : "copy";
        }} onDrop={(event) => {
            const files = Array.from(event.dataTransfer.files);
            if (event.defaultPrevented || !p.onDropFiles || (!files.length && !Array.from(event.dataTransfer.types).includes("Files"))) return;
            event.preventDefault();
            if (!inputDisabled) p.onDropFiles(files);
        }}>
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
            {p.rewind && (
                <div role="status" className="mb-1 flex items-center gap-2 rounded-xl bg-secondary/70 px-3 py-1.5 text-xs ring-1 ring-secondary">
                    <Edit05 className="size-3.5 shrink-0 text-fg-quaternary" aria-hidden="true" />
                    <span className="min-w-0 flex-1 text-secondary">{p.rewind.following > 0 ? t("console.rewindNotice", { count: number(p.rewind.following) }) : t("console.rewindNoticeLast")}</span>
                    <button type="button" onClick={p.onCancelRewind} className="shrink-0 rounded-md px-1.5 py-0.5 text-tertiary hover:bg-primary hover:text-primary">{t("console.rewindCancel")}</button>
                </div>
            )}
            <div className="composer-input">
                {p.quotes && p.quotes.length > 0 && (
                    <ul className="flex flex-wrap gap-1.5 px-3 pt-3">
                        {p.quotes.map((x) => (
                            <li key={x.conversation + x.reply_id} className="flex max-w-full items-center gap-1.5 rounded-lg border-l-2 border-brand bg-secondary px-2 py-1 text-xs text-secondary" title={x.excerpt ? plain(x.excerpt) : undefined}>
                                <span className="shrink-0 text-quaternary">{t("consoleChrome.quotedFrom", { title: x.title || x.conversation })}</span>
                                <span className="min-w-0 truncate">{x.excerpt}</span>
                                <button type="button" onClick={() => p.onDropQuote?.(x)} className="shrink-0 text-quaternary hover:text-primary" aria-label={t("consoleChrome.dropQuote")}>×</button>
                            </li>
                        ))}
                    </ul>
                )}
                <MarkdownInput
                    handle={p.boxRef}
                    value={p.value}
                    label={t("consoleChrome.message")}
                    disabled={inputDisabled}
                    placeholder={p.disabled ? t("consoleChrome.preparing") : p.busy ? (p.queueing === false ? t("consoleChrome.processing") : t("consoleChrome.queuePlaceholder")) : t("consoleChrome.placeholder")}
                    onChange={p.onChange}
                    onKey={p.onKey}
                    onPasteFiles={p.onPasteFiles}
                    onDropFiles={p.onDropFiles}
                />
                <div className="composer-controls"><div className="composer-options">
                    <Dropdown.Root>
                        <IconButton label={t("consoleChrome.verbs")} icon={Plus} />
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
                    <Dropdown.Root>
                        <AriaButton isDisabled={p.disabled || p.pending} aria-label="Agent" className={`${chip} text-secondary`}>
                            <span>{p.agent?.id || "Agent"}</span>
                            <ChevronDown className="size-3 text-fg-quaternary" />
                        </AriaButton>
                        <Dropdown.Popover placement="top end" className="w-96">
                            <Dropdown.Menu onAction={(k) => { if (String(k) !== p.agent?.id) p.onAgent(String(k)); }}
                                renderEmptyState={() => <div className="px-3 py-2">
                                    <p className="text-sm text-secondary">{t("consoleChrome.noAgents")}</p>
                                    <p className="mt-1 text-xs text-tertiary">{t("consoleChrome.noAgentsHint")}</p>
                                </div>}>
                                {(p.agents ?? []).map((a) => (
                                    <Dropdown.Item key={a.id} id={a.id} textValue={a.id} isDisabled={!a.usable}>
                                        <div className="flex min-w-0 flex-col">
                                            <span className="text-sm text-primary">{a.id} <span className="text-xs text-quaternary">{a.node ? nodeLabelOf(a.node) : ""} · {a.harness}{a.model ? " · " + a.model : ""}</span></span>
                                            {!a.usable && <span className="truncate text-xs text-tertiary">{a.because || a.why}</span>}
                                        </div>
                                    </Dropdown.Item>
                                ))}
                            </Dropdown.Menu>
                        </Dropdown.Popover>
                    </Dropdown.Root>
                    </div><div className="composer-run">
                    {p.agent && p.onSelectors && <PreferenceChips key={p.preferenceKey || p.agent.id} scope={p.preferenceKey || p.agent.id} agent={p.agent} load={p.onSelectors} onPrefer={p.onPrefer} />}
                    <button type="button" onClick={p.onToggleQueueing} aria-pressed={p.queueing !== false} className={`${chip} shrink-0 whitespace-nowrap text-quaternary`} title={p.queueing === false ? t("consoleChrome.enableQueue") : t("consoleChrome.disableQueue")}>
                        <CornerDownRight className="size-3.5" aria-hidden="true" /><span>{p.queueing === false ? t("consoleChrome.noQueue") : t("consoleChrome.queue")}</span>
                    </button>
                    </div><div className="composer-actions">
                    {p.busy && (
                        <button type="button" aria-label={p.stopping ? t("consoleChrome.stopping") : t("consoleChrome.stop")} title={p.stopping ? t("console.stopping") : t("consoleChrome.stopHint")} disabled={p.stopping} onClick={p.onStop}
                            className="composer-action is-stop">
                            <Square className="size-3.5" />
                        </button>
                    )}
                    {p.busy ? (p.value.trim() || p.hasMaterials) && p.queueing !== false ? (
                        <button type="button" aria-label={t("consoleChrome.queue")} title={p.pending ? t("console.sending") : t("consoleChrome.queueHint")} disabled={p.disabled || p.pending || p.stopping} onClick={p.onSubmit}
                            className="composer-action">
                            <CornerDownRight className="size-4" />
                        </button>
                    ) : (
                        // Clearing a submitted draft must not move Stop into
                        // the slot where a second pointer click will land.
                        <span aria-hidden="true" className="composer-action invisible" />
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
});


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
                    <Button size="xs" color="link-gray" isDisabled={saving || !draft.trim()} onClick={() => void save()}>{t("common.save")}</Button>
                    <Button size="xs" color="link-gray" isDisabled={saving} onClick={() => { setEditing(false); setError(""); }}>{t("common.cancel")}</Button>
                </> : <>
                    <span className="min-w-0 flex-1 truncate text-primary" title={q.input}>{q.input}</span>
                    {!!q.quotes?.length && <span className="text-xs text-quaternary">{t("consoleChrome.quotes", { count: number(q.quotes.length, locale) })}</span>}
                    <Button size="xs" color="tertiary" onClick={() => p.onSteer?.(q)} title={t("consoleChrome.steerHint")}>{t("consoleChrome.steer")}</Button>
                    <IconButton size="xs" label={t("common.delete")} icon={Trash01} onClick={() => p.onDropQueued?.(q)} title={t("common.delete")} />
                    <Dropdown.Root>
                        <IconButton size="xs" label={t("consoleChrome.more")} icon={DotsHorizontal} />
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

// PreferenceChips expose the Agent's typed options. Choices are requests for
// this thread, not evidence of the settings a running session has accepted.
function PreferenceChips({ agent, scope, load, onPrefer }: { agent: NonNullable<ConversationContext["agent"]>; scope: string; load: (scope?: string) => Promise<Selectors>; onPrefer?: (patch: Record<string, string>, scope?: string) => Promise<void> }) {
    const { t } = useI18n();
    const [sel, setSel] = useState<Selectors | null>(null);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState("");
    const [accepted, setAccepted] = useState<Record<string, string>>({});
    const [optionsOpen, setOptionsOpen] = useState(false);
    const changing = useRef(false);
    const mounted = useRef(true);
    useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
    const publish = (value: Selectors) => { setSel(value); setAccepted({}); };
    const open = (refresh = false) => {
        if (busy || changing.current) return;
        setError(""); if (sel && !refresh) return; setBusy(true);
        load(scope).then(value => { if (mounted.current) publish(value); })
            .catch(e => { if (mounted.current) setError(String(e).replace(/^Error: /, "")); })
            .finally(() => { if (mounted.current) setBusy(false); });
    };
    const prefer = async (patch: Record<string, string>) => {
        if (busy || changing.current || !onPrefer) return;
        changing.current = true;
        setBusy(true); setError("");
        try {
            await onPrefer(patch, scope);
            if (!mounted.current) return;
            setAccepted(previous => ({ ...previous, ...patch }));
            const value = await load(scope);
            if (mounted.current) publish(value);
        }
        catch (e) { if (mounted.current) setError(String(e).replace(/^Error: /, "")); }
        finally { changing.current = false; if (mounted.current) setBusy(false); }
    };
    const requested = (id: string) => {
        const acknowledgement = ownPreference(accepted, id);
        return acknowledgement !== undefined && acknowledgement !== "" ? acknowledgement : ownPreference(sel?.preferred, id);
    };
    const preferenceLabel = (id: string) => ownPreference(accepted, id) === "" ? t("consoleChrome.optionResetPending") : requested(id);
    const reasoning = sel?.options?.find(option => sessionOptionRole(option) === "reasoning");
    // ACP reserves the "mode" category for the agent's approval behaviour
    // (ask, approve for me, full access); the category is advisory, so the
    // conventional id counts too.
    const approval = sel?.options?.find(option => sessionOptionRole(option) === "approval");
    const modelLabel = ownPreference(accepted, "model") === "" ? t("consoleChrome.optionResetPending") : requested("model") || sel?.model || agent.model || t("consoleChrome.model");
    const models = sel?.models ?? [];
    const choices = reasoning?.Choices ?? [];
    const effort = reasoning ? requested(reasoning.ID) || reasoning.Current : undefined;
    const effortLabel = reasoning && ownPreference(accepted, reasoning.ID) === "" ? t("consoleChrome.optionResetPending") : choices.find((c) => c.Value === effort)?.Label || effort;
    const reasoningLabel = t("consoleChrome.reasoningEffort");
    const approvalChoices = approval?.Choices ?? [];
    const approvalMode = approval ? requested(approval.ID) || approval.Current : undefined;
    const approvalModeLabel = approval && ownPreference(accepted, approval.ID) === "" ? t("consoleChrome.optionResetPending") : approvalChoices.find((c) => c.Value === approvalMode)?.Label || approvalMode;
    const approvalLabel = t("consoleChrome.approvalMode");
    const options = (sel?.options || []).filter(option => sessionOptionRole(option) !== "model" && option.ID !== approval?.ID && option.ID !== reasoning?.ID);
    return (
        <>
            <DialogTrigger isOpen={optionsOpen} onOpenChange={isOpen => { setOptionsOpen(isOpen); if (isOpen) open(); }}>
                <AriaButton aria-label={t("consoleChrome.sessionOptions")} data-preference-scope={scope} className={`${chip} text-quaternary`}>
                    {t("consoleChrome.sessionOptions")}<ChevronDown className="size-3" />
                </AriaButton>
                <Popover placement="top start" className="z-[100] w-80 max-w-[calc(100vw-24px)] rounded-lg bg-primary shadow-lg ring-1 ring-secondary">
                    <Dialog aria-label={t("consoleChrome.sessionOptions")} className="max-h-[min(560px,70dvh)] space-y-4 overflow-y-auto overscroll-contain p-3 outline-hidden" aria-busy={busy}>
                        <h3 className="text-sm font-semibold text-primary">{t("consoleChrome.sessionOptions")}</h3>
                        <p className="text-xs text-tertiary">{t("consoleChrome.optionPreferenceHint")}</p>
                        {!sel ? <p role="status" className="text-xs text-tertiary">{t("consoleChrome.loadingChoices")}</p> : options.length > 0 && <LazyRegion resetKey={scope} onClose={() => setOptionsOpen(false)}>
                            {options.map(raw => <SessionOptionControl key={raw.ID}
                                option={sessionOption(raw)} requested={requested(raw.ID)} acceptedRequest={ownPreference(accepted, raw.ID)} disabled={busy || !onPrefer}
                                onChange={value => void prefer({ [raw.ID]: value ?? "" })} />)}
                        </LazyRegion>}
                        {sel && options.length === 0 && <p className="text-xs text-tertiary">{t("consoleChrome.noSessionOptions")}</p>}
                        <Button size="sm" color="link-gray" isDisabled={busy} onClick={() => open(true)}>{t("consoleChrome.optionReload")}</Button>
                        {error && <p role="alert" className="break-words text-xs text-error-primary">{error}</p>}
                    </Dialog>
                </Popover>
            </DialogTrigger>
            <Dropdown.Root onOpenChange={(isOpen) => { if (isOpen) open(); }}>
                <AriaButton isDisabled={busy} aria-label={t("consoleChrome.model")} className={`${chip} text-quaternary`}>
                    <span className="max-w-40 truncate">{modelLabel}</span>
                    <ChevronDown className="size-3" />
                </AriaButton>
                <Dropdown.Popover placement="top start" className="w-72">
                    {error && !sel ? <div className="px-3 py-2 text-xs text-error-primary">{error}</div> : !sel ? <div className="px-3 py-2 text-xs text-quaternary">{t("consoleChrome.loadingChoices")}</div> : (
                        <Dropdown.Menu onAction={(k) => void prefer({ model: String(k) })}>
                            <Dropdown.Section>
                                <Dropdown.SectionHeader className="px-2 py-1 u-meta text-quaternary">{t("consoleChrome.currentModel", { model: sel.model || t("common.unknown") })}{preferenceLabel("model") ? ` · ${t("consoleChrome.preferred", { model: preferenceLabel("model")! })}` : ""}{ownPreference(accepted, "model") !== undefined ? ` · ${t("consoleChrome.optionReadbackPending")}` : ""}</Dropdown.SectionHeader>
                                {models.length === 0 && <Dropdown.Item id="__none" label={t("consoleChrome.noModelSelector")} isDisabled />}
                                {models.map((c) => <Dropdown.Item key={c.Value} id={c.Value} label={c.Detail ? `${c.Label || c.Value} · ${c.Detail}` : (c.Label || c.Value)} />)}
                            </Dropdown.Section>
                        </Dropdown.Menu>
                    )}
                </Dropdown.Popover>
            </Dropdown.Root>
            {error && <span role="alert" className="text-xs text-error-primary">{error}</span>}
            <Dropdown.Root onOpenChange={(isOpen) => { if (isOpen) open(); }}>
                <AriaButton isDisabled={busy} aria-label={approvalLabel} className={`${chip} text-quaternary`}>
                    <span className="max-w-40 truncate">{approvalModeLabel ? `${approvalLabel} · ${approvalModeLabel}` : approvalLabel}</span>
                    <ChevronDown className="size-3" />
                </AriaButton>
                <Dropdown.Popover placement="top start" className="w-60">
                    {error && !sel ? <div className="px-3 py-2 text-xs text-error-primary">{error}</div> : !sel ? <div className="px-3 py-2 text-xs text-quaternary">{t("consoleChrome.loadingChoices")}</div> : (
                        <Dropdown.Menu onAction={(k) => { if (approval) void prefer({ [approval.ID]: String(k) }); }}>
                            <Dropdown.Section>
                                <Dropdown.SectionHeader className="px-2 py-1 u-meta text-quaternary">{t("consoleChrome.currentOption", { name: approvalLabel, value: approvalChoices.find(c => c.Value === approval?.Current)?.Label || approval?.Current || t("common.unknown") })}{approval && preferenceLabel(approval.ID) ? ` · ${t("consoleChrome.preferred", { model: approvalModeLabel || "" })}` : ""}{approval && ownPreference(accepted, approval.ID) !== undefined ? ` · ${t("consoleChrome.optionReadbackPending")}` : ""}</Dropdown.SectionHeader>
                                {approvalChoices.length === 0 && <Dropdown.Item id="__none" label={t("consoleChrome.noApprovalSelector")} isDisabled />}
                                {approvalChoices.map((c) => <Dropdown.Item key={c.Value} id={c.Value} label={c.Detail ? `${c.Label || c.Value} · ${c.Detail}` : (c.Label || c.Value)} />)}
                            </Dropdown.Section>
                        </Dropdown.Menu>
                    )}
                </Dropdown.Popover>
            </Dropdown.Root>
            <Dropdown.Root onOpenChange={(isOpen) => { if (isOpen) open(); }}>
                <AriaButton isDisabled={busy} aria-label={reasoningLabel} className={`${chip} text-quaternary`}>
                    <span className="max-w-40 truncate">{effortLabel ? `${reasoningLabel} · ${effortLabel}` : reasoningLabel}</span>
                    <ChevronDown className="size-3" />
                </AriaButton>
                <Dropdown.Popover placement="top start" className="w-60">
                    {error && !sel ? <div className="px-3 py-2 text-xs text-error-primary">{error}</div> : !sel ? <div className="px-3 py-2 text-xs text-quaternary">{t("consoleChrome.loadingChoices")}</div> : (
                        <Dropdown.Menu onAction={(k) => { if (reasoning) void prefer({ [reasoning.ID]: String(k) }); }}>
                            <Dropdown.Section>
                                <Dropdown.SectionHeader className="px-2 py-1 u-meta text-quaternary">{t("consoleChrome.currentOption", { name: reasoningLabel, value: choices.find(c => c.Value === reasoning?.Current)?.Label || reasoning?.Current || t("common.unknown") })}{reasoning && preferenceLabel(reasoning.ID) ? ` · ${t("consoleChrome.preferred", { model: effortLabel || "" })}` : ""}{reasoning && ownPreference(accepted, reasoning.ID) !== undefined ? ` · ${t("consoleChrome.optionReadbackPending")}` : ""}</Dropdown.SectionHeader>
                                {choices.length === 0 && <Dropdown.Item id="__none" label={t("consoleChrome.noReasoningSelector")} isDisabled />}
                                {choices.map((c) => <Dropdown.Item key={c.Value} id={c.Value} label={c.Detail ? `${c.Label || c.Value} · ${c.Detail}` : (c.Label || c.Value)} />)}
                            </Dropdown.Section>
                        </Dropdown.Menu>
                    )}
                </Dropdown.Popover>
            </Dropdown.Root>
        </>
    );
}

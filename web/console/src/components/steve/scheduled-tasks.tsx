import { useEffect, useRef, useState } from "react";
import { Clock } from "@untitledui/icons";
import { Dialog, Modal, ModalOverlay } from "@/components/application/modals/modal";
import { Table, TableCard } from "@/components/application/table/table";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { TextArea } from "@/components/base/textarea/textarea";
import { DialogBody, DialogFooter, DialogHeader, DialogSurface } from "@/components/steve/dialog-surface";
import { Nothing, StateBadge } from "@/components/steve/ui";
import { useI18n } from "@/providers/locale-provider";
import { useFleet } from "@/lib/fleet";
import { relative, when } from "@/lib/format";
import { conversationTransport, conversationURL } from "@/lib/conversation-identity";
import { fetchContext, fetchConversations, readScheduleReceipt, sendSchedule } from "@/lib/api/console";
import { message } from "@/lib/http";
import type { Conversation, ConversationContext, Schedule } from "@/lib/types";

type Action = "cancel" | "confirm" | "retry";
type Draft = { conversation: string; agent: string; expectedProject: string; mode: "every" | "at"; when: string; prompt: string };
type Operation = { id: string; conversation: string; input: string; kind: "create" | Action; expectedProject?: string; draft?: Draft };
type Selection = { job: Schedule; action: Action };
const pendingKey = "steve.schedule.pending";

// Retain the exact submission across reloads and lost HTTP replies. A retry
// reads the existing console receipt, never creates another command identity.
function readPending(): Operation | null {
    const raw = sessionStorage.getItem(pendingKey);
    if (!raw) return null;
    const value = JSON.parse(raw) as Operation;
    if (!value || !value.id || !value.conversation || !value.input || !["create", "cancel", "confirm", "retry"].includes(value.kind)) throw new Error("Invalid retained schedule submission");
    return value;
}
function readInitial() {
    try { return { pending: readPending(), error: "" }; }
    catch (error) { return { pending: null, error: message(error) }; }
}
const editable = (c: Conversation) => conversationTransport(c) === "console" && !c.read_only;

export function Scheduled() {
    const { t: tr, locale } = useI18n();
    const { snap, refresh } = useFleet();
    const [initial] = useState(readInitial);
    const [pending, setPending] = useState(initial.pending);
    const [open, setOpen] = useState(!!initial.pending);
    const [selection, setSelection] = useState<Selection | null>(null);
    const [conversations, setConversations] = useState<Conversation[]>([]);
    const [directoryError, setDirectoryError] = useState("");
    const [loading, setLoading] = useState(true);
    const [revision, setRevision] = useState(0);
    useEffect(() => {
        const controller = new AbortController();
        setLoading(true);
        void fetchConversations(controller.signal).then((result) => {
            if (controller.signal.aborted) return;
            if (!result.enabled) throw new Error(tr("board.consoleUnavailable"));
            setConversations(result.conversations || []); setDirectoryError("");
        }).catch((error) => { if (!controller.signal.aborted) setDirectoryError(message(error)); })
            .finally(() => { if (!controller.signal.aborted) setLoading(false); });
        return () => controller.abort();
    }, [revision, tr]);
    const source = snap.sources.find(s => s.name === "schedules");
    const unavailable = !source?.wired || !!source.error;
    const writable = conversations.filter(editable);
    const targetOf = (job: Schedule) => {
        // The directory is authoritative; never infer a transport from an ID.
        const matches = conversations.filter(c => c.id === job.conversation);
        return matches.length === 1 ? matches[0] : undefined;
    };
    const disabled = loading || !!directoryError || !!initial.error || unavailable || !!pending;
    const manage = (job: Schedule, action: Action) => { setSelection({ job, action }); setOpen(true); };
    return <>
        <TableCard.Root size="sm" className="workbench-table min-w-0">
            <TableCard.Header title={tr("board.scheduled")} badge={`${snap.schedules.length}`} description={tr("board.scheduleHint")}
                contentTrailing={<div className="flex flex-wrap gap-2">
                    <Button size="sm" color="secondary" isDisabled={loading} onClick={() => { setRevision(n => n + 1); refresh(); }}>{tr("workHistory.refresh")}</Button>
                    <Button size="sm" color="primary" isDisabled={disabled || !writable.length} onClick={() => { setSelection(null); setOpen(true); }}>{tr("board.createSchedule")}</Button>
                </div>} />
            {loading && <p role="status" className="p-4 text-sm text-tertiary">{tr("workHistory.loading")}</p>}
            {(directoryError || initial.error || unavailable) && <p role="alert" className="p-4 text-sm text-error-primary">{directoryError || initial.error || source?.error || tr("board.schedulesUnavailable")}</p>}
            {pending && !open && <div role="status" className="flex flex-wrap items-center gap-3 p-4 text-sm text-secondary">{tr("board.scheduleUncertain")}<Button size="sm" color="secondary" onClick={() => setOpen(true)}>{tr("board.retryOriginal")}</Button></div>}
            {!loading && !directoryError && !writable.length && <p className="p-4 text-sm text-tertiary">{tr("board.noScheduleConversation")} <a className="underline" href="#/console">{tr("board.openConversation")}</a></p>}
            {snap.schedules.length === 0 ? !unavailable && <Nothing icon={Clock} title={tr("board.noSchedules")}>{tr("board.createScheduleHint")}</Nothing> : (
                <Table aria-label={tr("board.scheduled")} size="sm">
                    <Table.Header>
                        <Table.Head id="when" label={tr("board.when")} isRowHeader />
                        <Table.Head id="next" label={tr("board.next")} />
                        <Table.Head id="what" label={tr("board.what")} />
                        <Table.Head id="who" label={tr("board.where")} />
                        <Table.Head id="last" label={tr("board.last")} />
                        <Table.Head id="actions" label={tr("board.scheduleActions")} />
                    </Table.Header>
                    <Table.Body items={snap.schedules}>{job => {
                        const target = targetOf(job), canManage = target && editable(target);
                        return <Table.Row id={job.id}>
                            <Table.Cell><span className="text-primary">#{job.id} · {job.spec}</span></Table.Cell>
                            <Table.Cell><span className="text-tertiary">{when(job.next_at, locale)}</span>{job.state && <div className="mt-1"><StateBadge state={job.state} /></div>}</Table.Cell>
                            <Table.Cell><span className="line-clamp-3 max-w-md whitespace-pre-wrap break-words">{job.prompt}</span>{job.error && <span className="mt-1 block max-w-md break-words text-xs text-error-primary">{job.error}</span>}</Table.Cell>
                            <Table.Cell><div className="flex max-w-xs flex-col gap-1 text-xs text-tertiary"><span className="break-words">{target?.title || job.conversation} · {job.agent || tr("common.default")}</span>
                                {target && <a className="rounded text-brand-secondary underline outline-focus-ring focus-visible:outline-2" href={`#${conversationURL(job.conversation, conversationTransport(target))}`}>{tr("board.openConversation")}</a>}
                            </div></Table.Cell>
                            <Table.Cell><span className="text-xs text-tertiary">{job.last_at ? tr("board.lastRun", { time: relative(job.last_at, locale), count: job.runs }) : tr("board.neverRun")}</span></Table.Cell>
                            <Table.Cell>{canManage ? <div className="flex flex-wrap gap-2">
                                {job.state === "unknown" ? <>
                                    <Button size="sm" color="link-gray" isDisabled={disabled} onClick={() => manage(job, "confirm")}>{tr("board.confirmSchedule")}</Button>
                                    <Button size="sm" color="link-gray" isDisabled={disabled} onClick={() => manage(job, "retry")}>{tr("board.retrySchedule")}</Button>
                                </> : <Button size="sm" color="link-gray" isDisabled={disabled || job.state === "dispatching"} onClick={() => manage(job, "cancel")}>{tr("board.cancelSchedule")}</Button>}
                            </div> : <span className="text-xs text-tertiary">{tr("board.scheduleOriginalChannel")}</span>}</Table.Cell>
                        </Table.Row>;
                    }}</Table.Body>
                </Table>
            )}
        </TableCard.Root>
        {open && <ScheduleDialog conversations={writable} restored={pending} selection={selection} onRetain={setPending} onChanged={refresh} onClose={() => { setOpen(false); setSelection(null); }} />}
    </>;
}

function ScheduleDialog({ conversations, restored, selection, onRetain, onChanged, onClose }: {
    conversations: Conversation[]; restored: Operation | null; selection: Selection | null; onRetain: (op: Operation | null) => void; onChanged: () => void; onClose: () => void;
}) {
    const { t: tr } = useI18n();
    const [draft, setDraft] = useState<Draft>(() => restored?.draft || { conversation: "", agent: "", expectedProject: "", mode: "every", when: "", prompt: "" });
    const [operation, setOperation] = useState(restored);
    const [context, setContext] = useState<ConversationContext | null>(null);
    const [contextError, setContextError] = useState("");
    const [busy, setBusy] = useState(false);
    const working = useRef(false);
    const [receipt, setReceipt] = useState("");
    const [error, setError] = useState("");
    const kind = operation?.kind || selection?.action || "create";
    const title = tr(kind === "create" ? "board.createSchedule" : kind === "cancel" ? "board.cancelSchedule" : kind === "confirm" ? "board.confirmSchedule" : "board.retrySchedule");
    const locked = !!operation || busy;
    useEffect(() => {
        if (!draft.conversation || operation || kind !== "create") return;
        const controller = new AbortController();
        setContext(null); setContextError("");
        void fetchContext(draft.conversation, controller.signal).then(result => {
            if (controller.signal.aborted) return;
            if (!result.enabled || !result.context) throw new Error(tr("board.consoleUnavailable"));
            setContext(result.context);
            setDraft(old => ({ ...old, agent: result.context?.agent?.id || "", expectedProject: result.context?.project?.id || "" }));
        }).catch(cause => { if (!controller.signal.aborted) setContextError(message(cause)); });
        return () => controller.abort();
    }, [draft.conversation, operation, kind, tr]);
    const change = (patch: Partial<Draft>) => setDraft(old => ({ ...old, ...patch }));
    const target = conversations.find(c => c.id === draft.conversation);
    const valid = !!target && !!draft.agent && !!draft.expectedProject && !!context && !contextError && !!draft.when.trim() && !!draft.prompt.trim();
    const command = `@${draft.agent} /${draft.mode} ${draft.when.trim()} ${draft.prompt.trim()}`;
    async function submit() {
        if (working.current || receipt || (!operation && kind === "create" && !valid)) return;
        working.current = true; setBusy(true); setError("");
        try {
            const op = operation || (selection ? { id: crypto.randomUUID(), kind: selection.action, conversation: selection.job.conversation, input: `/schedules ${selection.action} ${selection.job.id}` }
                : { id: crypto.randomUUID(), kind: "create" as const, conversation: draft.conversation, input: command, expectedProject: draft.expectedProject, draft: { ...draft } });
            // A retained operation is not authority to recreate a deleted
            // conversation or write to a target whose channel has changed.
            const directory = await fetchConversations();
            const targets = directory.conversations?.filter(c => c.id === op.conversation) || [];
            if (!directory.enabled || targets.length !== 1 || !editable(targets[0])) throw new Error(tr("board.scheduleTargetUnavailable"));
            let reply = null;
            if (op.kind === "create") {
                const current = await fetchContext(op.conversation);
                if (!op.expectedProject || !current.enabled || current.context?.project?.id !== op.expectedProject) {
                    // Only read an already accepted operation; never POST into
                    // a different project, even under the old command ID.
                    if (operation) reply = await readScheduleReceipt(op.conversation, op.id);
                    if (!reply) throw new Error(tr("board.scheduleProjectChanged"));
                }
            }
            sessionStorage.setItem(pendingKey, JSON.stringify(op));
            setOperation(op); onRetain(op);
            reply ||= await sendSchedule(op.conversation, op.input, op.id, op.expectedProject);
            if (!reply || reply.conversation !== op.conversation || !reply.id || !reply.exchange_id || typeof reply.text !== "string") throw new Error(tr("board.invalidScheduleReceipt"));
            // The command can refuse in a successful HTTP reply. Display its
            // actual text with neutral styling, not a guessed success toast.
            setReceipt(reply.error || reply.text);
            sessionStorage.removeItem(pendingKey); onRetain(null); onChanged();
        } catch (cause) { setError(message(cause)); }
        finally { working.current = false; setBusy(false); }
    }
    return <ModalOverlay isOpen isDismissable={!busy} onOpenChange={v => { if (!v && !busy) onClose(); }}>
        <Modal className="max-w-2xl"><Dialog aria-label={title}>
            <DialogSurface><DialogBody>
                <DialogHeader title={title} description={kind === "create" ? tr("board.createScheduleHint") : tr(kind === "cancel" ? "board.cancelScheduleHint" : kind === "confirm" ? "board.confirmScheduleHint" : "board.retryScheduleHint", { id: selection?.job.id || "" })} />
                {kind === "create" ? <>
                    <Select label={tr("board.targetConversation")} placeholder={tr("board.chooseConversation")} selectedKey={draft.conversation || null} isDisabled={locked}
                        onSelectionChange={key => { setContext(null); change({ conversation: String(key || ""), agent: "", expectedProject: "" }); }} items={conversations.map(c => ({ id: c.id, label: c.title || c.id }))}>
                        {item => <Select.Item {...item} />}
                    </Select>
                    <p className="break-words text-xs text-tertiary">{tr("board.scheduleTarget", { project: draft.expectedProject || "—", agent: draft.agent || "—" })}</p>
                    {contextError && <p role="alert" className="text-sm text-error-primary">{contextError}</p>}
                    <Select label={tr("board.scheduleType")} selectedKey={draft.mode} isDisabled={locked} onSelectionChange={key => change({ mode: key === "at" ? "at" : "every" })}
                        items={[{ id: "every", label: tr("board.recurring") }, { id: "at", label: tr("board.once") }]}>{item => <Select.Item {...item} />}</Select>
                    <Input label={tr("board.scheduleWhen")} value={draft.when} isDisabled={locked} onChange={value => change({ when: value })} placeholder={draft.mode === "every" ? "30m / 09:00 / mon 09:00" : "30m / tomorrow 09:00 / 2027-01-01 09:00"} hint={tr("board.scheduleTimezone")} />
                    <TextArea label={tr("board.schedulePrompt")} value={draft.prompt} isDisabled={locked} onChange={value => change({ prompt: value })} rows={4} placeholder={tr("board.schedulePromptPlaceholder")} />
                </> : <p className="whitespace-pre-wrap break-words text-sm text-secondary">{selection?.job.prompt}</p>}
                <details className="min-w-0 text-xs text-tertiary"><summary className="cursor-pointer">{tr("board.schedulePreview")}</summary><pre className="mt-2 max-h-32 overflow-auto whitespace-pre-wrap break-words">{operation?.input || (kind === "create" ? command : `/schedules ${kind} ${selection?.job.id}`)}</pre></details>
                {receipt && <div role="status" className="whitespace-pre-wrap break-words text-sm text-secondary">{receipt}</div>}
                {error && <div role="alert" className="whitespace-pre-wrap break-words text-sm text-error-primary">{error}</div>}
                {operation && !receipt && !busy && <p className="text-sm text-tertiary">{tr("board.scheduleUncertain")}</p>}
                <DialogFooter>
                    <Button size="sm" color="secondary" isDisabled={busy} onClick={onClose}>{tr(receipt || operation ? "common.close" : "common.cancel")}</Button>
                    {receipt ? kind === "create" && <Button size="sm" color="secondary" onClick={() => { setOperation(null); setReceipt(""); setError(""); change({ conversation: "", agent: "", expectedProject: "", prompt: "", when: "" }); }}>{tr("board.createAnother")}</Button>
                        : <Button size="sm" color={kind === "cancel" ? "primary-destructive" : "primary"} isDisabled={busy || (!operation && kind === "create" && !valid)} isLoading={busy} onClick={() => void submit()}>
                            {tr(operation ? "board.retryOriginal" : kind === "create" ? "board.create" : kind === "cancel" ? "board.confirmCancelSchedule" : kind === "confirm" ? "board.confirmSchedule" : "board.retrySchedule")}
                        </Button>}
                </DialogFooter>
            </DialogBody></DialogSurface>
        </Dialog></Modal>
    </ModalOverlay>;
}

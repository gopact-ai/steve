import { useI18n } from "@/providers/locale-provider";
import { Inbox01 } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { when } from "@/lib/format";
import { useFleet, useIntent } from "@/lib/fleet";
import { nodeLabelIn } from "@/lib/node-name";
import { label, labelsFor } from "@/lib/labels";
import { Md } from "@/components/steve/markdown";
import { PageBody, PageHeader, Panel } from "@/components/steve/page";
import { Nothing } from "@/components/steve/ui";
import { unavailableSource } from "@/lib/source-health";
import { ConflictsPanel } from "@/components/steve/conflicts";
import { useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router";
import { useAssistantPage } from "@/hooks/use-assistant-page";
import { EventInboxPanels } from "@/components/steve/event-inbox";
import { AssistantFeedback, AssistantPager } from "@/components/steve/assistant-projection";
import { Drawer } from "@/components/steve/drawer";
import { KeyValue } from "@/components/steve/page";
import { CodeBlock } from "@/components/steve/code-block";
import { approveClient, fetchBusinessEvent, resolveEvent, retryDelivery, reprocessEvent } from "@/lib/api/assistant";
import { message } from "@/lib/http";
import type { BusinessEvent, ClientInvocation, EventInbox } from "@/lib/assistant-types";

// InboxPage answers "what exactly do I have to decide right now". Only
// requests with actions and quarantined writers appear. Writer confirmation
// requires physical process evidence, so it intentionally has no action button.
// What an agent asks is written the way it writes everywhere else, so the
// command it wants to run arrives as a code block rather than as a line of
// backticks the reader has to decode.
export function InboxPage() {
    const { t: tr, locale } = useI18n();
    const snap = useFleet((fleet) => fleet.snap);
    const { act } = useIntent();
    const unavailable = unavailableSource(snap.sources, "ledger-attention");
    // Conflicts block work until somebody settles them, which is what this
    // page is for. They are kept above the requests because nothing else
    // here stops a result from reaching the project.
    const conflictSource = unavailableSource(snap.sources, "ledger-landings");
    const conflicts = snap.conflicts || [];
    const groups = ["writer", "disclosure", "effect", "question", "pairing"].map((type) => ({ type, items: snap.inbox.filter((r) => r.type === type && (r.resolvable || r.type === "writer")) })).filter((g) => g.items.length);
    const servicesEnabled = !!snap.hub.services?.includes("events.v1");
    const events = useAssistantPage<EventInbox>("/console/inbox", 5000, "eventsCursor", servicesEnabled);
    const calls = useAssistantPage<ClientInvocation>("/console/client-invocations?attention=1", 5000, "callsCursor", servicesEnabled);
    const [params, setParams] = useSearchParams(); const eventID = params.get("event");
    const [event, setEvent] = useState<BusinessEvent | null>(null); const [eventError, setEventError] = useState("");
    const [busy, setBusy] = useState(""); const [actionError, setActionError] = useState("");
    const actionFocus = useRef<HTMLElement | null>(null);
    useEffect(() => {
        if (busy) return;
        const focused = actionFocus.current; actionFocus.current = null;
        // Disabling the active button while saving can move focus outside
        // the modal. Restore it after rendering so Escape and Tab still work.
        if (focused?.isConnected && document.activeElement === document.body) focused.focus({ preventScroll: true });
    }, [busy]);
    useEffect(() => {
        setEvent(null); setEventError(""); if (!eventID) return;
        const controller = new AbortController();
        void fetchBusinessEvent(eventID, controller.signal).then((value) => { if (!controller.signal.aborted) setEvent(value); }).catch((cause) => { if (!controller.signal.aborted) setEventError(message(cause)); });
        return () => controller.abort();
    }, [eventID]);
    const action = async (id: string, run: () => Promise<unknown>) => { actionFocus.current = document.activeElement instanceof HTMLElement ? document.activeElement : null; setBusy(id); setActionError(""); try { await run(); events.refresh(); calls.refresh(); } catch (cause) { setActionError(message(cause)); } finally { setBusy(""); } };
    const closeEvent = () => setParams((before) => { const next = new URLSearchParams(before); next.delete("event"); return next; });
    return (
        <div className="workbench-page flex min-w-0 flex-col">
            <PageHeader title={tr("inbox.title")} description={tr("inbox.description")} />
            <PageBody>
            {unavailable && <div role="status" className="rounded-lg bg-secondary p-4 text-sm text-secondary">{tr("inbox.incomplete")}{unavailable.error}</div>}
            {(conflicts.length > 0 || conflictSource) && <ConflictsPanel conflicts={conflicts} nodes={snap.nodes} incomplete={conflictSource?.error} />}
            {!unavailable && groups.length === 0 && conflicts.length === 0 && !events.loading && !calls.loading && !events.error && !calls.error && !events.page?.items.length && !calls.page?.items.length && <Panel padding="flush"><Nothing icon={Inbox01} title={tr("inbox.empty")}>{tr("inbox.emptyHint")}</Nothing></Panel>}
            {groups.map((g) => (
                <Panel key={g.type} padding="flush" title={label(labelsFor(locale).requestType, g.type)}
                    badge={<Badge type="pill-color" size="sm" color="warning">{g.items.length}</Badge>}>
                    <ul className="divide-y divide-secondary">
                        {g.items.map((r) => (
                            <li key={r.id} className="flex min-w-0 flex-col items-start gap-3 px-4 py-4 sm:flex-row">
                                <div className="min-w-0 flex-1">
                                    <Md text={r.summary} className="max-h-72 overflow-y-auto text-primary" />
                                    {r.type === "writer" && <div className="mt-2 flex flex-col gap-1 text-xs text-secondary">
                                        <p>{tr("inbox.machine")}<span className="font-mono">{r.node ? nodeLabelIn(snap.nodes, r.node) : "—"}</span></p>
                                        <p className="break-all">{tr("inbox.directory")}<span className="font-mono">{r.workspace || "—"}</span></p>
                                        <p className="break-all">{tr("inbox.execution")}<span className="font-mono">{r.attempt_id || r.source}</span></p>
                                        <p className="mt-1 leading-5 text-tertiary">{tr("inbox.writerHint")}<a className="underline" href="https://github.com/gopact-ai/steve/blob/master/docs/operations.md#隔离执行与复制操作" target="_blank" rel="noreferrer">{tr("inbox.recoveryGuide")}</a>{tr("inbox.writerSteps")}</p>
                                    </div>}
                                    <div className="mt-1 break-all text-xs text-tertiary">{when(r.created_at, locale)}{r.project_id ? tr("inbox.projectSuffix", { project: r.project_id }) : ""}{r.task_id ? tr("inbox.taskSuffix", { task: r.task_id }) : ""} · <span className="font-mono">{r.id}</span></div>
                                </div>
                                <div className="flex shrink-0 flex-wrap gap-2">
                                    {r.type === "question" && r.conversation && <a className="rounded-md px-3 py-2 text-sm font-semibold text-brand-secondary outline-focus-ring focus-visible:outline-2" href={`#/console?conversation=${encodeURIComponent(r.conversation)}`}>{tr("inbox.answerInConversation")}</a>}
                                    {r.type !== "writer" && r.choices.map((c) => (
                                        <Button key={c.command} size="sm" color={c.danger ? "secondary-destructive" : "secondary"} onClick={() => { if (!c.danger || window.confirm(`${c.label}？\n\n${c.command}`)) act(c.command); }}>{c.label}</Button>
                                    ))}
                                </div>
                            </li>
                        ))}
                    </ul>
                </Panel>
            ))}
            {actionError ? <p role="alert" className="break-words text-sm text-error-primary">{actionError}</p> : null}
            {servicesEnabled ? <EventInboxPanels eventsReady={!!events.page && !events.error} callsReady={!!calls.page && !calls.error} entries={events.page?.items || []} calls={calls.page?.items || []} busy={busy} disabled={!!events.error || !!calls.error}
                eventsFeedback={<AssistantFeedback error={events.error} loading={events.loading} onRefresh={events.refresh} />} callsFeedback={<AssistantFeedback error={calls.error} loading={calls.loading} onRefresh={calls.refresh} />}
                eventsFooter={<AssistantPager next={events.page?.next || ""} cursor={events.cursor} onPage={events.setCursor} disabled={!!busy || !!events.error} />} callsFooter={<AssistantPager next={calls.page?.next || ""} cursor={calls.cursor} onPage={calls.setCursor} disabled={!!busy || !!calls.error} />}
                onInspect={(entry) => setParams((before) => { const next = new URLSearchParams(before); next.set("event", entry.event_id); return next; })}
                onResolve={(entry) => void action(entry.id, () => resolveEvent(entry.id))}
                onRetry={(entry) => { if (entry.delivery_id && window.confirm(tr("assistant.retryConfirm"))) void action(entry.id, () => retryDelivery(entry.delivery_id!)); }}
                onApproval={(call, allow) => void action(call.id, () => approveClient(call.id, allow))} /> : null}
            <p className="text-xs text-tertiary">{tr("inbox.otherChannels")}</p>
            </PageBody>
            {eventID ? <Drawer title={tr("assistant.eventDetail")} subtitle={eventID} onClose={closeEvent}>
                {eventError ? <p role="alert" className="break-words text-sm text-error-primary">{eventError}</p> : !event ? <p role="status" className="text-sm text-tertiary">{tr("assistant.loading")}</p> : <><KeyValue rows={[{ k: tr("assistant.eventType"), v: event.type }, { k: tr("assistant.origin"), v: event.source }, { k: tr("assistant.at"), v: when(event.at, locale) }]} /><CodeBlock code={JSON.stringify(event.payload, null, 2)} lang="json" label={tr("assistant.payload")} maxHeight={600} /><Button size="sm" isDisabled={!!busy} isLoading={busy === eventID} onClick={() => void action(eventID, () => reprocessEvent(eventID))}>{tr("assistant.reprocess")}</Button><a className="text-sm text-brand-secondary underline outline-focus-ring focus-visible:outline-2" href="#/connections?section=subscriptions">{tr("assistant.createRule")}</a></>}
            </Drawer> : null}
        </div>
    );
}

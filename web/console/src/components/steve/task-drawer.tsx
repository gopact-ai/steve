import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router";
import { useI18n } from "@/providers/locale-provider";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { send } from "@/lib/api/console";
import { short, when } from "@/lib/format";
import { useFleet } from "@/lib/fleet";
import { nodeLabelIn } from "@/lib/node-name";
import { fmtSeconds, fmtTokens, label, spend, labelsFor } from "@/lib/labels";
import type { Plan, Task, TaskDetail, WorkPage } from "@/lib/types";
import { consoleTaskConversation } from "@/lib/task-transport";
import { TaskDeliveries } from "./task-deliveries";
import { CallGraph } from "./call-graph";
import { Drawer, DrawerSection } from "./drawer";
import { TaskMetaMenu, TaskTitleEditor, useTaskMeta } from "./task-meta-menu";
import { TaskCloseDialog, useTaskClose } from "./task-close";
import { StateBadge, taskState } from "./ui";
import { fetchTask, fetchTaskAccounting, fetchTasks, fetchPlans } from "@/lib/api/work";
import { message } from "@/lib/http";
import { useWorkPage } from "@/hooks/use-work-page";
import { useResourceRead } from "@/hooks/use-resource-read";
import { ArtifactsTab } from "./work-tabs";

// TaskDrawer is one task's detail wherever a task is clicked — the
// board, the console's relations tab: the result first, then the plan,
// the call graph, the attempts and what they cost.
// Drawer is a task's detail: the result first, then the tree, the plan,
// the attempts and what they cost.
type TaskDrawerProps = { t: Pick<Task, "id">; tasks: Task[]; plan?: Plan; onClose: () => void; width?: number };

export function TaskDrawer(props: TaskDrawerProps) {
    return <TaskDrawerReader key={props.t.id} {...props} />;
}

function TaskDrawerReader(props: TaskDrawerProps) {
    const { t: tr } = useI18n();
    const { snap, refresh } = useFleet();
    const [id, setID] = useState(props.t.id);
    const [revision, setRevision] = useState(0);
    const [state, setState] = useState<{ key: string; value?: TaskDetail; error?: string }>({ key: "" });
    const [records, setRecords] = useState<{ key: string; rows: Map<string, { task: Task; order: number }> }>({ key: "", rows: new Map() });
    const requestOrder = useRef(0);
    const key = `${id}:${revision}`;
    const remember = (items: Task[], order: number, page = false) => setRecords((previous) => {
        const sameScope = previous.key === key;
        const rows = new Map(sameScope ? previous.rows : []);
        for (const task of items) {
            // Background revalidation cannot accumulate every child ever in
            // the first page. Only navigation grows the loaded membership.
            if (sameScope && !page && !rows.has(task.id)) continue;
            if ((rows.get(task.id)?.order ?? -1) < order) rows.set(task.id, { task, order });
        }
        return { key, rows };
    });
    const read = useResourceRead(key, async (signal) => {
        const order = ++requestOrder.current;
        return { value: await fetchTask(id, signal), order };
    }, ({ value, order }) => {
        setState({ key, value });
        remember([value.task, ...value.children.items], order);
    }, (error) => setState((previous) => ({ key, value: previous.key === key ? previous.value : undefined, error: message(error) })));
    // A summary change is only an invalidation signal. Neither its arrival
    // time nor updated_at establishes authority over a successful owner read.
    const invalidation = useMemo(() => JSON.stringify([taskSubtreeSignal(id, props.tasks), snap.plans.filter((plan) => plan.task_id === id)]), [id, props.tasks, snap.plans]);
    useEffect(() => { void read(); }, [key, invalidation, read]);
    // Acknowledged actions invalidate their owner independently. A failed
    // summary refresh must not prevent the available detail from updating.
    const changed = () => { refresh(); return read(); };
    const readChildren = async (cursor: string, signal: AbortSignal) => {
        const order = ++requestOrder.current;
        const page = await fetchTasks({ scope: "children", scope_id: id }, cursor, signal);
        if (!signal.aborted) remember(page.items, order, true);
        return page;
    };
    const retry = () => setRevision((n) => n + 1);
    if (state.key !== key || !state.value) return <Drawer width={props.width} title={`#${id}`} onClose={props.onClose}>
        {state.key === key && state.error ? <div role="alert" className="text-sm text-error-primary">{state.error}<Button size="sm" color="secondary" onClick={retry}>{tr("common.retry")}</Button></div> : <p role="status" className="text-sm text-tertiary">{tr("workHistory.loading")}</p>}
    </Drawer>;
    return <TaskDrawerContent key={key} {...props} detail={state.value} detailError={state.error} ownerRows={records.key === key ? records.rows : new Map()} readChildren={readChildren} changed={changed} reread={read} reload={retry} onPick={(task) => setID(task.id)} />;
}

// Only the bounded live workset is examined, never historical pages. Stable
// content avoids a read on each /state tick; cycles cannot hang invalidation.
function taskSubtreeSignal(id: string, tasks: Task[]) {
    const children = new Map<string, Task[]>();
    for (const task of tasks) {
        const parent = task.parent || "";
        const group = children.get(parent) || [];
        group.push(task);
        children.set(parent, group);
    }
    const ids = new Set([id]);
    for (const parent of ids) for (const child of children.get(parent) || []) ids.add(child.id);
    return JSON.stringify(tasks.filter((task) => ids.has(task.id)).sort((a, b) => a.id.localeCompare(b.id)));
}

function TaskDrawerContent({ detail, detailError, ownerRows, readChildren, changed, reread, tasks: baseTasks, onClose, width, reload, onPick }: TaskDrawerProps & {
    detail: TaskDetail; detailError?: string; ownerRows: Map<string, { task: Task; order: number }>;
    readChildren: (cursor: string, signal: AbortSignal) => Promise<WorkPage<Task>>;
    changed: () => Promise<void>; reread: () => Promise<void>; reload: () => void; onPick: (task: Task) => void;
}) {
    const { t: tr, locale } = useI18n();
    const { snap } = useFleet();
    const navigate = useNavigate();
    const [pending, setPending] = useState(false);
    const [error, setError] = useState("");
    const [result, setResult] = useState("");
    const acting = useRef(false);
    const t = detail.task;
    const plan = detail.plan;
    const childrenPage = useWorkPage(`children:${t.id}`, readChildren, true, detail.children);
    const accounting = useWorkPage(`accounting:${t.id}`, (cursor, signal) => fetchTaskAccounting(t.id, cursor, signal), true, detail.accounting);
    const childrenChanged = childrenPage.total !== undefined && childrenPage.total !== t.children_count;
    const accountingChanged = accounting.total !== undefined && accounting.total !== t.attempt_count;
    const [filesOpen, setFilesOpen] = useState(false);
    const [plansOpen, setPlansOpen] = useState(false);
    // Page membership/cursors stay pinned until explicit refresh. Successful
    // owner reads update displayed rows without trusting /state, resetting
    // history, or letting an older in-flight response replace a later read.
    const tasks = [...new Map([...baseTasks.filter((task) => task.parent !== t.id), ...childrenPage.items, t]
        .map((task) => [task.id, ownerRows.get(task.id)?.task || task])).values()];
    const plans = [...snap.plans.filter((p) => p.task_id !== t.id), ...(plan ? [plan] : [])];
    const meta = useTaskMeta(t);
    const closing = useTaskClose(t);
    const landings = snap.landings.filter((l) => l.project === t.project_id).slice(0, 5);
    const holds = ["running", "blocked", "review", "paused", "draft", "failed"].includes(t.lifecycle);
    const conversation = consoleTaskConversation(t);
    const consoleTask = conversation !== null;
    // A failure nobody has decided about yet: the only state where retrying
    // and settling are both on offer.
    const failedOpen = t.lifecycle === "failed" && !t.settlement;
    const completionRoot = !t.parent && !t.origin && !t.plan_id && !plan && ["running", "review"].includes(t.lifecycle);
    const canComplete = completionRoot && t.can_complete === true && t.execution === "idle" && t.attention === 0 && !t.pending_results && !t.uncertain_results;
    async function act(command: string) {
        if (acting.current || conversation === null) return;
        acting.current = true;
        setPending(true);
        setError("");
        setResult("");
        try {
            const reply = await send(conversation, command);
            if (reply.error) setError(reply.error);
            else setResult(reply.text);
            await changed();
        }
        catch (e) { setError(String(e).replace(/^Error: /, "")); await changed(); }
        finally { acting.current = false; setPending(false); }
    }
    return (
        <Drawer width={width} title={`#${t.id} ${t.title || t.goal}`}
            titleEditor={meta.renaming ? <TaskTitleEditor t={t} pending={meta.pending} onDone={async (title) => { const saved = await meta.finishTitle(title); if (saved && title !== null) reload(); return saved; }} /> : undefined}
            badges={<>
                <StateBadge state={taskState(t)} /><span className="text-xs text-tertiary">{label(labelsFor(locale).status, t.lane)}</span>
                {t.priority === "high" && <Badge type="pill-color" size="sm" color="warning">{tr("tasks.high")}</Badge>}
                {t.archived_at && <Badge type="pill-color" size="sm" color="gray">{tr("tasks.archived")}</Badge>}
                {t.settlement && <Badge type="pill-color" size="sm" color="gray">{tr(t.settlement === "handled" ? "tasks.settledHandled" : "tasks.settledIgnored")}</Badge>}
            </>}
            actions={<><TaskMetaMenu t={t} pending={meta.pending || meta.renaming} onRename={meta.rename} onPatch={(patch) => void meta.save(patch).then((saved) => { if (saved) reload(); })} onEnd={closing.closable ? closing.ask : undefined} />{closing.asking && <TaskCloseDialog t={t} onClose={() => { closing.dismiss(); void reread(); }} />}</>}
            subtitle={<>
                    {meta.error && <div role="alert" className="mt-1 text-xs text-error-primary">{meta.error}</div>}
                    {error && <div role="alert" className="mt-1 text-xs text-error-primary">{error}</div>}
                    {result && <div role="status" className="mt-1 text-xs text-secondary">{result}</div>}
                    <div className="mt-1 text-xs text-tertiary">{t.member} @ {nodeLabelIn(snap.nodes, t.node || snap.hub.node)} {tr("tasks.projectPrefix")}{t.project_id || "—"} · {label(labelsFor(locale).origin, t.origin || "chat")} {tr("tasks.conversationPrefix")}{t.channel}</div></>} onClose={onClose}>
                <DrawerSection title={tr("tasks.result")}>
                    <Button size="sm" color="link-gray" className="mb-2" onClick={reload}>{tr("workHistory.refresh")}</Button>
                    {detailError && <div role="alert" className="mb-2 text-xs text-error-primary">{detailError}<Button size="sm" color="link-gray" onClick={() => void reread()}>{tr("common.retry")}</Button></div>}
                    <div className="flex flex-col gap-1 text-sm">
                        <Row k={tr("tasks.execution")} v={t.execution === "running" ? tr("tasks.running") : t.execution === "unknown" ? tr("tasks.unknown") : tr("tasks.idle")} />
                        <Row k={tr("tasks.attention")} v={t.attention || t.uncertain_results ? tr("tasks.attentionCount", { count: t.attention + (t.uncertain_results || 0) }) : tr("tasks.none")} />
                        {t.settlement && <Row k={tr("tasks.settled")} v={tr(t.settlement === "handled" ? "tasks.settledHandled" : "tasks.settledIgnored")} />}
                        <Row k={tr("tasks.budget")} v={tr("tasks.budgetSummary", { turns: t.max_turns ? `${t.turns}/${t.max_turns}` : t.turns, elapsed: t.elapsed || "0s", limit: t.max_elapsed && t.max_elapsed !== "0s" ? ` / ${t.max_elapsed}` : "" })} />
                        <Row k={tr("tasks.usage")} v={`${spend(t.tokens, locale)} · ${fmtSeconds(t.seconds, locale)}`} />
                        {landings.length > 0 && <Row k={tr("tasks.recentMerges")} v={landings.map((l) => `${label(labelsFor(locale).taskState, l.state) === l.state ? l.state : l.state} ${short(l.artifact)} ${when(l.at, locale)}`).join(" · ")} />}
                    </div>
                    <div className="mt-3 flex flex-wrap gap-2">
                        {canComplete && <Button size="sm" color="secondary" isLoading={pending} showTextWhileLoading isDisabled={pending || !consoleTask} onClick={() => void act(`/tasks complete ${t.id}`)}>{tr("tasks.complete")}</Button>}
                        {holds && !["paused", "failed"].includes(t.lifecycle) && <Button size="sm" color="secondary" isDisabled={pending || !consoleTask} onClick={() => void act(`/tasks pause ${t.id}`)}>{tr("tasks.pause")}</Button>}
                        {t.lifecycle === "paused" && <Button size="sm" color="secondary" isDisabled={pending || !consoleTask} onClick={() => void act(`/tasks resume ${t.id}`)}>{tr("tasks.resume")}</Button>}
                        {failedOpen && <Button size="sm" color="secondary" isDisabled={pending || !consoleTask} onClick={() => void act(`/tasks resume ${t.id}`)}>{tr("common.retry")}</Button>}
                        {/* A failed task is often already dealt with, by hand or by
                            deciding it does not matter. Saying so leaves the failure
                            on the record; cancelling would call the work off. */}
                        {failedOpen && <Button size="sm" color="secondary" isDisabled={pending || !consoleTask} onClick={() => void act(`/tasks handled ${t.id}`)}>{tr("tasks.markHandled")}</Button>}
                        {failedOpen && <Button size="sm" color="secondary" isDisabled={pending || !consoleTask} onClick={() => void act(`/tasks ignore ${t.id}`)}>{tr("tasks.markIgnored")}</Button>}
                        {t.settlement && <Button size="sm" color="secondary" isDisabled={pending || !consoleTask} onClick={() => void act(`/tasks reopen ${t.id}`)}>{tr("tasks.reopen")}</Button>}
                        {holds && !t.settlement && <Button size="sm" color="secondary-destructive" isDisabled={pending || !consoleTask} onClick={() => void act(`/tasks cancel ${t.id}`)}>{tr("common.cancel")}</Button>}
                        <Button size="sm" color="link-gray" isDisabled={!consoleTask} onClick={() => { if (conversation === null) return; onClose(); navigate(`/console?conversation=${encodeURIComponent(conversation)}`); }}>{tr("tasks.viewInWorkbench")}</Button>
                    </div>
                    {failedOpen && <p className="mt-2 text-xs text-tertiary">{tr("tasks.settleHint")}</p>}
                    {completionRoot && <p className="mt-2 text-xs text-tertiary">{tr(canComplete ? "tasks.completeHint" : t.attention ? "tasks.completeAttention" : t.execution !== "idle" ? "tasks.completeBusy" : "tasks.completePending")}</p>}
                    {!consoleTask && <p className="mt-2 text-xs text-tertiary">{tr("tasks.channelHint")}</p>}
                </DrawerSection>
                <TaskDeliveries task={t} tasks={tasks} />
                {plan && (
                    <DrawerSection title={<>{tr("tasks.planRevision", { plan: plan.id, revision: plan.rev, by: plan.by })}</>}>
                        {plan.because && <p className="mb-2 line-clamp-3 text-xs text-tertiary" title={plan.because}>{tr("tasks.revisionReason")}{plan.because}</p>}
                        <ol className="flex flex-col divide-y divide-secondary rounded-lg ring-1 ring-secondary">
                            {plan.steps.map((s) => (
                                <li key={s.id} className="flex flex-col gap-1 px-3 py-2">
                                    <div className="flex items-center gap-2"><StateBadge state={s.state} /><span className="font-medium text-primary">{s.id}</span><span className="text-xs text-tertiary">{s.agent || "—"}{s.node ? ` @ ${nodeLabelIn(snap.nodes, s.node)}` : ""}</span>{s.verify && <span className="ml-auto u-meta text-quaternary">{tr("tasks.verification")}{s.verify}</span>}</div>
                                    <div className="line-clamp-3 text-xs text-secondary">{s.goal}</div>
                                    {s.error && <div className="text-xs text-error-primary">{s.error}</div>}
                                    {s.usage && <div className="u-meta text-quaternary">{s.usage.model} · {fmtTokens(s.usage.tokens.total, locale)} tok · {fmtSeconds(s.usage.seconds, locale)}</div>}
                                </li>
                            ))}
                        </ol>
                    </DrawerSection>
                )}
                {(t.children_count > 0 || childrenPage.items.length > 0 || plan) && (
                    <DrawerSection title={tr("tasks.callGraph")}>
                        <CallGraph roots={[t]} tasks={tasks} plans={plans} onSelect={onPick} />
                        <p className="mt-2 text-xs text-tertiary">{tr("workHistory.loaded", { count: childrenPage.items.length, total: childrenPage.total ?? tr("common.unknown") })}</p>
                        {childrenChanged && <div role="alert" className="mt-2 text-xs text-error-primary">{tr("workHistory.stale")}<Button size="sm" color="link-gray" onClick={reload}>{tr("workHistory.refresh")}</Button></div>}
                        {childrenPage.error && <div role="alert" className="mt-2 text-xs text-error-primary">{childrenPage.stale ? tr("workHistory.stale") : childrenPage.error}<Button size="sm" color="link-gray" onClick={childrenPage.stale ? childrenPage.refresh : childrenPage.retry}>{tr(childrenPage.stale ? "workHistory.refresh" : "common.retry")}</Button></div>}
                        {childrenPage.hasMore && <Button size="sm" color="link-gray" isDisabled={childrenPage.loading || childrenPage.stale || childrenChanged} onClick={childrenPage.more}>{tr("workHistory.children")}</Button>}
                    </DrawerSection>
                )}
                <DrawerSection title={tr("tasks.turns")}>
                    {accounting.items.length === 0 ? <div className="text-xs text-quaternary">{tr("tasks.noTurns")}</div> : (
                        <ul className="flex flex-col divide-y divide-secondary rounded-lg ring-1 ring-secondary text-xs">
                            {accounting.items.map((a) => (
                                <li key={a.index} className="flex flex-wrap items-center gap-2 px-3 py-1.5">
                                    <span className="text-tertiary">{when(a.started, locale)}</span>
                                    <span className="text-primary">{a.agent}</span>
                                    <span className="text-quaternary">{a.model || ""}</span>
                                    <span className="ml-auto text-tertiary">{fmtSeconds(a.seconds, locale)} · {spend(a.tokens, locale)}</span>
                                    {a.outcome && <Badge type="pill-color" size="sm" color={a.outcome === "ok" ? "success" : "error"}>{a.outcome}</Badge>}
                                </li>
                            ))}
                        </ul>
                    )}
                    <p className="mt-2 text-xs text-tertiary">{tr("workHistory.loaded", { count: accounting.items.length, total: accounting.total ?? tr("common.unknown") })}</p>
                    {accountingChanged && <div role="alert" className="mt-2 text-xs text-error-primary">{tr("workHistory.stale")}<Button size="sm" color="link-gray" onClick={reload}>{tr("workHistory.refresh")}</Button></div>}
                    {accounting.error && <div role="alert" className="mt-2 text-xs text-error-primary">{accounting.stale ? tr("workHistory.stale") : accounting.error}<Button size="sm" color="link-gray" onClick={accounting.stale ? accounting.refresh : accounting.retry}>{tr(accounting.stale ? "workHistory.refresh" : "common.retry")}</Button></div>}
                    {accounting.hasMore && <Button size="sm" color="link-gray" isDisabled={accounting.loading || accounting.stale || accountingChanged} onClick={accounting.more}>{tr("workHistory.turns")}</Button>}
                </DrawerSection>
                <details onToggle={(event) => setPlansOpen(event.currentTarget.open)} className="border-t border-secondary py-3">
                    <summary className="cursor-pointer rounded text-sm font-medium text-secondary outline-focus-ring focus-visible:outline-2">{tr("workHistory.plans")}</summary>
                    {plansOpen && <PlanHistory taskID={t.id} />}
                </details>
                <details onToggle={(event) => setFilesOpen(event.currentTarget.open)} className="border-t border-secondary py-3">
                    <summary className="cursor-pointer rounded text-sm font-medium text-secondary outline-focus-ring focus-visible:outline-2">{tr("workHistory.files")}</summary>
                    {filesOpen && <ArtifactsTab scope={{ task_id: t.id }} all={tasks} />}
                </details>
        </Drawer>
    );
}

function Row({ k, v }: { k: string; v: string }) {
    return <div className="flex gap-3"><span className="w-20 shrink-0 text-tertiary">{k}</span><span className="text-primary">{v}</span></div>;
}

function PlanHistory({ taskID }: { taskID: string }) {
    const { t: tr } = useI18n();
    const page = useWorkPage(`plans:${taskID}`, (cursor, signal) => fetchPlans(taskID, cursor, signal));
    return <div className="py-3 text-xs text-secondary">
        <p>{tr("workHistory.loaded", { count: page.items.length, total: page.total ?? tr("common.unknown") })}</p>
        {page.loading && <p role="status">{tr("workHistory.loading")}</p>}
        {page.error && <p role="alert">{page.stale ? tr("workHistory.stale") : page.error}</p>}
        <Button size="sm" color="link-gray" isDisabled={page.loading} onClick={page.error && !page.stale ? page.retry : page.refresh}>{tr(page.error && !page.stale ? "common.retry" : "workHistory.refresh")}</Button>
        {page.items.map((plan) => <details key={plan.id} className="border-b border-secondary py-2"><summary className="cursor-pointer rounded outline-focus-ring focus-visible:outline-2">{tr("tasks.planRevision", { plan: plan.id, revision: plan.rev, by: plan.by })} · {plan.goal}</summary><ol>{plan.steps.map((step) => <li key={step.id} className="flex flex-wrap gap-2 py-1"><StateBadge state={step.state} />{step.id} · {step.goal}</li>)}</ol></details>)}
        {page.hasMore && <Button size="sm" color="link-gray" isDisabled={page.loading || page.stale} onClick={page.more}>{tr("workHistory.more")}</Button>}
    </div>;
}

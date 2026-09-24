import { IconButton } from "@/components/steve/icon-button";
import { Link } from "react-router";
import { memo, useEffect, useState, useSyncExternalStore } from "react";
import { GitBranch01, MessageChatSquare, X } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Tab, TabList, Tabs } from "@/components/application/tabs/tabs";
import { when } from "@/lib/format";
import { useFleet } from "@/lib/fleet";
import { useNodeLabel } from "@/lib/node-name";
import { label, labelsFor } from "@/lib/labels";
import { placeLabel } from "@/lib/workspaces";
import type { ConversationContext, Plan, Reply, Task } from "@/lib/types";
import { CallGraph } from "./call-graph";
import { TaskDrawer } from "./task-drawer";
import { useI18n } from "@/providers/locale-provider";
import { MaterialShelf } from "./material-shelf";
import { useProjectMaterials } from "@/lib/materials";
import { useMaterial } from "@/providers/material-provider";
import { getSubmissionSupport, subscribeSubmissionSupport } from "@/lib/api/console";
import { ArtifactsTab } from "./work-tabs";
import { SetupPanel } from "./setup-panel";

// RAIL_WIDTH is the console's right column; a drawer opened from it is
// the same width, so the side of the page does not jump.
export const RAIL_WIDTH = 360;
import { Chips, KeyValue, Panel } from "./page";
import { InjectedPanel, ProcessBody, Working } from "./trace";
import type { Live } from "@/lib/live";
import { Mono, Nothing, Where } from "./ui";

export type RailTab = "context" | "trace" | "graph" | "artifacts" | "materials";

// Rail is the console's right column: the session's facts, the trace of
// the line in flight or the one picked, and the call graph of who is
// working for this session.
export const Rail = memo(function Rail({ conversation, context, live, plans, reply, tab, setTab, roots, onClose }: { conversation: string; context: ConversationContext | null; live: Live | null; plans: Plan[]; reply: Reply | null; tab: RailTab; setTab: (t: RailTab) => void; roots: Task[]; onClose: () => void }) {
    const [picked, setPicked] = useState<Task | null>(null);
    const snap = useFleet((fleet) => fleet.snap);
    const { t, locale } = useI18n();
    const nodeLabelOf = useNodeLabel();
    const support = useSyncExternalStore(subscribeSubmissionSupport, getSubmissionSupport);
    // The count sits on the tab so a project with materials is visible
    // without opening the shelf; the read is shared with the shelf.
    const store = useMaterial();
    const { materials } = useProjectMaterials(support.material_refs ? context?.project?.id : undefined, store.revision);
    // The trace tab takes over while something runs, and returns to
    // context when the user asks.
    useEffect(() => { if (live?.exchangeID) setTab("trace"); }, [live?.exchangeID, setTab]);
    const usable = context?.agents.filter((a) => a.usable) ?? [];
    const elsewhere = context?.agents.filter((a) => !a.usable) ?? [];
    return (
        <aside className="workbench-inspector" aria-label={t("console.details")} >
            <div className="inspector-heading"><strong>{t("console.details")}</strong><IconButton label={t("console.closeDetails")} onClick={onClose} icon={X} /></div>
            <div className="inspector-tabs">
                <Tabs selectedKey={tab} onSelectionChange={(k) => setTab(k as RailTab)}>
                    <TabList type="button-border" size="sm" items={[{ id: "context", label: t("console.conversation") }, { id: "trace", label: t("console.trace") }, { id: "graph", label: t("console.graph") }, { id: "artifacts", label: t("console.artifacts") }, ...(support.material_refs ? [{ id: "materials", label: t("materials.shelf"), badge: materials.length || undefined }] : [])]}>{(item) => <Tab {...item} />}</TabList>
                </Tabs>
            </div>
            <div className="inspector-body">
                {tab === "context" && context && (
                    <>
                        <Panel variant="section" title={t("console.project")}  badge={context.project && !context.project.bound ? <Badge type="pill-color" size="sm" color="gray">{t("console.unbound")}</Badge> : undefined}>
                            {context.project ? (
                                <KeyValue dense rows={[
                                    { k: t("console.name"), v: <span className="font-medium">{context.project.id}</span> },
                                    { k: t("console.projectHost"), v: <Where node={context.project.node} /> },
                                    { k: t("console.canonical"), v: <Mono className="text-secondary">{context.project.path}</Mono> },
                                    { k: t("console.workspace"), v: context.agent?.place ? placeLabel(context.agent.place, locale, nodeLabelOf) : t("console.notSelected") },
                                    { k: t("console.workMode"), v: label(labelsFor(locale).repo, context.project.repo), hint: t("console.workModeHint") },
                                    { k: t("console.level"), v: context.project.level },
                                ]} />
                            ) : <span className="text-sm text-quaternary">{t("console.noProject")}</span>}

                        </Panel>
                        <Panel variant="section" title={t("console.currentAgent")} >
                            {context.agent ? (
                                <KeyValue dense rows={[
                                    { k: t("console.name"), v: <span className="font-medium">{context.agent.id}</span> },
                                    { k: t("console.machine"), v: <Where node={context.agent.node} /> },
                                    { k: t("console.harness"), v: context.agent.harness },
                                    { k: t("console.model"), v: context.agent.model || <span className="text-quaternary">{t("console.unobserved")}</span> },
                                    { k: t("console.state"), v: context.agent.ready ? <span className="text-success-primary">{t("console.ready")}</span> : <span className="text-error-primary">{t("console.unavailablePrefix")} · {context.agent.why}</span> },
                                ]} />
                            ) : <span className="text-sm text-quaternary">{t("console.noAgent")}</span>}
                        </Panel>
                        <Panel variant="section" title={t("console.usableAgents")} >
                            <div className="flex flex-col gap-2 text-sm">
                                <Chips items={usable.map((a) => ({ id: a.id, title: `${a.node ? nodeLabelOf(a.node) : ""} · ${a.harness}` }))} empty={<span className="text-error-primary">{t("console.noUsableAgents")}</span>} />
                                {elsewhere.length > 0 && (
                                    <ul className="flex flex-col gap-1 text-xs text-tertiary">
                                        {elsewhere.map((a) => <li key={a.id}><Mono className="text-quaternary">{a.id}</Mono> <span>{a.because || a.why}</span></li>)}
                                    </ul>
                                )}
                            </div>
                        </Panel>
                        <SetupPanel conversation={context.conversation} agent={context.agent?.id} node={context.agent?.node} />
                    </>
                )}
                {tab === "trace" && (
                    live ? <Working live={live} plans={plans} /> : (reply?.process || reply?.injected) ? (
                        <>
                            {reply.injected && <InjectedPanel at={reply.at} in={reply.injected} />}
                            {reply.process && (
                                <Panel variant="section" title={`${t("console.trace")} · ${when(reply.at, locale)}`}>
                                    <ProcessBody process={reply.process} />
                                </Panel>
                            )}
                        </>
                    ) : <Nothing icon={MessageChatSquare} title={t("console.noTrace")} >{t("console.noTraceHint")}</Nothing>
                )}
                {tab === "materials" && support.material_refs && context?.project && <MaterialShelf key={context.project.id} project={context.project.id} />}
                {tab === "artifacts" && <ArtifactsTab scope={{ conversation }} all={snap.tasks} />}
                {tab === "graph" && <p className="py-2 text-xs text-tertiary">{t("workHistory.coverage", { count: snap.task_coverage.recent_closed })} <Link className="rounded underline outline-focus-ring focus-visible:outline-2" to={`/console?view=board&tab=all&history_conversation=${encodeURIComponent(conversation)}`}>{t("workHistory.history")}</Link></p>}
                {tab === "graph" && (
                    roots.length ? (
                        <Panel variant="section" title={t("console.workingFor")}  badge={<span className="text-xs text-tertiary">{t("console.treeHint")}</span>}>
                            <CallGraph roots={roots} tasks={snap.tasks} plans={snap.plans} liveSteps={live?.order} onSelect={setPicked} />
                        </Panel>
                    ) : <Nothing icon={GitBranch01} title={t("console.noTasks")} >{t("console.noTasksHint")}</Nothing>
                )}
            </div>
            {picked && <TaskDrawer t={picked} tasks={snap.tasks} plan={snap.plans.find((p) => p.task_id === picked.id)} onClose={() => setPicked(null)} width={RAIL_WIDTH} />}
        </aside>
    );
});

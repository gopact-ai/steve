import { useEffect, useState, useSyncExternalStore } from "react";
import { GitBranch01, MessageChatSquare, X } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Tab, TabList, Tabs } from "@/components/application/tabs/tabs";
import { when } from "@/lib/format";
import { useFleet } from "@/lib/fleet";
import { label, zh } from "@/lib/labels";
import { placeLabel } from "@/lib/workspaces";
import type { ConversationContext, Plan, Reply, Task } from "@/lib/types";
import { CallGraph } from "./call-graph";
import { TaskDrawer } from "./task-drawer";
import { useI18n } from "@/providers/locale-provider";
import { MaterialShelf } from "./material-shelf";
import { getSubmissionSupport, subscribeSubmissionSupport } from "@/lib/api/console";
import { CodeTab } from "./work-tabs";

// RAIL_WIDTH is the console's right column; a drawer opened from it is
// the same width, so the side of the page does not jump.
export const RAIL_WIDTH = 360;
import { Chips, KeyValue, Panel } from "./page";
import { InjectedPanel, ProcessBody, Working } from "./trace";
import type { Live } from "@/lib/live";
import { Mono, Nothing } from "./ui";

export type RailTab = "context" | "trace" | "graph" | "code" | "materials";

// Rail is the console's right column: the session's facts, the trace of
// the line in flight or the one picked, and the call graph of who is
// working for this session.
export function Rail({ context, live, plans, reply, tab, setTab, roots, onClose }: { context: ConversationContext | null; live: Live | null; plans: Plan[]; reply: Reply | null; tab: RailTab; setTab: (t: RailTab) => void; roots: Task[]; onClose: () => void }) {
    const [picked, setPicked] = useState<Task | null>(null);
    const { snap } = useFleet();
    const { t, locale } = useI18n();
    const support = useSyncExternalStore(subscribeSubmissionSupport, getSubmissionSupport);
    // The trace tab takes over while something runs, and returns to
    // context when the user asks.
    useEffect(() => { if (live?.exchangeID) setTab("trace"); }, [live?.exchangeID, setTab]);
    const usable = context?.agents.filter((a) => a.usable) ?? [];
    const elsewhere = context?.agents.filter((a) => !a.usable) ?? [];
    return (
        <aside className="workbench-inspector" aria-label={t("console.details")} >
            <div className="inspector-heading"><strong>{t("console.details")}</strong><button type="button" className="workbench-icon-button" aria-label={t("console.closeDetails")}  onClick={onClose}><X aria-hidden="true" /></button></div>
            <div className="inspector-tabs">
                <Tabs selectedKey={tab} onSelectionChange={(k) => setTab(k as RailTab)}>
                    <TabList type="button-border" size="sm" items={[{ id: "context", label: t("console.conversation") }, { id: "trace", label: t("console.trace") }, { id: "graph", label: t("console.graph") }, { id: "code", label: t("console.code") }, ...(support.material_refs ? [{ id: "materials", label: t("materials.shelf") }] : [])]}>{(item) => <Tab {...item} />}</TabList>
                </Tabs>
            </div>
            <div className="inspector-body">
                {tab === "context" && context && (
                    <>
                        <Panel title={t("console.project")}  badge={context.project && !context.project.bound ? <Badge type="pill-color" size="sm" color="gray">{t("console.unbound")}</Badge> : undefined}>
                            {context.project ? (
                                <KeyValue dense rows={[
                                    { k: t("console.name"), v: <span className="font-medium">{context.project.id}</span> },
                                    { k: t("console.projectHost"), v: <Mono>{context.project.node}</Mono> },
                                    { k: t("console.canonical"), v: <Mono className="text-secondary">{context.project.path}</Mono> },
                                    { k: t("console.workspace"), v: context.agent?.place ? placeLabel(context.agent.place, locale) : t("console.notSelected") },
                                    { k: t("console.workMode"), v: label(zh.repo, context.project.repo), hint: t("console.workModeHint") },
                                    { k: t("console.level"), v: context.project.level },
                                ]} />
                            ) : <span className="text-sm text-quaternary">{t("console.noProject")}</span>}

                        </Panel>
                        <Panel title={t("console.currentAgent")} >
                            {context.agent ? (
                                <KeyValue dense rows={[
                                    { k: t("console.name"), v: <span className="font-medium">{context.agent.id}</span> },
                                    { k: t("console.machine"), v: <Mono>{context.agent.node}</Mono> },
                                    { k: t("console.harness"), v: context.agent.harness },
                                    { k: t("console.model"), v: context.agent.model || <span className="text-quaternary">{t("console.unobserved")}</span> },
                                    { k: t("console.state"), v: context.agent.ready ? <span className="text-success-primary">{t("console.ready")}</span> : <span className="text-error-primary">{t("console.unavailablePrefix")} · {context.agent.why}</span> },
                                ]} />
                            ) : <span className="text-sm text-quaternary">{t("console.noAgent")}</span>}
                        </Panel>
                        <Panel title={t("console.usableAgents")} >
                            <div className="flex flex-col gap-2 text-sm">
                                <Chips items={usable.map((a) => ({ id: a.id, title: `${a.node} · ${a.harness}` }))} empty={<span className="text-error-primary">{t("console.noUsableAgents")}</span>} />
                                {elsewhere.length > 0 && (
                                    <ul className="flex flex-col gap-1 text-xs text-tertiary">
                                        {elsewhere.map((a) => <li key={a.id}><Mono className="text-quaternary">{a.id}</Mono> <span>{a.because || a.why}</span></li>)}
                                    </ul>
                                )}
                            </div>
                        </Panel>
                    </>
                )}
                {tab === "trace" && (
                    live ? <Working live={live} plans={plans} /> : (reply?.process || reply?.injected) ? (
                        <>
                            {reply.injected && <InjectedPanel at={reply.at} in={reply.injected} />}
                            {reply.process && (
                                <Panel title={`${t("console.trace")} · ${when(reply.at, locale)}`}>
                                    <ProcessBody process={reply.process} />
                                </Panel>
                            )}
                        </>
                    ) : <Nothing icon={MessageChatSquare} title={t("console.noTrace")} >{t("console.noTraceHint")}</Nothing>
                )}
                {tab === "materials" && support.material_refs && context?.project && <MaterialShelf key={context.project.id} project={context.project.id} />}
                {tab === "code" && <CodeTab roots={roots} all={snap.tasks} />}
                {tab === "graph" && (
                    roots.length ? (
                        <Panel title={t("console.workingFor")}  badge={<span className="text-xs text-tertiary">{t("console.treeHint")}</span>}>
                            <CallGraph roots={roots} tasks={snap.tasks} plans={snap.plans} liveSteps={live?.order} onSelect={setPicked} />
                        </Panel>
                    ) : <Nothing icon={GitBranch01} title={t("console.noTasks")} >{t("console.noTasksHint")}</Nothing>
                )}
            </div>
            {picked && <TaskDrawer t={picked} tasks={snap.tasks} plan={snap.plans.find((p) => p.task_id === picked.id)} onClose={() => setPicked(null)} width={RAIL_WIDTH} />}
        </aside>
    );
}

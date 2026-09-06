import { useEffect, useState } from "react";
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
import { CodeTab } from "./work-tabs";

// RAIL_WIDTH is the console's right column; a drawer opened from it is
// the same width, so the side of the page does not jump.
export const RAIL_WIDTH = 360;
import { Chips, KeyValue, Panel } from "./page";
import { InjectedPanel, ProcessBody, Working } from "./trace";
import type { Live } from "@/lib/live";
import { Mono, Nothing } from "./ui";

export type RailTab = "context" | "trace" | "graph" | "code";

// Rail is the console's right column: the session's facts, the trace of
// the line in flight or the one picked, and the call graph of who is
// working for this session.
export function Rail({ context, live, plans, reply, tab, setTab, roots, onClose }: { context: ConversationContext | null; live: Live | null; plans: Plan[]; reply: Reply | null; tab: RailTab; setTab: (t: RailTab) => void; roots: Task[]; onClose: () => void }) {
    const [picked, setPicked] = useState<Task | null>(null);
    const { snap } = useFleet();
    // The trace tab takes over while something runs, and returns to
    // context when the user asks.
    useEffect(() => { if (live?.exchangeID) setTab("trace"); }, [live?.exchangeID, setTab]);
    const usable = context?.agents.filter((a) => a.usable) ?? [];
    const elsewhere = context?.agents.filter((a) => !a.usable) ?? [];
    return (
        <aside className="workbench-inspector" aria-label="详情">
            <div className="inspector-heading"><strong>详情</strong><button type="button" className="workbench-icon-button" aria-label="关闭详情" onClick={onClose}><X aria-hidden="true" /></button></div>
            <div className="inspector-tabs">
                <Tabs selectedKey={tab} onSelectionChange={(k) => setTab(k as RailTab)}>
                    <TabList type="button-border" size="sm" items={[{ id: "context", label: "会话" }, { id: "trace", label: "过程" }, { id: "graph", label: "关系" }, { id: "code", label: "代码" }]}>{(item) => <Tab {...item} />}</TabList>
                </Tabs>
            </div>
            <div className="inspector-body">
                {tab === "context" && context && (
                    <>
                        <Panel title="项目" badge={context.project && !context.project.bound ? <Badge type="pill-color" size="sm" color="gray">默认，未绑定</Badge> : undefined}>
                            {context.project ? (
                                <KeyValue dense rows={[
                                    { k: "名字", v: <span className="font-medium">{context.project.id}</span> },
                                    { k: "项目主机", v: <Mono>{context.project.node}</Mono> },
                                    { k: "主目录", v: <Mono className="text-secondary">{context.project.path}</Mono> },
                                    { k: "当前工作区", v: context.agent?.place ? placeLabel(context.agent.place) : "尚未选择" },
                                    { k: "工作方式", v: label(zh.repo, context.project.repo), hint: "直接修改主目录：只有项目主机上的 Agent 能接。隔离副本：计划在别的机器上物化副本，完成后合并。" },
                                    { k: "数据等级", v: context.project.level },
                                ]} />
                            ) : <span className="text-sm text-quaternary">没有项目</span>}

                        </Panel>
                        <Panel title="当前 Agent">
                            {context.agent ? (
                                <KeyValue dense rows={[
                                    { k: "名字", v: <span className="font-medium">{context.agent.id}</span> },
                                    { k: "机器", v: <Mono>{context.agent.node}</Mono> },
                                    { k: "AI 工具", v: context.agent.harness },
                                    { k: "实际模型", v: context.agent.model || <span className="text-quaternary">未观测到</span> },
                                    { k: "状态", v: context.agent.ready ? <span className="text-success-primary">可用</span> : <span className="text-error-primary">不可用 · {context.agent.why}</span> },
                                ]} />
                            ) : <span className="text-sm text-quaternary">没有当前 Agent</span>}
                        </Panel>
                        <Panel title="可用 Agent">
                            <div className="flex flex-col gap-2 text-sm">
                                <Chips items={usable.map((a) => ({ id: a.id, title: `${a.node} · ${a.harness}` }))} empty={<span className="text-error-primary">当前项目没有可用 Agent</span>} />
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
                                <Panel title={`过程 · ${when(reply.at)}`}>
                                    <ProcessBody process={reply.process} />
                                </Panel>
                            )}
                        </>
                    ) : <Nothing icon={MessageChatSquare} title="暂无执行记录">发送消息后，在这里查看进度和工具调用。</Nothing>
                )}
                {tab === "code" && <CodeTab roots={roots} all={snap.tasks} />}
                {tab === "graph" && (
                    roots.length ? (
                        <Panel title="谁在为这条会话干活" badge={<span className="text-xs text-tertiary">任务 → 步骤 / 委派</span>}>
                            <CallGraph roots={roots} tasks={snap.tasks} plans={snap.plans} liveSteps={live?.order} onSelect={setPicked} />
                        </Panel>
                    ) : <Nothing icon={GitBranch01} title="暂无关联任务">创建任务后，在这里查看委派关系。</Nothing>
                )}
            </div>
            {picked && <TaskDrawer t={picked} tasks={snap.tasks} plan={snap.plans.find((p) => p.task_id === picked.id)} onClose={() => setPicked(null)} width={RAIL_WIDTH} />}
        </aside>
    );
}

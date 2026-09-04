import { useEffect } from "react";
import { GitBranch01, MessageChatSquare } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Tab, TabList, Tabs } from "@/components/application/tabs/tabs";
import { when } from "@/lib/api";
import { useFleet } from "@/lib/fleet";
import { label, zh } from "@/lib/labels";
import type { ConversationContext, Plan, Reply, Task } from "@/lib/types";
import { CallGraph } from "./call-graph";
import { Chips, KeyValue, Panel } from "./page";
import { InjectedPanel, ProcessBody, Working, type Live } from "./trace";
import { Mono, Nothing } from "./ui";

export type RailTab = "context" | "trace" | "graph";

// Rail is the console's right column: the session's facts, the trace of
// the line in flight or the one picked, and the call graph of who is
// working for this session.
export function Rail({ context, live, plans, reply, tab, setTab, roots }: { context: ConversationContext | null; live: Live | null; plans: Plan[]; reply: Reply | null; tab: RailTab; setTab: (t: RailTab) => void; roots: Task[] }) {
    const { snap } = useFleet();
    // The trace tab takes over while something runs, and returns to
    // context when the user asks.
    useEffect(() => { if (live) setTab("trace"); }, [live, setTab]);
    const usable = context?.agents.filter((a) => a.usable) ?? [];
    const elsewhere = context?.agents.filter((a) => !a.usable) ?? [];
    return (
        <aside className="hidden min-h-0 flex-col border-l border-secondary bg-secondary xl:flex">
            <div className="border-b border-secondary bg-primary px-4 py-2">
                <Tabs selectedKey={tab} onSelectionChange={(k) => setTab(k as RailTab)}>
                    <TabList type="button-border" size="sm" items={[{ id: "context", label: "会话" }, { id: "trace", label: live ? "过程（进行中）" : "过程" }, { id: "graph", label: roots.length ? `关系 (${roots.length})` : "关系" }]}>{(item) => <Tab {...item} />}</TabList>
                </Tabs>
            </div>
            <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4">
                {tab === "context" && context && (
                    <>
                        <Panel title="项目" badge={context.project && !context.project.bound ? <Badge type="pill-color" size="sm" color="gray">默认，未绑定</Badge> : undefined}>
                            {context.project ? (
                                <KeyValue dense rows={[
                                    { k: "名字", v: <span className="font-medium">{context.project.id}</span> },
                                    { k: "项目主机", v: <Mono>{context.project.node}</Mono> },
                                    { k: "主目录", v: <Mono className="text-secondary">{context.project.path}</Mono> },
                                    { k: "工作方式", v: label(zh.repo, context.project.repo), hint: "直接修改主目录：只有项目主机上的 Agent 能接。隔离副本：计划在别的机器上物化副本，完成后合并。" },
                                    { k: "数据等级", v: context.project.level },
                                ]} />
                            ) : <span className="text-sm text-quaternary">没有项目</span>}
                            <p className="text-xs text-quaternary">切换项目只影响本会话：已有任务不迁移，当前 Agent 的会话归档并新开，有回合在跑时不能切。</p>
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
                        <Panel title="谁能接本会话">
                            <div className="flex flex-col gap-2 text-sm">
                                <Chips items={usable.map((a) => ({ id: a.id, title: `${a.node} · ${a.harness}` }))} empty={<span className="text-error-primary">没有 — 换一个 Agent 所在机器上的项目</span>} />
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
                    ) : <Nothing icon={MessageChatSquare} title="还没有过程">发一条消息，这里会显示给 agent 的上下文、它的推理、工具调用和步骤。</Nothing>
                )}
                {tab === "graph" && (
                    roots.length ? (
                        <Panel title="谁在为这条会话干活" badge={<span className="text-xs text-tertiary">任务 → 步骤 / 委派</span>}>
                            <CallGraph roots={roots} tasks={snap.tasks} plans={snap.plans} liveSteps={live?.order} />
                        </Panel>
                    ) : <Nothing icon={GitBranch01} title="还没有任务">这条会话的任务、它拆出的步骤、以及 Agent 之间的委派会画在这里。</Nothing>
                )}
            </div>
        </aside>
    );
}

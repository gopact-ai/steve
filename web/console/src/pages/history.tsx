import { useEffect, useState } from "react";
import { BookOpen01, Database01, SearchSm, ShieldTick, Users01, Zap } from "@untitledui/icons";
import { Table, TableCard } from "@/components/application/table/table";
import { Tab, TabList, Tabs } from "@/components/application/tabs/tabs";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { fetchHistory, short, when } from "@/lib/api";
import { useFleet } from "@/lib/fleet";
import type { HistoryEntry } from "@/lib/types";
import { PageBody, PageHeader } from "@/components/steve/page";
import { Mono, Nothing, StateBadge } from "@/components/steve/ui";

// HistoryPage answers "what happened, who did it, how did it end". The
// timeline reads the ledger journal and the connectivity observations,
// paged by the ledger's sequence; the audit tab holds the raw records.
export function HistoryPage() {
    const { snap, events } = useFleet();
    const [tab, setTab] = useState<"timeline" | "audit">("timeline");
    const [entries, setEntries] = useState<HistoryEntry[]>([]);
    const [next, setNext] = useState(0);
    const [loading, setLoading] = useState(false);
    const [filter, setFilter] = useState("");
    const load = async (before: number, replace: boolean) => {
        setLoading(true);
        try {
            const data = await fetchHistory(before);
            setEntries((list) => replace ? data.entries : [...list, ...data.entries]);
            setNext(data.next);
        } finally { setLoading(false); }
    };
    useEffect(() => { void load(0, true); }, []);
    // A new event means the head of the timeline moved; re-read it.
    useEffect(() => { if (events.length) void load(0, true); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [events.length]);
    const shown = entries.filter((e) => !filter || `${e.subject} ${e.text} ${e.actor}`.toLowerCase().includes(filter.toLowerCase()));
    const f = snap.facts;
    return (
        <div className="workbench-page flex min-w-0 flex-col">
            <PageHeader title="历史" description="查找任务变化、机器事件与审计记录。">
                <Tabs selectedKey={tab} onSelectionChange={(k) => setTab(k as "timeline" | "audit")}>
                    <TabList type="button-border" size="sm" items={[{ id: "timeline", label: "时间线" }, { id: "audit", label: "审计" }]}>{(item) => <Tab {...item} />}</TabList>
                </Tabs>
            </PageHeader>
            <PageBody>
            {tab === "timeline" && (
                <div className="workbench-panel min-w-0 rounded-lg bg-primary ring-1 ring-secondary">
                    <div className="flex min-w-0 items-center gap-3 border-b border-secondary px-4 py-3">
                        <Input size="sm" aria-label="筛选历史记录" icon={SearchSm} className="min-w-0 max-w-sm flex-1" placeholder="搜索任务、机器或操作者…" value={filter} onChange={setFilter} />
                        <span className="shrink-0 text-xs tabular-nums text-tertiary">{shown.length} 条</span>
                    </div>
                    {shown.length === 0 ? <Nothing icon={BookOpen01} title={filter ? "没有匹配的记录" : loading ? "正在加载记录…" : "还没有记录"} /> : (
                        <ol className="divide-y divide-secondary">
                            {shown.map((e, i) => (
                                <li key={`${e.seq}-${e.at}-${i}`} className="flex min-w-0 flex-wrap items-start gap-x-3 gap-y-1 px-4 py-3 text-sm">
                                    <span className="w-36 shrink-0 text-xs tabular-nums text-tertiary">{when(e.at)}</span>
                                    <Badge type="pill-color" size="sm" color={e.kind.startsWith("observe") ? "blue" : "gray"}>{e.kind === "ledger" ? "账本" : e.kind.replace("observe.", "机器 ")}</Badge>
                                    <span className="min-w-0 basis-full break-words text-primary md:flex-1 md:basis-auto">{e.text}</span>
                                    {e.operation && <Mono className="shrink-0 text-quaternary">{short(e.operation)}</Mono>}
                                </li>
                            ))}
                        </ol>
                    )}
                    <div className="border-t border-secondary px-5 py-2">
                        <Button size="sm" color="link-gray" isLoading={loading} isDisabled={!next} onClick={() => void load(next, false)}>{next ? "加载更早记录" : "已显示全部记录"}</Button>
                    </div>
                </div>
            )}
            {tab === "audit" && (
                <div className="grid min-w-0 grid-cols-1 gap-5 xl:grid-cols-2">
                    <TableCard.Root size="sm" className="workbench-table min-w-0">
                        <TableCard.Header title="容量预留" badge={`${f.reservations.length}`} description="尚未执行的步骤所需容量。" />
                        {f.reservations.length === 0 ? <Nothing icon={Zap} title="无" /> : (
                            <Table aria-label="预留" size="sm">
                                <Table.Header><Table.Head id="key" label="键" isRowHeader /><Table.Head id="where" label="机器 · 工具" /><Table.Head id="slots" label="槽位" /><Table.Head id="for" label="为" /><Table.Head id="exp" label="到期" /></Table.Header>
                                <Table.Body items={f.reservations}>{(r) => <Table.Row id={r.id}><Table.Cell><Mono>{r.key}</Mono></Table.Cell><Table.Cell>{r.node} · {r.harness}</Table.Cell><Table.Cell>{r.slots}</Table.Cell><Table.Cell>{r.for}</Table.Cell><Table.Cell><span className="text-tertiary">{when(r.expires_at)}</span></Table.Cell></Table.Row>}</Table.Body>
                            </Table>
                        )}
                    </TableCard.Root>
                    <TableCard.Root size="sm" className="workbench-table min-w-0">
                        <TableCard.Header title="授权" badge={`${f.grants.length}`} description="项目访问权限。所有者拥有全局管理权限。" />
                        {f.grants.length === 0 ? <Nothing icon={Users01} title="无" /> : (
                            <Table aria-label="授权" size="sm">
                                <Table.Header><Table.Head id="project" label="项目" isRowHeader /><Table.Head id="who" label="谁" /><Table.Head id="role" label="角色" /><Table.Head id="by" label="授予者" /></Table.Header>
                                <Table.Body items={f.grants.map((g, i) => ({ ...g, id: `${g.project}-${g.principal}-${i}` }))}>{(g) => <Table.Row id={g.id}><Table.Cell>{g.project}</Table.Cell><Table.Cell><Mono>{g.principal}</Mono></Table.Cell><Table.Cell>{g.role}</Table.Cell><Table.Cell><span className="text-tertiary">{g.by}</span></Table.Cell></Table.Row>}</Table.Body>
                            </Table>
                        )}
                    </TableCard.Root>
                    <TableCard.Root size="sm" className="workbench-table min-w-0">
                        <TableCard.Header title="见证" badge={`${f.attestations.length}`} description="名字绑定前记录在案的验证结论。" />
                        {f.attestations.length === 0 ? <Nothing icon={ShieldTick} title="无" /> : (
                            <Table aria-label="见证" size="sm">
                                <Table.Header><Table.Head id="artifact" label="产物" isRowHeader /><Table.Head id="verdict" label="结论" /><Table.Head id="by" label="谁" /><Table.Head id="at" label="何时" /></Table.Header>
                                <Table.Body items={f.attestations.map((a, i) => ({ ...a, id: `${a.artifact}-${i}` }))}>{(a) => <Table.Row id={a.id}><Table.Cell><Mono>{short(a.artifact)}</Mono></Table.Cell><Table.Cell><StateBadge state={a.verdict} /></Table.Cell><Table.Cell>{a.by}</Table.Cell><Table.Cell><span className="text-tertiary">{when(a.at)}</span></Table.Cell></Table.Row>}</Table.Body>
                            </Table>
                        )}
                    </TableCard.Root>
                    <TableCard.Root size="sm" className="workbench-table min-w-0">
                        <TableCard.Header title="副本" badge={`${f.replicas.length}`} description="产物在各机器上的副本，按机器代数。" />
                        {f.replicas.length === 0 ? <Nothing icon={Database01} title="无" /> : (
                            <Table aria-label="副本" size="sm">
                                <Table.Header><Table.Head id="artifact" label="产物" isRowHeader /><Table.Head id="node" label="机器" /><Table.Head id="gen" label="代" /><Table.Head id="state" label="状态" /><Table.Head id="at" label="何时" /></Table.Header>
                                <Table.Body items={f.replicas.map((r, i) => ({ ...r, id: `${r.artifact}-${r.node}-${i}` }))}>{(r) => <Table.Row id={r.id}><Table.Cell><Mono>{short(r.artifact)}</Mono></Table.Cell><Table.Cell>{r.node}</Table.Cell><Table.Cell>{r.generation}</Table.Cell><Table.Cell><StateBadge state={r.state} /></Table.Cell><Table.Cell><span className="text-tertiary">{when(r.at)}</span></Table.Cell></Table.Row>}</Table.Body>
                            </Table>
                        )}
                    </TableCard.Root>
                    <TableCard.Root size="sm" className="workbench-table min-w-0 xl:col-span-2">
                        <TableCard.Header title="实时事件" badge={`${events.length}`} description="本次连接收到的事件。持久记录见时间线。" />
                        {events.length === 0 ? <Nothing icon={BookOpen01} title="暂无事件" /> : (
                            <ul className="max-h-96 divide-y divide-secondary overflow-y-auto text-xs">
                                {events.slice(0, 200).map((e, i) => <li key={i} className="flex gap-3 px-5 py-1.5"><span className="w-32 shrink-0 text-tertiary">{when(e.at)}</span><Mono>{e.kind}</Mono><span className="truncate text-secondary">{e.detail || e.text || ""}{e.task_id ? ` · #${e.task_id}` : ""}{e.step_id ? ` · ${e.step_id}` : ""}</span></li>)}
                            </ul>
                        )}
                    </TableCard.Root>
                </div>
            )}
            </PageBody>
        </div>
    );
}

import { Inbox01 } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { when } from "@/lib/format";
import { useFleet, useIntent } from "@/lib/fleet";
import { label, zh } from "@/lib/labels";
import { PageBody, PageHeader } from "@/components/steve/page";
import { Nothing } from "@/components/steve/ui";
import { unavailableSource } from "@/lib/source-health";

// InboxPage answers "what exactly do I have to decide right now". Only
// requests with actions and quarantined writers appear. Writer confirmation
// requires physical process evidence, so it intentionally has no action button.
export function InboxPage() {
    const { snap } = useFleet();
    const { act } = useIntent();
    const unavailable = unavailableSource(snap.sources, "ledger-attention");
    const groups = ["writer", "disclosure", "effect", "question", "pairing"].map((type) => ({ type, items: snap.inbox.filter((r) => r.type === type && (r.resolvable || r.type === "writer")) })).filter((g) => g.items.length);
    return (
        <div className="workbench-page flex min-w-0 flex-col">
            <PageHeader title="待处理" description="核实隔离执行、确认数据披露请求，以及核对结果未知的外部操作。" />
            <PageBody>
            {unavailable && <div role="status" className="rounded-lg bg-secondary p-4 text-sm text-secondary">待处理信息尚未完整读取，当前列表可能不完整。{unavailable.error}</div>}
            {!unavailable && groups.length === 0 && <div className="workbench-panel rounded-lg bg-primary ring-1 ring-secondary"><Nothing icon={Inbox01} title="暂无待处理请求">新请求会显示在这里。</Nothing></div>}
            {groups.map((g) => (
                <section key={g.type} className="workbench-panel min-w-0 rounded-lg bg-primary ring-1 ring-secondary">
                    <div className="flex items-center gap-2 border-b border-secondary px-5 py-3">
                        <span className="text-sm font-semibold text-primary">{label(zh.requestType, g.type)}</span>
                        <Badge type="pill-color" size="sm" color="warning">{g.items.length}</Badge>
                    </div>
                    <ul className="divide-y divide-secondary">
                        {g.items.map((r) => (
                            <li key={r.id} className="flex min-w-0 flex-col items-start gap-3 px-4 py-4 sm:flex-row">
                                <div className="min-w-0 flex-1">
                                    <div className="break-words text-sm font-medium text-primary">{r.summary}</div>
                                    {r.type === "writer" && <div className="mt-2 flex flex-col gap-1 text-xs text-secondary">
                                        <p>机器：<span className="font-mono">{r.node || "—"}</span></p>
                                        <p className="break-all">目录：<span className="font-mono">{r.workspace || "—"}</span></p>
                                        <p className="break-all">执行：<span className="font-mono">{r.attempt_id || r.source}</span></p>
                                        <p className="mt-1 leading-5 text-tertiary">需运维在指定机器核实原进程已退出。断连、关闭流或重启 Hub 不能代替核实。随后按<a className="underline" href="https://github.com/gopact-ai/steve/blob/master/docs/operations.md#隔离执行与复制操作" target="_blank" rel="noreferrer">隔离恢复流程</a>停止 Hub，并通过 ledger confirm-stopped 记录证据后重新对账。</p>
                                    </div>}
                                    <div className="mt-1 break-all text-xs text-tertiary">{when(r.created_at)}{r.project_id ? ` · 项目 ${r.project_id}` : ""}{r.task_id ? ` · 任务 #${r.task_id}` : ""} · <span className="font-mono">{r.id}</span></div>
                                </div>
                                <div className="flex shrink-0 flex-wrap gap-2">
                                    {r.type !== "writer" && r.choices.map((c) => (
                                        <Button key={c.command} size="sm" color={c.danger ? "secondary-destructive" : "secondary"} onClick={() => { if (!c.danger || window.confirm(`${c.label}？\n\n${c.command}`)) act(c.command); }}>{c.label}</Button>
                                    ))}
                                </div>
                            </li>
                        ))}
                    </ul>
                </section>
            ))}
            <p className="text-xs text-tertiary">Agent 提问与飞书接入申请，请在对应会话中处理。</p>
            </PageBody>
        </div>
    );
}

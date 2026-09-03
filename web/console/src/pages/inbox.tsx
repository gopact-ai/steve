import { Inbox01 } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { when } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import { label, zh } from "@/lib/labels";
import { PageBody, PageHeader } from "@/lib/page";
import { Nothing } from "@/lib/ui";

// InboxPage answers "what exactly do I have to decide right now". Only
// requests that are still resolvable appear; the operations behind them
// stay the authority, and every button is the command that settles one.
export function InboxPage() {
    const { snap } = useFleet();
    const { act } = useIntent();
    const groups = ["disclosure", "effect", "question", "pairing"].map((type) => ({ type, items: snap.inbox.filter((r) => r.type === type && r.resolvable) })).filter((g) => g.items.length);
    return (
        <div className="flex flex-col">
            <PageHeader title="待处理" description="只有你能定的事：允许 sealed 项目的内容发出去，对账结果未知的对外动作。回答 agent 提问与飞书接入申请的通道尚未接入控制台，这里暂不显示。" />
            <PageBody>
            {groups.length === 0 && <div className="rounded-xl bg-primary shadow-xs ring-1 ring-secondary"><Nothing icon={Inbox01} title="没有要你处理的">有事时侧栏角标会亮。</Nothing></div>}
            {groups.map((g) => (
                <section key={g.type} className="rounded-xl bg-primary shadow-xs ring-1 ring-secondary">
                    <div className="flex items-center gap-2 border-b border-secondary px-5 py-3">
                        <span className="text-sm font-semibold text-primary">{label(zh.requestType, g.type)}</span>
                        <Badge type="pill-color" size="sm" color="warning">{g.items.length}</Badge>
                    </div>
                    <ul className="divide-y divide-secondary">
                        {g.items.map((r) => (
                            <li key={r.id} className="flex items-start gap-4 px-5 py-3">
                                <div className="min-w-0 flex-1">
                                    <div className="text-sm text-primary">{r.summary}</div>
                                    <div className="mt-0.5 text-xs text-tertiary">{when(r.created_at)}{r.project_id ? ` · 项目 ${r.project_id}` : ""}{r.task_id ? ` · 任务 #${r.task_id}` : ""} · <span className="font-mono">{r.id}</span></div>
                                </div>
                                <div className="flex shrink-0 gap-2">
                                    {r.choices.map((c) => (
                                        <Button key={c.command} size="sm" color={c.danger ? "secondary-destructive" : "secondary"} onClick={() => { if (!c.danger || window.confirm(`${c.label}？\n\n${c.command}`)) act(c.command); }}>{c.label}</Button>
                                    ))}
                                </div>
                            </li>
                        ))}
                    </ul>
                </section>
            ))}
            </PageBody>
        </div>
    );
}

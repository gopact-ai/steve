import { useI18n } from "@/providers/locale-provider";
import { Inbox01 } from "@untitledui/icons";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { when } from "@/lib/format";
import { useFleet, useIntent } from "@/lib/fleet";
import { label, labelsFor } from "@/lib/labels";
import { PageBody, PageHeader } from "@/components/steve/page";
import { Nothing } from "@/components/steve/ui";
import { unavailableSource } from "@/lib/source-health";

// InboxPage answers "what exactly do I have to decide right now". Only
// requests with actions and quarantined writers appear. Writer confirmation
// requires physical process evidence, so it intentionally has no action button.
export function InboxPage() {
    const { t: tr, locale } = useI18n();
    const { snap } = useFleet();
    const { act } = useIntent();
    const unavailable = unavailableSource(snap.sources, "ledger-attention");
    const groups = ["writer", "disclosure", "effect", "question", "pairing"].map((type) => ({ type, items: snap.inbox.filter((r) => r.type === type && (r.resolvable || r.type === "writer")) })).filter((g) => g.items.length);
    return (
        <div className="workbench-page flex min-w-0 flex-col">
            <PageHeader title={tr("inbox.title")} description={tr("inbox.description")} />
            <PageBody>
            {unavailable && <div role="status" className="rounded-lg bg-secondary p-4 text-sm text-secondary">{tr("inbox.incomplete")}{unavailable.error}</div>}
            {!unavailable && groups.length === 0 && <div className="workbench-panel rounded-lg bg-primary ring-1 ring-secondary"><Nothing icon={Inbox01} title={tr("inbox.empty")}>{tr("inbox.emptyHint")}</Nothing></div>}
            {groups.map((g) => (
                <section key={g.type} className="workbench-panel min-w-0 rounded-lg bg-primary ring-1 ring-secondary">
                    <div className="flex items-center gap-2 border-b border-secondary px-5 py-3">
                        <span className="text-sm font-semibold text-primary">{label(labelsFor(locale).requestType, g.type)}</span>
                        <Badge type="pill-color" size="sm" color="warning">{g.items.length}</Badge>
                    </div>
                    <ul className="divide-y divide-secondary">
                        {g.items.map((r) => (
                            <li key={r.id} className="flex min-w-0 flex-col items-start gap-3 px-4 py-4 sm:flex-row">
                                <div className="min-w-0 flex-1">
                                    <div className="break-words text-sm font-medium text-primary">{r.summary}</div>
                                    {r.type === "writer" && <div className="mt-2 flex flex-col gap-1 text-xs text-secondary">
                                        <p>{tr("inbox.machine")}<span className="font-mono">{r.node || "—"}</span></p>
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
                </section>
            ))}
            <p className="text-xs text-tertiary">{tr("inbox.otherChannels")}</p>
            </PageBody>
        </div>
    );
}

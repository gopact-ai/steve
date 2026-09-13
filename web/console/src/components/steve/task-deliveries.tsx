import { useI18n } from "@/providers/locale-provider";
import { when } from "@/lib/format";
import type { Task } from "@/lib/types";
import { DrawerSection } from "./drawer";

export function TaskDeliveries({ task, tasks }: { task: Task; tasks: Task[] }) {
    const { t: tr, locale } = useI18n();
    const byID = new Map(tasks.map((t) => [t.id, t]));
    const records = tasks.filter((child) => {
        if (!child.result_delivery) return false;
        const seen = new Set<string>();
        for (let id: string | undefined = child.id; id && !seen.has(id); id = byID.get(id)?.parent) {
            if (id === task.id) return true;
            seen.add(id);
        }
        return false;
    });
    if (!records.length) return null;
    const names = { pending: tr("tasks.handoffPending"), delivered: tr("tasks.handoffDelivered"), suppressed: tr("tasks.handoffSuppressed"), uncertain: tr("tasks.handoffUncertain") };
    return <DrawerSection title={tr("tasks.handoffs")}>
        <ul className="flex flex-col divide-y divide-secondary">
            {records.map((child) => {
                const d = child.result_delivery!;
                const parent = byID.get(child.parent || "");
                return <li key={child.id} className="min-w-0 py-2 text-xs">
                    <div className="flex flex-wrap items-center justify-between gap-2">
                        <span className="font-medium text-secondary">#{child.id} → #{child.parent}</span>
                        <span className={d.state === "uncertain" ? "text-warning-primary" : "text-tertiary"}>{names[d.state]}</span>
                    </div>
                    <p className="mt-1 break-words text-secondary">{child.title || child.goal}</p>
                    {!!d.attempts && <p className="mt-1 text-tertiary">{tr("tasks.handoffAttempts", { count: d.attempts })}</p>}
                    {d.error && <p className="mt-1 break-words text-warning-primary">{d.error}</p>}
                    {d.state === "uncertain" && <p className="mt-1 text-tertiary">{tr("tasks.handoffCheck")}</p>}
                    {d.state === "pending" && <p className="mt-1 text-tertiary">{parent?.lifecycle !== "running" ? tr("tasks.handoffHeld") : d.next_attempt_at ? tr("tasks.handoffRetry", { time: when(d.next_attempt_at, locale) }) : tr("tasks.handoffWaiting")}</p>}
                </li>;
            })}
        </ul>
    </DrawerSection>;
}

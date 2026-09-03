import { Activity } from "@untitledui/icons";
import { when } from "@/lib/api";
import { useFleet } from "@/lib/fleet";
import { Nothing } from "@/lib/ui";

export function ActivityPage() {
    const { events } = useFleet();
    return (
        <div className="p-6">
            <div className="rounded-xl bg-primary shadow-xs ring-1 ring-secondary">
                <div className="border-b border-secondary px-5 py-4">
                    <h2 className="text-md font-semibold text-primary">History</h2>
                    <p className="text-sm text-tertiary">The change stream, newest first.</p>
                </div>
                {events.length === 0 ? <Nothing icon={Activity} title="Quiet">Events appear as steps start, finish and land.</Nothing> : (
                    <ol className="divide-y divide-secondary">
                        {events.map((ev, i) => (
                            <li key={i} className="flex items-baseline gap-3 px-5 py-2 font-mono text-xs">
                                <span className="w-20 shrink-0 text-quaternary">{when(ev.at)}</span>
                                <span className="text-brand-secondary">{ev.kind}</span>
                                <span className="text-secondary">
                                    {ev.plan_id ? `plan ${ev.plan_id}` : ""}{ev.step_id ? ` · ${ev.step_id}` : ""}{ev.state ? ` → ${ev.state}` : ""}
                                </span>
                                {ev.detail && <span className="text-tertiary">{ev.detail}</span>}
                            </li>
                        ))}
                    </ol>
                )}
            </div>
        </div>
    );
}

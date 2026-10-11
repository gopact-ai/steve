import { Clock, AlertCircle, Zap, Link01 } from "@untitledui/icons";
import { Panel } from "@/components/steve/page";
import { AssistantFeedback } from "@/components/steve/assistant-projection";
import { useI18n } from "@/providers/locale-provider";
import { number } from "@/lib/format";
import type { EventFlowSnapshot, ServiceBacklog } from "@/lib/assistant-types";

export function EventBusOverview({ value, error, refresh, flow }: { value: ServiceBacklog | null; error: string; updated: string; refresh: () => void; flow: EventFlowSnapshot | null }) {
    const { t, locale } = useI18n();
    const stages = [
        { id: "event_outbox", label: t("assistant.busRouting"), hint: t("assistant.busRoutingHint"), icon: Link01, queue: flow?.queues.routing },
        { id: "event_pending", label: t("assistant.deliveryPending"), hint: t("assistant.busPendingHint"), icon: Clock, queue: flow?.queues.pending },
        { id: "processing", label: t("assistant.busProcessing"), hint: t("assistant.busProcessingHint"), icon: Zap, queue: flow ? { pending: flow.queues.claimed.pending + flow.queues.accepted.pending } : undefined },
        { id: "event_unknown", label: t("assistant.unknown"), hint: t("assistant.busUnknownHint"), icon: AlertCircle, queue: flow?.queues.unknown },
    ];
    return <section aria-label={t("assistant.busMetrics")} className="flex min-w-0 flex-col gap-3">
        <AssistantFeedback error={error} loading={!value && !error} onRefresh={refresh} />
        {!error ? <div className="bus-dashboard-metrics">{stages.map(({ id, label, hint, icon: Icon, queue }) => <Panel key={id} padding="flush">
            <dl className="bus-metric" data-bus-queue={value && queue ? id : undefined}>
                <dt title={hint}>{label}<Icon aria-hidden="true" className="size-4 text-fg-quaternary" /></dt>
                <dd className={id === "event_unknown" && queue?.pending ? "text-warning-primary" : "text-primary"}>{value && queue ? number(queue.pending, locale) : "—"}</dd>
            </dl>
        </Panel>)}</div> : null}
    </section>;
}

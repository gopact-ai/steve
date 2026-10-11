import { RefreshCw01 } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { Panel } from "@/components/steve/page";
import { AssistantFeedback } from "@/components/steve/assistant-projection";
import { useI18n } from "@/providers/locale-provider";
import { bytes, dateTime, number } from "@/lib/format";
import type { EventFlowSnapshot, ServiceBacklog } from "@/lib/assistant-types";

export function EventBusOverview({ value, error, updated, refresh, flow }: { value: ServiceBacklog | null; error: string; updated: string; refresh: () => void; flow?: EventFlowSnapshot | null }) {
    const { t, locale } = useI18n();
    const stages = [
        { id: "event_outbox", label: t("assistant.busRouting"), hint: t("assistant.busRoutingHint") },
        { id: "event_pending", label: t("assistant.deliveryPending"), hint: t("assistant.busPendingHint") },
        { id: "processing", label: t("assistant.busProcessing"), hint: t("assistant.busProcessingHint") },
        { id: "event_unknown", label: t("assistant.unknown"), hint: t("assistant.busUnknownHint") },
        { id: "event_inbox", label: t("assistant.eventsInbox"), hint: t("assistant.busInboxHint") },
    ];
    const queues: ServiceBacklog["queues"] = { ...value?.queues, ...(flow ? { event_outbox: flow.queues.routing, event_pending: flow.queues.pending, event_claimed: flow.queues.claimed, event_accepted: flow.queues.accepted, event_unknown: flow.queues.unknown } : {}) };
    return <Panel title={t("assistant.busOverview")} description={t("assistant.busDescription")} aside={<Button size="sm" color="secondary" iconLeading={RefreshCw01} onClick={refresh}>{t("assistant.refresh")}</Button>}>
        <AssistantFeedback error={error} loading={!value && !error} onRefresh={refresh} />
        {value ? <>
            <dl className="grid min-w-0 grid-cols-2 gap-x-5 gap-y-4 lg:grid-cols-5">
                {stages.map((stage) => {
                    const count = stage.id === "processing" ? (queues.event_claimed?.pending || 0) + (queues.event_accepted?.pending || 0) : queues[stage.id]?.pending || 0;
                    const oldest = stage.id === "processing" ? [queues.event_claimed?.oldest, queues.event_accepted?.oldest].filter(Boolean).sort()[0] : queues[stage.id]?.oldest;
                    return <div key={stage.id} className="flex min-w-0 flex-col gap-1.5" data-bus-queue={stage.id}>
                        <dt className="text-xs font-medium text-secondary">{stage.label}</dt>
                        <dd className={`text-2xl font-semibold tabular-nums ${stage.id === "event_unknown" && count ? "text-warning-primary" : "text-primary"}`}>{number(count, locale)}</dd>
                        <dd className="text-xs text-tertiary">{stage.hint}</dd>
                        {oldest ? <dd className="break-words text-xs text-tertiary">{t("assistant.busOldest")} {dateTime(oldest, locale, { dateStyle: "short", timeStyle: "medium" })}</dd> : null}
                    </div>;
                })}
            </dl>
            <div className="mt-1 flex min-w-0 flex-wrap items-center justify-between gap-x-5 gap-y-2 border-t border-secondary pt-3 text-xs text-tertiary">
                <p>{t("assistant.busLimits", { total: number(value.limits.events?.total || 0, locale), source: number(value.limits.events?.per_source || 0, locale) })}</p>
                <p>{t("assistant.busStorage", { hot: bytes(value.storage.hot_bytes, locale), cold: bytes(value.storage.cold_bytes, locale) })}</p>
                <p aria-live="polite">{t("assistant.busUpdated")} {dateTime(updated, locale, { hour: "2-digit", minute: "2-digit", second: "2-digit" })}</p>
            </div>
            <p className="text-xs text-tertiary">{t("assistant.busCountsHint")}</p>
            <div className="flex flex-wrap gap-2"><Button size="sm" color="link-color" href="/inbox">{t("assistant.busOpenInbox")}</Button><Button size="sm" color="link-color" href="/connections?section=subscriptions">{t("assistant.busManageRules")}</Button></div>
        </> : null}
    </Panel>;
}

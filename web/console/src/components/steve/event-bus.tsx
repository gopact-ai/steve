import { useMemo } from "react";
import { useSearchParams } from "react-router";
import { Link01 } from "@untitledui/icons";
import { Tabs } from "@/components/application/tabs/tabs";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Panel } from "@/components/steve/page";
import { Nothing } from "@/components/steve/ui";
import { AssistantFeedback, AssistantPager } from "@/components/steve/assistant-projection";
import { EventBusFlow } from "@/components/steve/event-bus-flow";
import { EventBusInspector } from "@/components/steve/event-bus-inspector";
import { DeliveryState } from "@/components/steve/event-bus-state";
import { useAssistantPage } from "@/hooks/use-assistant-page";
import { useBusOverview } from "@/hooks/use-bus-overview";
import { useI18n } from "@/providers/locale-provider";
import { dateTime, number } from "@/lib/format";
import type { EventDelivery, EventJournalEntry, EventProvider } from "@/lib/assistant-types";

export function EventBus() {
    const { t, locale } = useI18n(); const [params, setParams] = useSearchParams();
    const view = params.get("view") === "events" ? "events" : "deliveries";
    const overview = useBusOverview();
    const deliveries = useAssistantPage<EventDelivery>("/console/event-deliveries", 10000, "deliveryCursor", view === "deliveries");
    const events = useAssistantPage<EventJournalEntry>("/console/business-events", 10000, "eventCursor", view === "events");
    const providers = useAssistantPage<EventProvider>("/console/event-providers", 10000, "providerCursor");
    const names = useMemo(() => new Map(providers.page?.items.map((item) => [item.id, item.name])), [providers.page]);
    const resource = view === "events" ? events : deliveries;
    const deliveryID = params.get("delivery") || ""; const eventID = params.get("event") || "";
    const detailLink = (key: "delivery" | "event", id: string) => { const next = new URLSearchParams(params); next.delete("delivery"); next.delete("event"); next.set(key, id); return `/connections?${next}`; };
    const close = () => setParams((before) => { const next = new URLSearchParams(before); next.delete("delivery"); next.delete("event"); return next; });
    return <>
        <EventBusFlow overview={overview} refreshOverview={() => { void overview.refresh(); resource.refresh(); providers.refresh(); }} />
        <Panel title={t("assistant.busRecords")} description={t("assistant.busRecordsHint")}
            badge={resource.page && !resource.error ? <Badge type="pill-color" size="sm">{t("assistant.busPageCount", { count: number(resource.page.items.length, locale) })}</Badge> : null}
            toolbar={<Tabs selectedKey={view} onSelectionChange={(key) => setParams((before) => { const next = new URLSearchParams(before); next.set("view", String(key)); next.delete("delivery"); next.delete("event"); return next; })}><Tabs.List aria-label={t("assistant.busRecords")} type="button-border" size="sm" items={[{ id: "deliveries", label: t("assistant.busDeliveries") }, { id: "events", label: t("assistant.busEvents") }]}>{(item) => <Tabs.Item {...item} />}</Tabs.List></Tabs>}
            footer={<AssistantPager next={resource.page?.next || ""} cursor={resource.cursor} onPage={resource.setCursor} disabled={!!resource.error} />}>
            <AssistantFeedback error={resource.error} loading={resource.loading} onRefresh={resource.refresh} />
            {!resource.error && resource.page?.items.length === 0 ? <Nothing icon={Link01} title={t(view === "events" ? "assistant.busEmptyEvents" : "assistant.busEmptyDeliveries")}>{t("assistant.busEmptyHint")}</Nothing> : null}
            {!resource.error && view === "deliveries" ? <ul aria-label={t("assistant.busDeliveries")} className="divide-y divide-secondary">{deliveries.page?.items.map((delivery) => <li key={delivery.id} className="grid min-w-0 gap-3 py-4 first:pt-0 last:pb-0 md:grid-cols-[minmax(0,2fr)_minmax(0,2fr)_minmax(8rem,1fr)_auto] [content-visibility:auto]">
                <div className="min-w-0"><h3 className="break-words text-sm font-semibold text-primary">{delivery.rule.name}</h3><p className="mt-1 break-all font-mono text-xs text-tertiary" translate="no">{delivery.rule.event_type} · v{delivery.rule.revision}</p><p className="mt-1 truncate font-mono text-xs text-quaternary" title={delivery.id} translate="no">{delivery.id}</p></div>
                <div className="min-w-0"><p className="text-xs text-tertiary">{t("assistant.busTarget")}</p><p className="mt-1 break-all text-sm text-secondary">{delivery.rule.mode === "provider" ? names.get(delivery.rule.handler || "") || delivery.rule.handler : delivery.rule.mode === "work" ? delivery.rule.conversation : t("assistant.manual")}</p>{delivery.rule.project ? <p className="mt-1 break-all text-xs text-tertiary">{delivery.rule.project}</p> : null}</div>
                <div className="flex min-w-0 flex-col items-start gap-2"><DeliveryState state={delivery.state} mode={delivery.rule.mode} /><p className="break-words text-xs text-tertiary">{dateTime(delivery.updated_at, locale, { dateStyle: "short", timeStyle: "medium" })}</p>{delivery.detail ? <p className="line-clamp-2 break-words text-xs text-secondary">{delivery.detail}</p> : null}</div>
                <div><Button size="sm" color="secondary" href={detailLink("delivery", delivery.id)}>{t("assistant.inspect")}</Button></div>
            </li>)}</ul> : null}
            {!resource.error && view === "events" ? <ul aria-label={t("assistant.busEvents")} className="divide-y divide-secondary">{events.page?.items.map((event) => <li key={event.id} className="grid min-w-0 gap-3 py-4 first:pt-0 last:pb-0 md:grid-cols-[minmax(0,2fr)_minmax(0,2fr)_minmax(8rem,1fr)_auto] [content-visibility:auto]">
                <div className="min-w-0"><h3 className="break-all font-mono text-sm font-semibold text-primary" translate="no">{event.type}</h3>{event.subject ? <p className="mt-1 line-clamp-2 break-words text-sm text-secondary">{event.subject}</p> : null}<p className="mt-1 truncate font-mono text-xs text-quaternary" title={event.id} translate="no">{event.id}</p></div>
                <div className="min-w-0"><p className="text-xs text-tertiary">{t("assistant.origin")}</p><p className="mt-1 break-all text-sm text-secondary">{names.get(event.source.replace(/^connection:/, "")) || event.source}</p></div>
                <div className="flex min-w-0 flex-col items-start gap-2"><Badge type="pill-color" color={event.routed ? "gray" : "brand"} size="sm">{t(event.routed ? "assistant.busRouted" : "assistant.busRouting")}</Badge><p className="break-words text-xs text-tertiary">{dateTime(event.at, locale, { dateStyle: "short", timeStyle: "medium" })}</p></div>
                <div><Button size="sm" color="secondary" href={detailLink("event", event.id)}>{t("assistant.inspect")}</Button></div>
            </li>)}</ul> : null}
        </Panel>
        <Panel title={t("assistant.busProviders")} description={t("assistant.busProvidersHint")} footer={<AssistantPager next={providers.page?.next || ""} cursor={providers.cursor} onPage={providers.setCursor} disabled={!!providers.error} />}>
            <AssistantFeedback error={providers.error} loading={providers.loading} onRefresh={providers.refresh} />
            {!providers.error && providers.page?.items.length === 0 ? <p className="text-sm text-tertiary">{t("assistant.busEmptyProviders")}</p> : null}
            {!providers.error ? <ul className="divide-y divide-secondary">{providers.page?.items.map((provider) => <li key={provider.id} className="flex min-w-0 flex-wrap items-start justify-between gap-3 py-3 first:pt-0 last:pb-0"><div className="min-w-0 flex-1"><h3 className="break-words text-sm font-semibold">{provider.name}</h3><p className="mt-1 break-all text-xs text-tertiary" translate="no">{provider.plugin_id ? `${provider.plugin_id} · ${provider.version}` : provider.principal}</p></div><Badge type="pill-color" color={provider.state === "online" ? "success" : provider.state === "offline" ? "warning" : "gray"} size="sm">{t(provider.state === "online" ? "assistant.online" : provider.state === "offline" ? "assistant.offline" : provider.state === "retired" ? "assistant.busRetired" : "assistant.busUnregistered")}</Badge></li>)}</ul> : null}
        </Panel>
        {deliveryID || eventID ? <EventBusInspector key={deliveryID || eventID} deliveryID={deliveryID} eventID={eventID} onClose={close} /> : null}
    </>;
}

import { useMemo } from "react";
import { useSearchParams } from "react-router";
import { Link01, ArrowUpRight } from "@untitledui/icons";
import { Tabs } from "@/components/application/tabs/tabs";
import { Table } from "@/components/application/table/table";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Panel } from "@/components/steve/page";
import { Nothing } from "@/components/steve/ui";
import { AssistantFeedback, AssistantPager } from "@/components/steve/assistant-projection";
import { EventBusFlow } from "@/components/steve/event-bus-flow";
import { EventBusInspector } from "@/components/steve/event-bus-inspector";
import { DeliveryState } from "@/components/steve/event-bus-state";
import { useAssistantPage } from "@/hooks/use-assistant-page";
import { useBreakpoint } from "@/hooks/use-breakpoint";
import { useBusOverview } from "@/hooks/use-bus-overview";
import { useI18n } from "@/providers/locale-provider";
import { bytes, dateTime, number } from "@/lib/format";
import type { EventDelivery, EventJournalEntry, EventProvider } from "@/lib/assistant-types";

export function EventBus() {
    const { t, locale } = useI18n(); const [params, setParams] = useSearchParams();
    const view = params.get("view") === "events" ? "events" : "deliveries";
    const overview = useBusOverview();
    const compact = !useBreakpoint("lg");
    const deliveries = useAssistantPage<EventDelivery>("/console/event-deliveries", 10000, "deliveryCursor", view === "deliveries");
    const events = useAssistantPage<EventJournalEntry>("/console/business-events", 10000, "eventCursor", view === "events");
    const providers = useAssistantPage<EventProvider>("/console/event-providers", 10000, "providerCursor");
    const names = useMemo(() => new Map(providers.page?.items.map((item) => [item.id, item.name])), [providers.page]);
    const resource = view === "events" ? events : deliveries;
    const deliveryID = params.get("delivery") || ""; const eventID = params.get("event") || "";
    const detailLink = (key: "delivery" | "event", id: string) => { const next = new URLSearchParams(params); next.delete("delivery"); next.delete("event"); next.set(key, id); return `/connections?${next}`; };
    const close = () => setParams((before) => { const next = new URLSearchParams(before); next.delete("delivery"); next.delete("event"); return next; });
    const fullTime = (at: string) => dateTime(at, locale, { dateStyle: "short", timeStyle: "medium" });
    const recordTime = (at: string) => <time dateTime={at} title={fullTime(at)} className="whitespace-nowrap text-xs tabular-nums">{fullTime(at)}</time>;
    const capacity = overview.value;
    const providerPanel = <Panel title={t("assistant.busProviders")} padding="flush"
        badge={providers.page && !providers.error ? <Badge size="sm" type="pill-color">{number(providers.page.items.length, locale)}</Badge> : null}
        footer={<>
            <AssistantPager next={providers.page?.next || ""} cursor={providers.cursor} onPage={providers.setCursor} disabled={!!providers.error} />
            {capacity && !overview.error ? <details className="bus-capacity"><summary>{t("assistant.busCapacity")}</summary><dl>
                <dt>{t("assistant.eventsInbox")}</dt><dd><a className="underline outline-focus-ring focus-visible:outline-2" href="#/inbox">{number(capacity.queues.event_inbox?.pending || 0, locale)}</a></dd>
                <dt>{t("assistant.busLimitTotal")}</dt><dd>{number(capacity.limits.events?.total || 0, locale)}</dd>
                <dt>{t("assistant.busLimitSource")}</dt><dd>{number(capacity.limits.events?.per_source || 0, locale)}</dd>
                <dt>{t("assistant.busHotStorage")}</dt><dd>{bytes(capacity.storage.hot_bytes, locale)}</dd>
                <dt>{t("assistant.busColdStorage")}</dt><dd>{bytes(capacity.storage.cold_bytes, locale)}</dd>
                <dt>{t("assistant.busUpdated")}</dt><dd>{dateTime(overview.updated, locale, { hour: "2-digit", minute: "2-digit", second: "2-digit" })}</dd>
            </dl></details> : null}
        </>}>
        <div className="px-4 py-2"><AssistantFeedback error={providers.error} loading={providers.loading} onRefresh={providers.refresh} />
            {!providers.error && providers.page?.items.length === 0 ? <p className="py-3 text-sm text-tertiary">{t("assistant.busEmptyProviders")}</p> : null}
            {!providers.error ? <ul className="bus-provider-list divide-y divide-secondary">{providers.page?.items.map((provider) => <li key={provider.id} className="flex min-w-0 items-center gap-3 py-3">
                <div className="flex size-8 shrink-0 items-center justify-center rounded-lg border border-secondary bg-secondary text-tertiary"><Link01 aria-hidden="true" className="size-4" /></div>
                <div className="min-w-0 flex-1"><h3 title={provider.name} className="truncate text-xs font-semibold text-primary">{provider.name}</h3><p title={provider.plugin_id ? `${provider.plugin_id} · ${provider.version}` : provider.principal} className="mt-1 truncate text-xs text-quaternary" translate="no">{provider.plugin_id || provider.principal}</p></div>
                <Badge type="pill-color" color={provider.state === "online" ? "success" : provider.state === "offline" ? "warning" : "gray"} size="sm">{t(provider.state === "online" ? "assistant.online" : provider.state === "offline" ? "assistant.offline" : provider.state === "retired" ? "assistant.busRetired" : "assistant.busUnregistered")}</Badge>
            </li>)}</ul> : null}
        </div>
    </Panel>;
    return <>
        <EventBusFlow overview={overview} refreshOverview={() => { void overview.refresh(); resource.refresh(); providers.refresh(); }} providers={providerPanel} />
        <Panel title={t("assistant.busRecords")} padding="flush"
            badge={resource.page && !resource.error ? <Badge type="pill-color" size="sm">{t("assistant.busPageCount", { count: number(resource.page.items.length, locale) })}</Badge> : null}
            toolbar={<Tabs selectedKey={view} onSelectionChange={(key) => setParams((before) => { const next = new URLSearchParams(before); next.set("view", String(key)); next.delete("delivery"); next.delete("event"); return next; })}><Tabs.List aria-label={t("assistant.busRecords")} type="button-border" size="sm" items={[{ id: "deliveries", label: t("assistant.busDeliveries") }, { id: "events", label: t("assistant.busEvents") }]}>{(item) => <Tabs.Item {...item} />}</Tabs.List></Tabs>}
            footer={<AssistantPager next={resource.page?.next || ""} cursor={resource.cursor} onPage={resource.setCursor} disabled={!!resource.error} />}>
            {resource.error || resource.loading ? <div className="p-4"><AssistantFeedback error={resource.error} loading={resource.loading} onRefresh={resource.refresh} /></div> : null}
            {!resource.error && resource.page?.items.length === 0 ? <div className="p-4"><Nothing icon={Link01} title={t(view === "events" ? "assistant.busEmptyEvents" : "assistant.busEmptyDeliveries")}>{t("assistant.busEmptyHint")}</Nothing></div> : null}
            {!resource.error && view === "deliveries" && deliveries.page?.items.length ? <div className="bus-record-scroll"><Table aria-label={t("assistant.busDeliveries")} selectionMode="none" className={`bus-record-table table-fixed ${compact ? "" : "min-w-200"}`}>
                <Table.Header size="sm"><Table.Head id="event" isRowHeader label={t("assistant.busRecordEvent")} className="bus-table-event" />{!compact ? <Table.Head id="target" label={t("assistant.busTarget")} className="bus-table-target w-[22%]" /> : null}<Table.Head id="state" label={t("assistant.busRecordState")} className="bus-table-state w-48 whitespace-nowrap" />{!compact ? <Table.Head id="time" label={t("assistant.busRecordTime")} className="bus-table-time w-48 whitespace-nowrap" /> : null}<Table.Head id="inspect" aria-label={t("assistant.inspect")} className="w-16" /></Table.Header>
                <Table.Body dependencies={[compact, names, params, locale]} items={deliveries.page.items}>{(delivery) => <Table.Row className="after:hidden" id={delivery.id} href={detailLink("delivery", delivery.id)} size="sm">
                    <Table.Cell size="sm" textValue={delivery.rule.name}><div title={delivery.rule.name} className="truncate text-sm font-medium text-primary">{delivery.rule.name}</div><p title={delivery.rule.event_type} className="mt-1 truncate font-mono text-xs text-tertiary" translate="no">{delivery.rule.event_type} · v{delivery.rule.revision}</p><span className="sr-only" translate="no">{delivery.id}</span></Table.Cell>
                    {!compact ? <Table.Cell size="sm" className="bus-table-target"><p className="truncate text-xs text-secondary" title={delivery.rule.handler || delivery.rule.conversation}>{delivery.rule.mode === "provider" ? names.get(delivery.rule.handler || "") || delivery.rule.handler : delivery.rule.mode === "work" ? delivery.rule.conversation : t("assistant.manual")}</p>{delivery.rule.project ? <p title={delivery.rule.project} className="mt-1 truncate text-xs text-quaternary">{delivery.rule.project}</p> : null}</Table.Cell> : null}
                    <Table.Cell size="sm" className="bus-table-state"><DeliveryState state={delivery.state} mode={delivery.rule.mode} className="max-w-full whitespace-normal break-words" /></Table.Cell>
                    {!compact ? <Table.Cell size="sm" className="bus-table-time">{recordTime(delivery.updated_at)}</Table.Cell> : null}
                    <Table.Cell size="sm"><Button size="sm" color="tertiary" iconLeading={ArrowUpRight} href={detailLink("delivery", delivery.id)} aria-label={t("assistant.inspect")} /></Table.Cell>
                </Table.Row>}</Table.Body>
            </Table></div> : null}
            {!resource.error && view === "events" && events.page?.items.length ? <div className="bus-record-scroll"><Table aria-label={t("assistant.busEvents")} selectionMode="none" className={`bus-record-table table-fixed ${compact ? "" : "min-w-200"}`}>
                <Table.Header size="sm"><Table.Head id="event" isRowHeader label={t("assistant.busRecordEvent")} className="bus-table-event" />{!compact ? <Table.Head id="source" label={t("assistant.busRecordSource")} className="bus-table-target w-[22%]" /> : null}<Table.Head id="state" label={t("assistant.busRecordState")} className="bus-table-state w-48 whitespace-nowrap" />{!compact ? <Table.Head id="time" label={t("assistant.busRecordTime")} className="bus-table-time w-48 whitespace-nowrap" /> : null}<Table.Head id="inspect" aria-label={t("assistant.inspect")} className="w-16" /></Table.Header>
                <Table.Body dependencies={[compact, names, params, locale]} items={events.page.items}>{(event) => <Table.Row className="after:hidden" id={event.id} href={detailLink("event", event.id)} size="sm">
                    <Table.Cell size="sm" textValue={event.type}><p title={event.type} className="truncate font-mono text-sm font-medium text-primary" translate="no">{event.type}</p>{event.subject ? <p title={event.subject} className="mt-1 truncate text-xs text-tertiary">{event.subject}</p> : null}<span className="sr-only" translate="no">{event.id}</span></Table.Cell>
                    {!compact ? <Table.Cell size="sm" className="bus-table-target"><p className="truncate text-xs text-secondary" title={event.source}>{names.get(event.source) || event.source}</p></Table.Cell> : null}
                    <Table.Cell size="sm" className="bus-table-state"><Badge type="pill-color" color={event.routed ? "success" : "gray"} size="sm">{t(event.routed ? "assistant.busRouted" : "assistant.busRouting")}</Badge></Table.Cell>
                    {!compact ? <Table.Cell size="sm" className="bus-table-time">{recordTime(event.at)}</Table.Cell> : null}
                    <Table.Cell size="sm"><Button size="sm" color="tertiary" iconLeading={ArrowUpRight} href={detailLink("event", event.id)} aria-label={t("assistant.inspect")} /></Table.Cell>
                </Table.Row>}</Table.Body>
            </Table></div> : null}
        </Panel>
        {deliveryID || eventID ? <EventBusInspector key={deliveryID || eventID} deliveryID={deliveryID} eventID={eventID} onClose={close} /> : null}
    </>;
}

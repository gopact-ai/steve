import { Link } from "react-router";
import { ArrowUpRight, ArrowRight, CheckCircle, AlertCircle } from "@untitledui/icons";
import { Tabs } from "@/components/application/tabs/tabs";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Panel } from "@/components/steve/page";
import { EventDefinition } from "@/components/steve/event-definition";
import { eventDefinitionText } from "@/lib/event-definition";
import { DeliveryState } from "@/components/steve/event-bus-state";
import { AssistantFeedback } from "@/components/steve/assistant-projection";
import type { EventActivitySnapshot } from "@/lib/assistant-types";
import { useI18n } from "@/providers/locale-provider";
import { dateTime, number, relative } from "@/lib/format";
import { eventActivityGroups } from "@/lib/event-activity-groups";

export function EventBusActivity({ projection, attention, onTab, names, detailLink, historyLink }: { projection: { value: EventActivitySnapshot | null; error: string; refresh: () => void }; attention: boolean; onTab: (attention: boolean) => void; names: Map<string, string>; detailLink: (key: "delivery" | "event", id: string) => string; historyLink: string }) {
    const { t, locale } = useI18n();
    const { value, error, refresh } = projection;
    const groups = eventActivityGroups(value?.recent || []);
    const rows = groups.slice(0, 4);
    const stamp = (at: string) => <time dateTime={at} title={dateTime(at, locale, { dateStyle: "short", timeStyle: "medium" })} className="bus-activity-time">{relative(at, locale)}</time>;
    return <Panel title={t("assistant.busActivity")} padding="flush" aside={<Button size="sm" color="link-color" iconTrailing={ArrowRight} href={historyLink}>{t("assistant.busViewAll")}</Button>}
        toolbar={<Tabs selectedKey={attention ? "attention" : "recent"} onSelectionChange={(key) => onTab(key === "attention")}><Tabs.List aria-label={t("assistant.busActivity")} type="button-gray" size="sm"><Tabs.Item id="recent">{t("assistant.busRecentEvents")}</Tabs.Item><Tabs.Item id="attention">{t("assistant.busAttention")}{value?.attention.length ? <Badge size="sm" type="pill-color" color="warning">{number(value.attention.length, locale)}{value.more_attention ? "+" : ""}</Badge> : null}</Tabs.Item></Tabs.List></Tabs>}
        footer={value && !error ? <div className="flex min-w-0 flex-wrap items-center justify-between gap-2 text-xs text-quaternary"><span>{t(attention ? "assistant.busAttentionScope" : "assistant.busRecentScope", { count: number(value.recent.length, locale) })}</span>{attention ? <Button size="sm" color="link-color" href="/inbox">{t("assistant.busOpenInbox")}</Button> : groups.length > 4 ? <span>{t("assistant.busMoreGroups", { count: number(groups.length - 4, locale) })}</span> : null}</div> : undefined}>
        {error || !value ? <div className="p-4"><AssistantFeedback error={error} loading={!value && !error} onRefresh={() => { void refresh(); }} /></div> : attention ? value.attention.length ? <ul className="bus-activity-list">{value.attention.map((item) => <li key={item.id}><Link className="bus-activity-row" to={detailLink("delivery", item.id)}>
            <AlertCircle aria-hidden="true" className={`bus-activity-icon ${item.state === "failed" ? "text-error-primary" : "text-warning-primary"}`} />
            <div className="bus-activity-content"><p className="bus-activity-title" title={item.subject || eventDefinitionText(item.contract, locale).name || item.type}>{item.subject || eventDefinitionText(item.contract, locale).name || item.type}</p><p className="bus-activity-meta"><span title={item.source}>{names.get(item.source) || item.source}</span><span aria-hidden="true">·</span><span title={item.rule}>{item.rule}</span></p><EventDefinition type={item.type} definition={item.contract} compact showName={!!item.subject} showType={!!item.subject || !!eventDefinitionText(item.contract, locale).name} />{item.detail ? <p className="bus-activity-detail" title={item.detail}>{item.detail}</p> : null}</div>
            <div className="bus-activity-state"><DeliveryState state={item.state} mode={item.mode} />{stamp(item.at)}</div><ArrowUpRight className="bus-activity-arrow" aria-hidden="true" />
        </Link></li>)}</ul> : <div className="bus-activity-empty"><CheckCircle aria-hidden="true" className="size-5 text-fg-quaternary" /><p>{t("assistant.busNoAttention")}</p></div> : rows.length ? <ul className="bus-activity-list">{rows.map(({ event, count }, index) => <li key={JSON.stringify([event.source, event.type, event.subject, index])}><Link className="bus-activity-row" to={detailLink("event", event.id)}>
            <span className="bus-activity-mark" aria-hidden="true" />
            <div className="bus-activity-content"><div className="bus-activity-heading"><p className="bus-activity-title" title={event.subject || eventDefinitionText(event.contract, locale).name || event.type}>{event.subject || eventDefinitionText(event.contract, locale).name || event.type}</p>{count > 1 ? <span className="bus-activity-repeat" title={t("assistant.busRepeated", { count: number(count, locale) })}>×{number(count, locale)}</span> : null}</div><p className="bus-activity-meta"><span title={event.source}>{names.get(event.source) || event.source}</span></p><EventDefinition type={event.type} definition={event.contract} compact showName={!!event.subject} showType={!!event.subject || !!eventDefinitionText(event.contract, locale).name} /></div>
            <div className="bus-activity-state"><span className={event.routed ? "text-tertiary" : "text-secondary"}>{t(event.routed ? "assistant.busRouted" : "assistant.busRouting")}</span>{stamp(event.at)}</div><ArrowUpRight className="bus-activity-arrow" aria-hidden="true" />
        </Link></li>)}</ul> : <div className="bus-activity-empty"><p>{t("assistant.busEmptyEvents")}</p></div>}
    </Panel>;
}

import { useEffect, useState } from "react";
import { Button } from "@/components/base/buttons/button";
import { Drawer, DrawerSection } from "@/components/steve/drawer";
import { KeyValue } from "@/components/steve/page";
import { CodeBlock } from "@/components/steve/code-block";
import { DeliveryState } from "@/components/steve/event-bus-state";
import { AssistantFeedback } from "@/components/steve/assistant-projection";
import { fetchBusinessEvent, fetchEventDelivery } from "@/lib/api/assistant";
import { dateTime } from "@/lib/format";
import { eventDefinitionText } from "@/lib/event-definition";
import { message } from "@/lib/http";
import { useI18n } from "@/providers/locale-provider";
import type { BusinessEvent, EventDelivery } from "@/lib/assistant-types";

export function EventBusInspector({ deliveryID, eventID, onClose }: { deliveryID: string; eventID: string; onClose: () => void }) {
    const { t, locale } = useI18n();
    const [value, setValue] = useState<{ event: BusinessEvent; delivery?: EventDelivery } | null>(null);
    const [error, setError] = useState(""); const [version, setVersion] = useState(0);
    useEffect(() => {
        const controller = new AbortController(); let active = true;
        setValue(null); setError("");
        const load = async () => {
            try {
                const delivery = deliveryID ? await fetchEventDelivery(deliveryID, controller.signal) : undefined;
                const event = await fetchBusinessEvent(delivery?.event_id || eventID, controller.signal);
                if (active) setValue({ event, delivery });
            } catch (cause) { if (active && !controller.signal.aborted) setError(message(cause)); }
        };
        void load();
        return () => { active = false; controller.abort(); };
    }, [deliveryID, eventID, version]);
    const delivery = value?.delivery;
    const definition = eventDefinitionText(value?.event.contract, locale);
    return <Drawer title={t(deliveryID ? "assistant.busDeliveryDetail" : "assistant.eventDetail")} subtitle={<span className="font-mono" translate="no">{deliveryID || eventID}</span>} onClose={onClose}>
        <AssistantFeedback error={error} loading={!value && !error} onRefresh={() => setVersion((before) => before + 1)} />
        {value ? <>
            {delivery ? <DrawerSection title={t("assistant.busConsumerEvidence")} aside={<DeliveryState state={delivery.state} mode={delivery.rule.mode} />}>
                <KeyValue rows={[
                    { k: t("assistant.busUpdated"), v: dateTime(delivery.updated_at, locale, { dateStyle: "short", timeStyle: "medium" }) },
                    { k: t("assistant.receipt"), v: delivery.receipt || t("assistant.busNoReceipt") },
                    ...(delivery.claim_until ? [{ k: t("assistant.busClaimUntil"), v: dateTime(delivery.claim_until, locale, { dateStyle: "short", timeStyle: "medium" }) }] : []),
                    ...(delivery.claim_token ? [{ k: t("assistant.busClaimToken"), v: <code translate="no">{delivery.claim_token}</code> }] : []),
                ]} />
                {delivery.state === "accepted" ? <p className="mt-3 text-xs text-tertiary">{t("assistant.busAcceptedHint")}</p> : null}
                {delivery.detail ? <p className="mt-3 whitespace-pre-wrap break-words text-sm text-secondary">{delivery.detail}</p> : null}
                {delivery.resolution ? <CodeBlock code={JSON.stringify(delivery.resolution, null, 2)} lang="json" label={t("assistant.busReconciliation")} maxHeight={240} /> : null}
            </DrawerSection> : null}
            <DrawerSection title={t("assistant.busOriginalEvent")}>
                <KeyValue rows={[
                    { k: t("assistant.busEventID"), v: <code translate="no">{value.event.id}</code> },
                    { k: t("assistant.eventType"), v: <code translate="no">{value.event.type}</code> },
                    ...(definition.name ? [{ k: t("assistant.name"), v: definition.name }] : []),
                    ...(definition.description ? [{ k: t("assistant.eventDescription"), v: <span className="whitespace-pre-wrap break-words [overflow-wrap:anywhere]">{definition.description}</span> }] : []),
                    { k: t("assistant.origin"), v: <code translate="no">{value.event.source}</code> },
                    { k: t("assistant.at"), v: dateTime(value.event.at, locale, { dateStyle: "short", timeStyle: "medium" }) },
                    { k: t("assistant.busProducerKey"), v: <code translate="no">{value.event.key}</code> },
                    ...(value.event.subject ? [{ k: t("assistant.busSubject"), v: value.event.subject }] : []),
                    ...(value.event.causation ? [{ k: t("assistant.busCausation"), v: <code translate="no">{value.event.causation}</code> }] : []),
                ]} />
                <CodeBlock code={JSON.stringify(value.event.payload, null, 2)} lang="json" label={t("assistant.payload")} maxHeight={360} />
            </DrawerSection>
            {delivery ? <DrawerSection title={t("assistant.busFrozenRule")}>
                <p className="mb-3 text-xs text-tertiary">{t("assistant.busFrozenHint")}</p>
                <KeyValue rows={[
                    { k: t("assistant.name"), v: delivery.rule.name },
                    { k: t("assistant.ruleID"), v: <code translate="no">{delivery.rule.id}</code> },
                    { k: t("assistant.busRuleRevision"), v: delivery.rule.revision },
                    { k: t("assistant.eventType"), v: <code translate="no">{delivery.rule.event_type}</code> },
                    { k: t("assistant.mode"), v: t(delivery.rule.mode === "work" ? "assistant.work" : delivery.rule.mode === "provider" ? "assistant.provider" : "assistant.manual") },
                    { k: t("assistant.busTarget"), v: delivery.rule.mode === "provider" ? delivery.rule.handler : delivery.rule.mode === "work" ? delivery.rule.conversation : t("assistant.manual") },
                    ...(delivery.rule.project ? [{ k: t("assistant.project"), v: delivery.rule.project }] : []),
                ]} />
                {delivery.rule.instruction ? <p className="mt-3 whitespace-pre-wrap break-words text-sm text-secondary">{delivery.rule.instruction}</p> : null}
            </DrawerSection> : null}
            <div className="flex flex-wrap gap-2"><Button size="sm" color="secondary" href={`/inbox?event=${encodeURIComponent(value.event.id)}`}>{t("assistant.busOpenInbox")}</Button><Button size="sm" color="secondary" href="/connections?section=subscriptions">{t("assistant.busManageRules")}</Button></div>
        </> : null}
    </Drawer>;
}

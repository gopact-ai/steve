import type { ReactNode } from "react";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Panel } from "@/components/steve/page";
import { CodeBlock } from "@/components/steve/code-block";
import { useI18n } from "@/providers/locale-provider";
import { when } from "@/lib/format";
import type { ClientInvocation, EventInbox } from "@/lib/assistant-types";

export function EventInboxPanels({ entries, calls, eventsReady, callsReady, busy, disabled, eventsFeedback, callsFeedback, eventsFooter, callsFooter, onInspect, onResolve, onRetry, onApproval }: {
    entries: EventInbox[]; calls: ClientInvocation[]; eventsReady: boolean; callsReady: boolean; busy: string; disabled: boolean;
    eventsFeedback: ReactNode; callsFeedback: ReactNode; eventsFooter: ReactNode; callsFooter: ReactNode;
    onInspect: (entry: EventInbox) => void; onResolve: (entry: EventInbox) => void; onRetry: (entry: EventInbox) => void; onApproval: (call: ClientInvocation, allow: boolean) => void;
}) {
    const { t, locale } = useI18n();
    return <>
        <Panel title={t("assistant.approveTitle")} description={t("assistant.approveHint")} footer={callsFooter}>
            {callsFeedback}
            {callsReady && calls.length === 0 ? <p className="text-sm text-tertiary">{t("assistant.emptyApprovals")}</p> : <ul className="divide-y divide-secondary">{calls.map((call) => <li key={call.id} className="flex min-w-0 flex-col gap-3 py-4 first:pt-0 last:pb-0 [content-visibility:auto]">
                <div className="flex min-w-0 flex-wrap items-start justify-between gap-3"><div className="min-w-0 flex-1"><div className="flex flex-wrap items-center gap-2"><h3 className="break-all text-sm font-semibold text-primary" translate="no">{call.request.capability}</h3><Badge type="pill-color" color="warning" size="sm">{t(call.state === "unknown" ? "assistant.unknown" : "assistant.clientApproval")}</Badge></div><p className="mt-1 break-all text-xs text-tertiary">{call.request.client} → <a className="underline outline-focus-ring focus-visible:outline-2" href={`#/console?conversation=${encodeURIComponent(call.request.conversation)}`}>{call.request.conversation}</a></p></div>{call.state === "approval" ? <div className="flex flex-wrap gap-2"><Button size="sm" color="secondary" isDisabled={disabled || !!busy} onClick={() => onApproval(call, false)}>{t("assistant.reject")}</Button><Button size="sm" isLoading={busy === call.id} isDisabled={disabled || !!busy} onClick={() => onApproval(call, true)}>{t("assistant.approve")}</Button></div> : null}</div>
                <CodeBlock code={JSON.stringify(call.request.arguments, null, 2)} lang="json" label={t("assistant.json")} maxHeight={180} />
                {call.error ? <p className="break-words text-sm text-error-primary">{call.error}</p> : null}
                {call.receipt ? <p className="break-all text-xs text-secondary">{t("assistant.receipt")} · {call.receipt}</p> : null}
                <p className="break-all text-xs text-tertiary" translate="no">{when(call.at, locale)} · {call.id}</p>
            </li>)}</ul>}
        </Panel>
        <Panel title={t("assistant.eventsInbox")} description={t("assistant.eventsHint")} footer={eventsFooter}>
            {eventsFeedback}
            {eventsReady && entries.length === 0 ? <p className="text-sm text-tertiary">{t("assistant.emptyEvents")}</p> : <ul className="divide-y divide-secondary">{entries.map((entry) => <li key={entry.id} className="flex min-w-0 flex-col gap-3 py-4 first:pt-0 last:pb-0 [content-visibility:auto]">
                <div className="flex min-w-0 flex-wrap items-start justify-between gap-3"><div className="min-w-0 flex-1"><div className="flex flex-wrap items-center gap-2"><Badge type="pill-color" size="sm" color={entry.reason === "failed" ? "error" : "warning"}>{t(entry.reason === "failed" ? "assistant.failed" : entry.reason === "unknown" ? "assistant.unknown" : entry.reason === "subscription" ? "assistant.subscription" : "assistant.unhandled")}</Badge><span className="text-xs text-tertiary">{when(entry.at, locale)}</span></div>{entry.detail ? <p className="mt-2 break-words text-sm text-secondary">{entry.detail}</p> : null}<p className="mt-2 break-all text-xs text-tertiary" translate="no">{entry.event_id}{entry.delivery_id ? ` → ${entry.delivery_id}` : ""}</p></div><div className="flex flex-wrap gap-2"><Button size="sm" color="secondary" isDisabled={disabled || !!busy} onClick={() => onInspect(entry)}>{t("assistant.inspect")}</Button><Button size="sm" color="secondary" isLoading={busy === entry.id} isDisabled={disabled || !!busy} onClick={() => onResolve(entry)}>{t("assistant.resolve")}</Button>{entry.delivery_id && ["unknown", "failed"].includes(entry.reason) ? <Button size="sm" color="secondary-destructive" isDisabled={disabled || !!busy} onClick={() => onRetry(entry)}>{t("assistant.retry")}</Button> : null}</div></div>
            </li>)}</ul>}
        </Panel>
    </>;
}

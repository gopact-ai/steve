import { useMemo, useState } from "react";
import { useSearchParams } from "react-router";
import { Link01, Plus, RefreshCw01 } from "@untitledui/icons";
import { Tabs } from "@/components/application/tabs/tabs";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { PageBody, PageHeader, Panel, Chips } from "@/components/steve/page";
import { Drawer } from "@/components/steve/drawer";
import { Nothing } from "@/components/steve/ui";
import { ConnectionEditor } from "@/components/steve/connection-editor";
import { SubscriptionEditor } from "@/components/steve/subscription-editor";
import { EventBus } from "@/components/steve/event-bus";
import { AssistantFeedback, AssistantPager } from "@/components/steve/assistant-projection";
import { useAssistantPage } from "@/hooks/use-assistant-page";
import { useI18n } from "@/providers/locale-provider";
import { useFleet } from "@/lib/fleet";
import { message } from "@/lib/http";
import { when } from "@/lib/format";
import { enrollConnection, revokeConnection, saveSubscription } from "@/lib/api/assistant";
import type { ClientRegistration, Connection, EventProvider, Subscription } from "@/lib/assistant-types";

export function ConnectionsPage() {
    const { t } = useI18n(); const [params, setParams] = useSearchParams();
    const section = ["bus", "connections", "subscriptions", "clients"].includes(params.get("section") || "") ? params.get("section")! : "connections";
    return <div className="workbench-page flex min-w-0 flex-col">
        <PageHeader title={t("assistant.title")} description={section === "bus" ? undefined : t("assistant.description")}>
            <Tabs selectedKey={section} onSelectionChange={(key) => setParams({ section: String(key) })}>
                <Tabs.List type="underline" size="sm" aria-label={t("assistant.title")} className="flex-wrap" items={[{ id: "bus", label: t("assistant.bus") }, { id: "connections", label: t("assistant.connections") }, { id: "subscriptions", label: t("assistant.subscriptions") }, { id: "clients", label: t("assistant.clients") }]}>{(item) => <Tabs.Item {...item} />}</Tabs.List>
            </Tabs>
        </PageHeader>
        <PageBody>{section === "bus" ? <EventBus /> : section === "subscriptions" ? <Subscriptions /> : section === "clients" ? <Clients /> : <Connections />}</PageBody>
    </div>;
}

function Connections() {
    const { t, locale } = useI18n(); const resource = useAssistantPage<Connection>("/console/connections");
    const projectRows = useFleet((fleet) => fleet.snap.projects);
    const projects = useMemo(() => projectRows.map((project) => project.id), [projectRows]);
    const [params, setParams] = useSearchParams(); const [busy, setBusy] = useState(""); const [error, setError] = useState("");
    const [credential, setCredential] = useState<{ grant: Connection; token: string } | null>(null);
    const close = () => { if (!busy) { setParams((before) => { const next = new URLSearchParams(before); next.delete("enroll"); return next; }); setCredential(null); setError(""); } };
    const enroll = async (body: Partial<Connection>) => {
        setBusy("enroll"); setError("");
        try { const saved = await enrollConnection(body); setCredential(saved); resource.refresh(); } catch (cause) { setError(message(cause)); } finally { setBusy(""); }
    };
    const revoke = async (item: Connection) => {
        if (!window.confirm(t("assistant.revokeConfirm"))) return;
        setBusy(item.id); setError("");
        try { await revokeConnection(item.id); resource.refresh(); } catch (cause) { setError(message(cause)); } finally { setBusy(""); }
    };
    return <>
        <AssistantFeedback error={resource.error} loading={resource.loading} onRefresh={resource.refresh} />
        {error && !params.has("enroll") ? <p role="alert" className="text-sm text-error-primary">{error}</p> : null}
        <Panel title={t("assistant.connections")} aside={<><Button size="sm" color="secondary" iconLeading={RefreshCw01} onClick={resource.refresh}>{t("assistant.refresh")}</Button><Button size="sm" iconLeading={Plus} onClick={() => setParams((before) => { const next = new URLSearchParams(before); next.set("enroll", "1"); return next; })}>{t("assistant.enroll")}</Button></>}
            footer={<AssistantPager next={resource.page?.next || ""} cursor={resource.cursor} onPage={resource.setCursor} disabled={!!busy || !!resource.error} />}>
            {resource.page?.items.length === 0 ? <Nothing icon={Link01} title={t("assistant.emptyConnections")}>{t("assistant.emptyConnectionsHint")}</Nothing> : <ul className="divide-y divide-secondary">{resource.page?.items.map((item) => <li key={item.id} className="flex min-w-0 flex-col gap-3 py-4 first:pt-0 last:pb-0 [content-visibility:auto]">
                <div className="flex min-w-0 flex-wrap items-start justify-between gap-3"><div className="min-w-0"><h3 className="break-words text-sm font-semibold text-primary">{item.name}</h3><div className="mt-1 flex flex-wrap items-center gap-2"><Badge type="pill-color" color={item.enabled ? "success" : "gray"} size="sm">{t(item.enabled ? "assistant.enabled" : "assistant.disabled")}</Badge><span className="text-xs text-tertiary">{t(item.kind === "provider" ? "assistant.provider" : "assistant.client")} · {when(item.created_at, locale)}</span></div></div>{item.enabled ? <Button size="sm" color="secondary-destructive" isLoading={busy === item.id} isDisabled={!!busy || !!resource.error} onClick={() => void revoke(item)}>{t("assistant.revoke")}</Button> : null}</div>
                <code className="break-all text-xs text-tertiary" translate="no">{item.principal}</code>
                {item.event_types?.length ? <Chips items={item.event_types.map((id) => ({ id }))} /> : null}
                {Object.entries(item.work || {}).map(([conversation, project]) => <p key={conversation} className="break-all text-xs text-secondary">{t("assistant.work")} · <a className="underline outline-focus-ring focus-visible:outline-2" href={`#/console?conversation=${encodeURIComponent(conversation)}`}>{conversation}</a> → {project}</p>)}
                {item.capabilities?.map((scope) => <p key={`${scope.client}/${scope.capability}/${scope.conversation}`} className="break-all text-xs text-secondary" translate="no">{scope.capability} → {scope.conversation}{scope.approval ? ` · ${t("assistant.clientApproval")}` : ""}</p>)}
            </li>)}</ul>}
        </Panel>
        {params.has("enroll") && !credential ? <ConnectionEditor projects={projects} busy={busy === "enroll"} error={error} onClose={close} onSubmit={(body) => void enroll(body)} /> : null}
        {credential ? <Drawer title={t("assistant.tokenTitle")} subtitle={credential.grant.name} onClose={close}><p className="text-sm text-secondary">{t(credential.token ? "assistant.tokenHint" : "assistant.tokenLost")}</p>{credential.token ? <code className="block break-all rounded-lg bg-secondary p-3 text-sm text-primary" translate="no">{credential.token}</code> : null}<Button size="sm" color="secondary" onClick={close}>{t("assistant.close")}</Button></Drawer> : null}
    </>;
}

function Subscriptions() {
    const { t } = useI18n(); const resource = useAssistantPage<Subscription>("/console/subscriptions");
    const providers = useAssistantPage<Connection>("/console/connections", 15000, "providerCursor");
    const definitions = useAssistantPage<EventProvider>("/console/event-providers", 15000, "eventProviderCursor");
    const projectRows = useFleet((fleet) => fleet.snap.projects);
    const projects = useMemo(() => projectRows.map((project) => project.id), [projectRows]);
    const [params, setParams] = useSearchParams(); const [initial, setInitial] = useState<Subscription | null>(null);
    const [busy, setBusy] = useState(false); const [error, setError] = useState("");
    const open = (rule?: Subscription) => {
        const draft = rule || { id: crypto.randomUUID(), revision: 0, name: "", enabled: true, event_type: "mail.received", include_derived: false, mode: "inbox" as const, replay_safe: false, conversation: "console:main" };
        setInitial(draft); setError(""); setParams((before) => { const next = new URLSearchParams(before); next.set("edit", draft.id); return next; });
    };
    const close = () => { if (!busy) { setInitial(null); setParams((before) => { const next = new URLSearchParams(before); next.delete("edit"); return next; }); } };
    const save = async (body: Subscription) => { setBusy(true); setError(""); try { await saveSubscription(body); setInitial(null); setParams((before) => { const next = new URLSearchParams(before); next.delete("edit"); return next; }); resource.refresh(); } catch (cause) { setError(message(cause)); } finally { setBusy(false); } };
    const editing = params.get("edit") ? initial || resource.page?.items.find((rule) => rule.id === params.get("edit")) : null;
    return <>
        <AssistantFeedback error={resource.error || providers.error} loading={resource.loading} onRefresh={() => { resource.refresh(); providers.refresh(); }} />
        <Panel title={t("assistant.subscriptions")} aside={<Button size="sm" iconLeading={Plus} isDisabled={!!resource.error || !!providers.error} onClick={() => open()}>{t("assistant.createRule")}</Button>}
            footer={<AssistantPager next={resource.page?.next || ""} cursor={resource.cursor} onPage={resource.setCursor} disabled={busy || !!resource.error} />}>
            {resource.page?.items.length === 0 ? <Nothing icon={Link01} title={t("assistant.emptyRules")}>{t("assistant.emptyRulesHint")}</Nothing> : <ul className="divide-y divide-secondary">{resource.page?.items.map((rule) => <li key={rule.id} className="flex min-w-0 flex-wrap items-start justify-between gap-3 py-4 first:pt-0 last:pb-0 [content-visibility:auto]"><div className="min-w-0 flex-1"><div className="flex flex-wrap items-center gap-2"><h3 className="break-words text-sm font-semibold">{rule.name}</h3><Badge type="pill-color" color={rule.enabled ? "success" : "gray"} size="sm">{t(rule.enabled ? "assistant.enabled" : "assistant.disabled")}</Badge></div><p className="mt-2 break-all text-sm text-secondary" translate="no">{rule.event_type} → {t(rule.mode === "work" ? "assistant.work" : rule.mode === "provider" ? "assistant.provider" : "assistant.manual")}</p><p className="mt-1 break-all text-xs text-tertiary">{rule.mode === "work" ? `${rule.conversation} · ${rule.project}` : rule.mode === "provider" ? providers.page?.items.find((item) => item.id === rule.handler)?.name || rule.handler : rule.source || "—"}</p>{rule.instruction ? <p className="mt-2 whitespace-pre-wrap break-words text-sm text-secondary">{rule.instruction}</p> : null}</div><Button size="sm" color="secondary" isDisabled={busy || !!resource.error || !!providers.error} onClick={() => open(rule)}>{t("assistant.edit")}</Button></li>)}</ul>}
        </Panel>
        {editing ? <SubscriptionEditor key={editing.id} initial={editing} projects={projects} providers={providers.page?.items || []} definitions={definitions.error ? [] : definitions.page?.items || []} busy={busy} error={error} onClose={close} onSubmit={(body) => void save(body)} /> : null}
    </>;
}

function Clients() {
    const { t } = useI18n(); const resource = useAssistantPage<ClientRegistration>("/console/client-capabilities");
    return <><AssistantFeedback error={resource.error} loading={resource.loading} onRefresh={resource.refresh} /><Panel title={t("assistant.clients")} aside={<Button size="sm" color="secondary" onClick={resource.refresh}>{t("assistant.refresh")}</Button>} footer={<AssistantPager next={resource.page?.next || ""} cursor={resource.cursor} onPage={resource.setCursor} disabled={!!resource.error} />}>
        {resource.page?.items.length === 0 ? <Nothing icon={Link01} title={t("assistant.emptyClients")}>{t("assistant.emptyClientsHint")}</Nothing> : <ul className="divide-y divide-secondary">{resource.page?.items.map((client) => <li key={client.id} className="flex min-w-0 flex-col gap-3 py-4 first:pt-0 last:pb-0 [content-visibility:auto]"><div className="flex flex-wrap items-center gap-2"><h3 className="break-words text-sm font-semibold">{client.name}</h3><Badge type="pill-color" size="sm" color={Date.parse(client.until) > Date.now() ? "success" : "gray"}>{t(Date.parse(client.until) > Date.now() ? "assistant.online" : "assistant.offline")}</Badge></div><code className="break-all text-xs text-tertiary" translate="no">{client.id}</code><Chips items={client.capabilities.map((capability) => ({ id: capability.name, title: capability.description }))} /></li>)}</ul>}
    </Panel></>;
}

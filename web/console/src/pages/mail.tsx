import { useState } from "react";
import { useNavigate, useSearchParams } from "react-router";
import { Plus, RefreshCw01, Upload01, Download01, Settings01 } from "@untitledui/icons";
import { Button } from "@/components/base/buttons/button";
import { Badge } from "@/components/base/badges/badges";
import { PageBody, PageHeader, Panel } from "@/components/steve/page";
import { MailRoster } from "@/components/steve/mail-roster";
import { MailAccountEditor } from "@/components/steve/mail-account-editor";
import { MailImport } from "@/components/steve/mail-import";
import { MailLogin } from "@/components/steve/mail-login";
import { MailAI, MailApps, MailExport } from "@/components/steve/mail-settings";
import { useMail } from "@/hooks/use-mail";
import { useI18n } from "@/providers/locale-provider";
import { useIntent } from "@/lib/fleet";
import { batchMailboxes, removeMailbox, startMailLogin, syncMailbox, updateMailbox } from "@/lib/api/mail";
import { message } from "@/lib/http";
import { conversationURL } from "@/lib/conversation-identity";

export function MailPage() {
    const { t } = useI18n(); const resource = useMail(); const { act } = useIntent(); const navigate = useNavigate();
    const [params, setParams] = useSearchParams(); const [busy, setBusy] = useState(""); const [error, setError] = useState("");
    const snapshot = resource.snapshot; const panel = params.get("panel");
    const disabled = !!resource.error || !snapshot || !!busy;
    const change = (key: string, value: string) => setParams(before => { const next = new URLSearchParams(before); if (key === "resetFilters") { for (const name of ["q", "category", "provider", "status"]) next.delete(name); } else if (value) next.set(key, value); else next.delete(key); return next; }, { replace: true });
    const close = () => { setParams(before => { const next = new URLSearchParams(before); next.delete("panel"); next.delete("account"); next.delete("draft"); next.delete("login"); return next; }); setError(""); };
    const saved = () => { close(); resource.refresh(); };
    const open = (value: string, account?: string) => { setParams(before => { const next = new URLSearchParams(before); next.set("panel", value); if (account) next.set("account", account); else next.delete("account"); return next; }); setError(""); };
    const action = async (id: string, run: () => Promise<unknown>) => { setBusy(id); setError(""); try { await run(); resource.refresh(); return true; } catch (cause) { setError(message(cause)); return false; } finally { setBusy(""); } };
    const login = (id: string) => action(id, async () => { const job = await startMailLogin(id); if (job.method !== "onepassword" || job.needs_owner) setParams(before => { const next = new URLSearchParams(before); next.set("login", job.id); return next; }); });
    const assist = (account?: string) => {
        const ai = snapshot?.ai; if (!ai?.enabled || !ai.client || !ai.conversation) return;
        act(t(account ? "mail.aiLoginPrompt" : "mail.aiManagePrompt", { client: ai.client, account: account || "" })); navigate(conversationURL(ai.conversation));
    };
    const groups = [...new Set(snapshot?.items.map(row => row.group).filter(Boolean) || [])].sort();
    const editing = snapshot?.items.find(row => row.id === params.get("account"));
    return <div className="workbench-page flex min-w-0 flex-col">
        <PageHeader title={t("mail.title")} description={t("mail.description")} actions={<>
            <Button size="sm" color="secondary" iconLeading={Upload01} isDisabled={disabled} onClick={() => open("import")}>{t("mail.import")}</Button>
            <Button size="sm" color="secondary" iconLeading={Download01} isDisabled={disabled || !snapshot?.items.length} onClick={() => open("export")}>{t("mail.export")}</Button>
            <Button size="sm" iconLeading={Plus} isDisabled={disabled} onClick={() => open("add")}>{t("mail.add")}</Button>
        </>} />
        <PageBody>
            {resource.loading ? <p role="status" className="text-sm text-tertiary">{t("mail.loading")}</p> : null}
            {resource.error ? <Panel title={t(resource.unavailable ? "mail.offline" : "mail.readError")} aside={<Button size="sm" color="secondary" onClick={resource.refresh}>{t("mail.retry")}</Button>}>
                <p role="alert" className="break-words text-sm text-error-primary">{resource.unavailable ? t("mail.offlineHint") : resource.error}</p>
                <a href="https://github.com/gopact-ai/steve/tree/master/providers/mail#mailbox-management" target="_blank" rel="noreferrer" className="mt-3 inline-block text-sm text-brand-secondary underline outline-focus-ring focus-visible:outline-2">{t("mail.setupGuide")}</a>
            </Panel> : null}
            {error ? <p role="alert" className="break-words text-sm text-error-primary">{error}</p> : null}
            {snapshot && !resource.error ? <>
                <Panel title={t("mail.accounts")} footer={<p className="text-xs text-tertiary">{t("mail.newOnly")}</p>} padding="flush" aside={<>
                    <Button size="sm" color="secondary" iconLeading={RefreshCw01} onClick={resource.refresh}>{t("mail.refresh")}</Button>
                    <Button size="sm" color="secondary" iconLeading={Settings01} isDisabled={disabled} onClick={() => open("apps")}>{t("mail.apps")}</Button>
                </>}>
                    <MailRoster accounts={snapshot.items} params={params} change={change} disabled={disabled} busy={busy} error={error} onBatch={(ids, update) => action("batch", () => batchMailboxes({ ids, ...update }))} onAction={(row, command) => {
                        if (command === "edit") open("edit", row.id);
                        else if (command === "login") void login(row.id);
                        else if (command === "sync") void action(row.id, () => syncMailbox(row.id));
                        else if (command === "pause") void action(row.id, () => updateMailbox(row.id, { enabled: !row.enabled }));
                        else if (command === "job") change("login", row.login || "");
                        else if (window.confirm(t("mail.removeConfirm", { address: row.address }))) void action(row.id, () => removeMailbox(row.id));
                    }} />
                </Panel>
                <Panel title={t("mail.ai")} description={t("mail.aiHint")} aside={<Button size="sm" color="secondary" isDisabled={disabled} onClick={() => open("ai")}>{t(snapshot.ai.enabled ? "mail.manageAI" : "mail.enableAI")}</Button>}>
                    <div className="flex min-w-0 flex-wrap items-center gap-3"><Badge size="sm" type="pill-color" color={snapshot.ai.enabled ? "success" : "gray"}>{t(snapshot.ai.enabled ? "mail.aiEnabled" : "mail.aiDisabled")}</Badge>{snapshot.ai.enabled ? <><span className="break-all text-xs text-tertiary" translate="no">{snapshot.ai.conversation}</span><Button size="sm" color="secondary" isDisabled={disabled} onClick={() => assist()}>{t("mail.aiLogin")}</Button></> : null}</div>
                </Panel>
                <p className="text-xs text-tertiary">{t("mail.rulesHint")} <a href="#/connections?section=subscriptions" className="text-brand-secondary underline outline-focus-ring focus-visible:outline-2">{t("mail.rules")}</a></p>
            </> : null}
        </PageBody>
        {panel === "add" && snapshot && !resource.error ? <MailAccountEditor groups={groups} onClose={close} onSaved={saved} /> : null}
        {panel === "edit" && editing && !resource.error ? <MailAccountEditor groups={groups} initial={editing} onClose={close} onSaved={saved} /> : null}
        {panel === "apps" && snapshot && !resource.error ? <MailApps onClose={close} onSaved={saved} /> : null}
        {panel === "export" && snapshot && !resource.error ? <MailExport onClose={close} /> : null}
        {panel === "ai" && snapshot && !resource.error ? <MailAI current={snapshot.ai} onClose={close} onSaved={saved} /> : null}
        {panel === "import" && snapshot && !resource.error ? <MailImport draftID={params.get("draft") || undefined} ai={snapshot.ai} onDraft={id => change("draft", id)} onClose={close} onSaved={saved} /> : null}
        {params.get("login") && !resource.error ? <MailLogin id={params.get("login")!} accounts={snapshot?.items || []} onClose={() => { change("login", ""); resource.refresh(); }} onDone={() => { change("login", ""); resource.refresh(); }} /> : null}
    </div>;
}

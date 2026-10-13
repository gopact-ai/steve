import { useState } from "react";
import { Input } from "@/components/base/input/input";
import { TextArea } from "@/components/base/textarea/textarea";
import { Checkbox } from "@/components/base/checkbox/checkbox";
import { Select } from "@/components/base/select/select";
import { Button } from "@/components/base/buttons/button";
import { Drawer } from "@/components/steve/drawer";
import { useI18n } from "@/providers/locale-provider";
import { configureMailAI, configureMailApps, exportMail } from "@/lib/api/mail";
import { enrollConnection, revokeConnection } from "@/lib/api/assistant";
import { message } from "@/lib/http";
import { mailCapabilities, type MailSnapshot } from "@/lib/mail-types";

export function MailApps({ onClose, onSaved }: { onClose: () => void; onSaved: () => void }) {
    const { t } = useI18n(); const [google, setGoogle] = useState(""); const [microsoft, setMicrosoft] = useState(""); const [busy, setBusy] = useState(false); const [error, setError] = useState("");
    const close = () => { if (!busy) onClose(); };
    const save = async () => { setBusy(true); setError(""); try { await configureMailApps({ google_credentials: google, client_id: microsoft }); setGoogle(""); onSaved(); } catch (cause) { setError(message(cause)); } finally { setBusy(false); } };
    return <Drawer title={t("mail.apps")} onClose={close} closeDisabled={busy}><form className="flex min-w-0 flex-col gap-4" onSubmit={event => { event.preventDefault(); void save(); }}>
        <p className="text-sm text-tertiary">{t("mail.appsHint")}</p>
        <TextArea name="mail-google-app" label={t("mail.googleCredentials")} value={google} onChange={setGoogle} isDisabled={busy} autoComplete="off" />
        <a href="https://console.cloud.google.com/apis/credentials" target="_blank" rel="noreferrer" className="text-sm text-brand-secondary underline outline-focus-ring focus-visible:outline-2">{t("mail.googleConsole")}</a>
        <Input name="mail-microsoft-app" label={t("mail.microsoftID")} value={microsoft} onChange={setMicrosoft} isDisabled={busy} autoComplete="off" spellCheck="false" />
        <a href="https://entra.microsoft.com" target="_blank" rel="noreferrer" className="text-sm text-brand-secondary underline outline-focus-ring focus-visible:outline-2">{t("mail.microsoftConsole")}</a>
        {error ? <p role="alert" className="break-words text-sm text-error-primary">{error}</p> : null}
        <Button type="submit" size="sm" isLoading={busy}>{t("mail.save")}</Button>
    </form></Drawer>;
}

export function MailExport({ onClose }: { onClose: () => void }) {
    const { t } = useI18n(); const [format, setFormat] = useState("csv"); const [secrets, setSecrets] = useState(false); const [busy, setBusy] = useState(false); const [error, setError] = useState(""); const [done, setDone] = useState(false);
    const close = () => { if (!busy) onClose(); };
    const download = async () => {
        setBusy(true); setError(""); setDone(false);
        try {
            const file = await exportMail({ format, include_secrets: secrets }); const bytes = Uint8Array.from(atob(file.data), char => char.charCodeAt(0));
            const url = URL.createObjectURL(new Blob([bytes], { type: file.type })); const link = document.createElement("a"); link.href = url; link.download = file.name; link.click(); window.setTimeout(() => URL.revokeObjectURL(url), 1000); setDone(true);
        } catch (cause) { setError(message(cause)); } finally { setBusy(false); }
    };
    return <Drawer title={t("mail.export")} onClose={close} closeDisabled={busy}><div className="flex min-w-0 flex-col gap-4">
        <Select label={t("mail.format")} selectedKey={format} onSelectionChange={key => setFormat(String(key))} isDisabled={busy} items={[{ id: "csv", label: "1Password CSV" }, { id: "1pux", label: "1Password 1PUX" }, { id: "json", label: "Steve JSON" }]}>{item => <Select.Item {...item} />}</Select>
        <Checkbox label={t("mail.includeSecrets")} isSelected={secrets} onChange={setSecrets} isDisabled={busy} />
        <p className="text-sm text-tertiary">{t(secrets ? "mail.plaintextExport" : "mail.exportHint")}</p>
        {error ? <p role="alert" className="break-words text-sm text-error-primary">{error}</p> : null}
        {done ? <p role="status" className="text-sm text-success-primary">{t("mail.downloaded")}</p> : null}
        <Button size="sm" isLoading={busy} onClick={() => void download()}>{t("mail.download")}</Button>
    </div></Drawer>;
}

export function MailAI({ current, onClose, onSaved }: { current: MailSnapshot["ai"]; onClose: () => void; onSaved: () => void }) {
    const { t } = useI18n(); const [conversation, setConversation] = useState(() => current.conversation || (sessionStorage.getItem("steve.conversation.transport") === "console" ? sessionStorage.getItem("steve.conversation") : "") || "console:main");
    const [approval, setApproval] = useState(false); const [busy, setBusy] = useState(false); const [error, setError] = useState("");
    const [credential, setCredential] = useState<{ id: string; token: string } | null>(null); const [key] = useState(() => crypto.randomUUID());
    const close = () => { if (!busy) onClose(); };
    const enable = async () => {
        setBusy(true); setError("");
        try {
            let saved = credential;
            if (!saved) {
                const result = await enrollConnection({ idempotency_key: key, name: "Steve Mail Assistant", kind: "client", capabilities: mailCapabilities.map(capability => ({ client: "", capability, conversation, approval })) });
                if (!result.token) throw new Error(t("mail.aiTokenLost"));
                saved = { id: result.grant.id, token: result.token }; setCredential(saved);
            }
            await configureMailAI({ ...saved, conversation }); setCredential(null); onSaved();
        } catch (cause) { setError(message(cause)); } finally { setBusy(false); }
    };
    const disable = async () => { setBusy(true); setError(""); try { if (current.client) await revokeConnection(current.client); await configureMailAI({ enabled: false }); onSaved(); } catch (cause) { setError(message(cause)); } finally { setBusy(false); } };
    return <Drawer title={t("mail.ai")} onClose={close} closeDisabled={busy}><div className="flex min-w-0 flex-col gap-4">
        <p className="text-sm text-tertiary">{t("mail.aiHint")}</p>
        <Input name="mail-ai-conversation" label={t("mail.conversation")} value={conversation} onChange={setConversation} isRequired isDisabled={busy || current.enabled || !!credential} autoComplete="off" spellCheck="false" />
        {!current.enabled ? <Checkbox label={t("mail.aiApproval")} isSelected={approval} onChange={setApproval} isDisabled={busy || !!credential} /> : null}
        {error ? <p role="alert" className="break-words text-sm text-error-primary">{error}</p> : null}
        <Button size="sm" color={current.enabled ? "secondary-destructive" : "primary"} isLoading={busy} onClick={() => void (current.enabled ? disable() : enable())}>{t(current.enabled ? "mail.disableAI" : "mail.enableAI")}</Button>
    </div></Drawer>;
}

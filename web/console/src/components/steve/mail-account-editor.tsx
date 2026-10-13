import { useState } from "react";
import { Input } from "@/components/base/input/input";
import { Select } from "@/components/base/select/select";
import { Button } from "@/components/base/buttons/button";
import { Drawer } from "@/components/steve/drawer";
import { MailCategoryInput } from "@/components/steve/mail-category-input";
import { useI18n } from "@/providers/locale-provider";
import { addMailbox, updateMailbox } from "@/lib/api/mail";
import { message } from "@/lib/http";
import { providerNames, type MailAccount, type MailProvider } from "@/lib/mail-types";

export function MailAccountEditor({ initial, groups, onClose, onSaved }: { initial?: MailAccount; groups: string[]; onClose: () => void; onSaved: (account: MailAccount) => void }) {
    const { t } = useI18n(); const [provider, setProvider] = useState<MailProvider>(initial?.provider || "gmail");
    const [address, setAddress] = useState(initial?.address || ""); const [title, setTitle] = useState(initial?.title || "");
    const [password, setPassword] = useState(""); const [busy, setBusy] = useState(false); const [error, setError] = useState("");
    const [group, setGroup] = useState(initial?.group || ""); const [tags, setTags] = useState((initial?.tags || []).join(", "));
    const close = () => { if (!busy) onClose(); };
    const save = async () => {
        setBusy(true); setError("");
        try {
            const metadata = { group, tags: tags.split(/[,，]/).map(tag => tag.trim()).filter(Boolean) };
            const account = initial ? await updateMailbox(initial.id, { title, ...metadata, ...(password ? { app_password: password } : {}) }) :
                await addMailbox({ provider, address, title: title || address, ...metadata, password, credential_kind: provider === "icloud" ? "app_password" : "login_password" });
            setPassword(""); onSaved(account);
        } catch (cause) { setError(message(cause)); } finally { setBusy(false); }
    };
    return <Drawer title={t(initial ? "mail.edit" : "mail.add")} onClose={close} closeDisabled={busy}>
        <form className="flex min-w-0 flex-col gap-4" onSubmit={event => { event.preventDefault(); void save(); }}>
            <Select label={t("mail.provider")} selectedKey={provider} isDisabled={!!initial || busy} onSelectionChange={key => setProvider(key as MailProvider)} items={Object.entries(providerNames).map(([id, label]) => ({ id, label }))}>{item => <Select.Item {...item} />}</Select>
            <Input name="mail-address" label={t("mail.address")} type="email" value={address} onChange={setAddress} isRequired isDisabled={!!initial || busy} autoComplete="username" spellCheck="false" placeholder="name@example.com…" />
            <Input name="mail-title" label={t("mail.name")} value={title} onChange={setTitle} isDisabled={busy} autoComplete="off" />
            <MailCategoryInput value={group} onChange={setGroup} groups={groups} disabled={busy} />
            <Input name="mail-tags" label={t("mail.tags")} value={tags} onChange={setTags} isDisabled={busy} autoComplete="off" hint={t("mail.tagsHint")} />
            {provider === "icloud" ? <><Input name="mail-app-password" label={t("mail.appPassword")} type="password" value={password} onChange={setPassword} isDisabled={busy} autoComplete="new-password" hint={t("mail.appPasswordHint")} /><a href="https://account.apple.com" target="_blank" rel="noreferrer" className="text-sm text-brand-secondary underline outline-focus-ring focus-visible:outline-2">{t("mail.appleAccount")}</a></> : <p className="text-sm text-tertiary">{t("mail.oauthHint")}</p>}
            {error ? <p role="alert" className="break-words text-sm text-error-primary">{error}</p> : null}
            <div className="flex flex-wrap justify-end gap-2"><Button size="sm" color="secondary" isDisabled={busy} onClick={close}>{t("common.cancel")}</Button><Button type="submit" size="sm" isLoading={busy}>{t("mail.save")}</Button></div>
        </form>
    </Drawer>;
}

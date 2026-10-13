import { useEffect, useState } from "react";
import { useNavigate } from "react-router";
import { Checkbox } from "@/components/base/checkbox/checkbox";
import { TextArea } from "@/components/base/textarea/textarea";
import { Button } from "@/components/base/buttons/button";
import { Drawer } from "@/components/steve/drawer";
import { useI18n } from "@/providers/locale-provider";
import { useIntent } from "@/lib/fleet";
import { commitMailImport, previewMailImport, readMailDraft } from "@/lib/api/mail";
import { message } from "@/lib/http";
import { conversationURL } from "@/lib/conversation-identity";
import { providerNames, type MailDraft, type MailSnapshot } from "@/lib/mail-types";

function base64(bytes: Uint8Array) {
    const chunks: string[] = [];
    for (let start = 0; start < bytes.length; start += 8192) chunks.push(String.fromCharCode(...bytes.subarray(start, start + 8192)));
    return btoa(chunks.join(""));
}

export function MailImport({ draftID, ai, onDraft, onClose, onSaved }: { draftID?: string; ai: MailSnapshot["ai"]; onDraft: (id: string) => void; onClose: () => void; onSaved: () => void }) {
    const { t } = useI18n(); const { act } = useIntent(); const navigate = useNavigate();
    const [draft, setDraft] = useState<MailDraft | null>(null); const [selected, setSelected] = useState<string[]>([]);
    const [paste, setPaste] = useState(""); const [appPasswords, setAppPasswords] = useState(false);
    const [busy, setBusy] = useState(false); const [error, setError] = useState("");
    useEffect(() => {
        if (!draftID) return; const controller = new AbortController();
        void readMailDraft(draftID, controller.signal).then(value => { setDraft(value); setSelected(value.items.filter(row => !row.duplicate).map(row => row.id)); }, cause => { if (!controller.signal.aborted) setError(message(cause)); });
        return () => controller.abort();
    }, [draftID]);
    const close = () => { if (!busy) onClose(); };
    const preview = async (name: string, bytes: Uint8Array) => {
        if (bytes.length > 4 * 1024 * 1024) { setError(t("mail.fileLimit")); return; }
        setBusy(true); setError("");
        try { const value = await previewMailImport({ name, data: base64(bytes) }); setPaste(""); setDraft(value); setSelected(value.items.filter(row => !row.duplicate).map(row => row.id)); onDraft(value.id); }
        catch (cause) { setError(message(cause)); } finally { setBusy(false); }
    };
    const commit = async () => {
        if (!draft) return; setBusy(true); setError("");
        try { await commitMailImport({ draft_id: draft.id, rows: selected, icloud_app_passwords: appPasswords }); onSaved(); }
        catch (cause) { setError(message(cause)); } finally { setBusy(false); }
    };
    const assist = async () => {
        if (!draft || !ai.client || !ai.conversation) return;
        // Only opaque IDs reach the model. The imported document and secrets stay in the provider vault.
        setBusy(true); setError("");
        try {
            if (appPasswords) await commitMailImport({ draft_id: draft.id, rows: selected, icloud_app_passwords: true });
            act(t("mail.aiImportPrompt", { client: ai.client, draft: draft.id, rows: JSON.stringify(selected) }));
            navigate(conversationURL(ai.conversation));
        } catch (cause) { setError(message(cause)); } finally { setBusy(false); }
    };
    return <Drawer title={t("mail.import")} onClose={close} closeDisabled={busy} width={600}>
        <div className="flex min-w-0 flex-col gap-4">
            <p className="text-sm text-tertiary">{t("mail.importHint")}</p>
            {!draft ? <>
                <label className="flex flex-col gap-2 text-sm font-medium text-secondary">{t("mail.file")}<input name="mail-import-file" type="file" accept=".csv,.1pux,.json,.txt" disabled={busy} className="min-w-0 max-w-full rounded-md border border-secondary p-3 text-sm outline-focus-ring focus-visible:outline-2" onChange={event => {
                    const file = event.target.files?.[0]; event.target.value = "";
                    if (!file) return;
                    if (file.size > 4 * 1024 * 1024) { setError(t("mail.fileLimit")); return; }
                    void file.arrayBuffer().then(buffer => preview(file.name, new Uint8Array(buffer))).catch(cause => setError(message(cause)));
                }} /></label>
                <TextArea name="mail-import-text" label={t("mail.paste")} value={paste} onChange={setPaste} isDisabled={busy} autoComplete="off" placeholder="name@gmail.com…" hint={t("mail.pasteHint")} />
                <Button size="sm" color="secondary" isLoading={busy} isDisabled={!paste.trim()} onClick={() => void preview("accounts.txt", new TextEncoder().encode(paste))}>{t("mail.preview")}</Button>
            </> : <>
                <p role="status" className="text-sm text-secondary">{t("mail.previewSummary", { count: draft.items.length, skipped: draft.skipped })}</p>
                <ul className="max-h-96 overflow-y-auto overscroll-contain divide-y divide-secondary rounded-lg border border-secondary px-3">
                    {draft.items.map(row => <li key={row.id} className="min-w-0 py-3 [content-visibility:auto]"><Checkbox isSelected={selected.includes(row.id)} isDisabled={row.duplicate || busy || draft.committed} onChange={value => setSelected(before => value ? [...before, row.id] : before.filter(id => id !== row.id))} label={<span className="min-w-0 break-all text-sm">{row.address}<span className="mt-1 block text-xs text-tertiary">{providerNames[row.provider]} · {row.group || t("mail.uncategorized")} · {t(row.duplicate ? "mail.duplicate" : row.has_password ? "mail.secretStored" : "mail.noSecret")}{row.tags?.length ? ` · ${row.tags.join(", ")}` : ""}</span></span>} /></li>)}
                </ul>
                {draft.items.some(row => row.provider === "icloud" && row.has_password && row.credential_kind !== "app_password") ? <Checkbox label={t("mail.confirmAppPasswords")} isSelected={appPasswords} onChange={setAppPasswords} isDisabled={busy} /> : null}
                {draft.committed ? <p role="status" className="text-sm text-success-primary">{t("mail.imported")}</p> : <div className="flex flex-wrap justify-end gap-2">
                    <Button size="sm" color="secondary" isDisabled={busy} onClick={() => { setDraft(null); setSelected([]); onDraft(""); }}>{t("mail.chooseAgain")}</Button>
                    {ai.enabled ? <Button size="sm" color="secondary" isDisabled={busy || !selected.length} onClick={() => void assist()}>{t("mail.aiImport")}</Button> : null}
                    <Button size="sm" isLoading={busy} isDisabled={!selected.length} onClick={() => void commit()}>{t("mail.importSelected", { count: selected.length })}</Button>
                </div>}
            </>}
            {error ? <p role="alert" className="break-words text-sm text-error-primary">{error}</p> : null}
        </div>
    </Drawer>;
}

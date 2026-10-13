import { useEffect, useState } from "react";
import { Button } from "@/components/base/buttons/button";
import { Drawer } from "@/components/steve/drawer";
import { useI18n } from "@/providers/locale-provider";
import { readMailLogin } from "@/lib/api/mail";
import { message } from "@/lib/http";
import type { MailLogin as Login } from "@/lib/mail-types";

export function MailLogin({ id, onClose, onDone }: { id: string; onClose: () => void; onDone: () => void }) {
    const { t } = useI18n(); const [job, setJob] = useState<Login | null>(null); const [error, setError] = useState("");
    useEffect(() => {
        const controller = new AbortController(); let pending = false; let done = false;
        const load = async () => {
            if (pending || done) return; pending = true;
            try { const value = await readMailLogin(id, controller.signal); if (!controller.signal.aborted) { setJob(value); setError(""); done = ["authorized", "error"].includes(value.state); } }
            catch (cause) { if (!controller.signal.aborted) setError(message(cause)); }
            finally { pending = false; }
        };
        void load(); const timer = window.setInterval(() => { if (!document.hidden) void load(); }, 1500);
        return () => { controller.abort(); window.clearInterval(timer); };
    }, [id]);
    const safeURL = (() => { try { const url = new URL(job?.url || ""); return url.protocol === "https:" && ["accounts.google.com", "login.microsoftonline.com", "microsoft.com", "www.microsoft.com"].includes(url.hostname) ? url.href : undefined; } catch { return undefined; } })();
    return <Drawer title={t("mail.login")} onClose={onClose}>
        <div className="flex min-w-0 flex-col gap-4" aria-live="polite">
            <p className="text-sm text-secondary">{t(job?.state === "authorized" ? "mail.authorizedHint" : job?.state === "error" ? "mail.loginError" : job?.state === "waiting" ? "mail.waitingHint" : "mail.starting")}</p>
            {job?.user_code ? <div><p className="mb-2 text-xs text-tertiary">{t("mail.deviceCode")}</p><code className="break-all rounded-md bg-secondary p-3 text-lg font-semibold tracking-widest" translate="no">{job.user_code}</code></div> : null}
            {safeURL ? <Button size="sm" color="primary" href={safeURL} target="_blank" rel="noreferrer">{t("mail.openLogin")}</Button> : null}
            {error || job?.hint ? <p role="alert" className="break-words text-sm text-error-primary">{error || job?.hint}</p> : null}
            {job?.state === "authorized" ? <Button size="sm" onClick={onDone}>{t("mail.done")}</Button> : null}
        </div>
    </Drawer>;
}

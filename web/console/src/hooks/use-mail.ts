import { useEffect, useState } from "react";
import { HTTPError, message } from "@/lib/http";
import { mailSnapshot } from "@/lib/api/mail";
import type { MailSnapshot } from "@/lib/mail-types";
import { loginActive } from "@/lib/mail-login-state";

export function useMail() {
    const [snapshot, setSnapshot] = useState<MailSnapshot | null>(null);
    const [error, setError] = useState(""); const [unavailable, setUnavailable] = useState(false);
    const [loading, setLoading] = useState(true); const [version, setVersion] = useState(0);
    useEffect(() => {
        const controller = new AbortController(); let pending = false; let interval = 5000; let timer: number | undefined;
        const schedule = () => { window.clearTimeout(timer); if (!controller.signal.aborted) timer = window.setTimeout(() => { if (document.hidden) schedule(); else void load(); }, interval); };
        const load = async () => {
            if (pending || controller.signal.aborted) return; pending = true;
            try {
                const value = await mailSnapshot(controller.signal);
                interval = value.items.some(account => loginActive(account.login_status)) ? 1500 : 5000;
                if (!controller.signal.aborted) { setSnapshot(value); setError(""); setUnavailable(false); }
            } catch (cause) {
                if (!controller.signal.aborted) { setError(message(cause)); setUnavailable(cause instanceof HTTPError && cause.code === "mail_unavailable"); }
            } finally { pending = false; if (!controller.signal.aborted) { setLoading(false); schedule(); } }
        };
        void load();
        const visible = () => { if (!document.hidden) void load(); };
        document.addEventListener("visibilitychange", visible);
        return () => { controller.abort(); window.clearTimeout(timer); document.removeEventListener("visibilitychange", visible); };
    }, [version]);
    return { snapshot, error, unavailable, loading, refresh: () => setVersion(value => value + 1) };
}

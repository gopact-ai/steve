import { useRef, useState, useSyncExternalStore } from "react";
import { DotsHorizontal } from "@untitledui/icons";
import { Button as AriaButton } from "react-aria-components";
import { Dropdown } from "@/components/base/dropdown/dropdown";
import { captureMaterial } from "@/lib/api/material";
import { getSubmissionSupport, subscribeSubmissionSupport, fetchContext } from "@/lib/api/console";
import type { Material, MaterialSource, MaterialSelector, MaterialRef } from "@/lib/types";
import { useMaterial } from "@/providers/material-provider";
import { useI18n } from "@/providers/locale-provider";

export interface CaptureSpec { project: string; source: MaterialSource; title?: string }
export function MaterialActions({ capture, material, selector, anchor }: { capture?: CaptureSpec; material?: Material; selector?: MaterialSelector; anchor?: MaterialRef }) {
    const { t } = useI18n(); const store = useMaterial(); const [busy, setBusy] = useState(false); const pending = useRef(false); const [error, setError] = useState("");
    const support = useSyncExternalStore(subscribeSubmissionSupport, getSubmissionSupport);
    if (!support.material_refs) return null;
    async function action(kind: string) {
        if (pending.current) return;
        const target = store.target;
        if (kind === "chat" && !target) { setError(t("materials.noTarget")); return; }
        pending.current = true; setBusy(true); setError("");
        try {
            const value = material || (capture ? await captureMaterial(capture.project, capture.source, capture.title) : null);
            if (!value) throw new Error(t("materials.unknownSource"));
            const ref: MaterialRef = anchor || { id: value.id, ...(selector ? { selector } : {}) };
            if (kind === "chat") { const {context}=await fetchContext(target!.conversation); if(context?.project?.id!==target!.project || !context.project.bound)throw new Error(t("materials.wrongProject")); store.add(value, ref, target!); }
            else if (kind === "side") store.pin(value, ref);
            else store.annotate({ material: value, ref });
        } catch (error) { setError(error instanceof Error ? error.message : String(error)); }
        finally { pending.current = false; setBusy(false); }
    }
    return <div className="inline-flex max-w-full flex-wrap items-center gap-2">
        <Dropdown.Root><AriaButton aria-label={t("materials.actions")} isDisabled={busy} className="workbench-icon-button"><DotsHorizontal aria-hidden="true" className="size-4" /></AriaButton><Dropdown.Popover className="w-56"><Dropdown.Menu onAction={(key) => void action(String(key))}>
            <Dropdown.Item id="chat" label={store.target ? t("materials.addTarget", { title: store.target.title }) : t("materials.add")} />
            <Dropdown.Item id="side" label={t("materials.pin")} />
            <Dropdown.Item id="note" label={t("materials.annotate")} />
        </Dropdown.Menu></Dropdown.Popover></Dropdown.Root>
        {busy && <span role="status" className="text-xs text-tertiary">{t("materials.loading")}</span>}
        {error && <span role="alert" className="text-xs text-error-primary">{error}</span>}
    </div>;
}

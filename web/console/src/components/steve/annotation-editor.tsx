import { useEffect, useRef, useState } from "react";
import { Dialog, Modal, ModalOverlay } from "react-aria-components";
import { Button } from "@/components/base/buttons/button";
import { TextArea } from "@/components/base/textarea/textarea";
import { saveAnnotation, listAnnotations } from "@/lib/api/material";
import type { Material, MaterialRef, MaterialAnnotation } from "@/lib/types";
import { HTTPError } from "@/lib/http";
import { useI18n } from "@/providers/locale-provider";

export function AnnotationEditor({ material, anchor, annotation, onClose, onSaved }: { material: Material; anchor: MaterialRef; annotation?: MaterialAnnotation; onClose: () => void; onSaved: () => void }) {
    const { t } = useI18n();
    const [id] = useState(() => annotation?.id || `annotation-${Date.now()}-${Math.random().toString(36).slice(2)}`);
    const [body, setBody] = useState(annotation?.body || ""); const [revision, setRevision] = useState(annotation?.revision || 0);
    const [busy, setBusy] = useState(false); const pending = useRef(false); const [error, setError] = useState(""); const [conflict, setConflict] = useState(false); const [latestBody, setLatestBody] = useState<string | null>(null);
    const dirty = body !== (annotation?.body || "");
    useEffect(() => {
        if (!dirty && !busy) return;
        const warn = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ""; };
        window.addEventListener("beforeunload", warn);
        return () => window.removeEventListener("beforeunload", warn);
    }, [dirty, busy]);
    function close() { if (!busy && (!dirty || window.confirm(t("materials.discardAnnotation")))) onClose(); }
    async function save(deleted = false) {
        if (pending.current) return; pending.current = true; setBusy(true); setError("");
        try { const saved = await saveAnnotation({ id, project: material.project, ref: anchor, body, expected_revision: revision, deleted }); setRevision(saved.revision); onSaved(); onClose(); }
        catch (error) { setConflict(error instanceof HTTPError && error.status === 409); setError(error instanceof Error ? error.message : String(error)); }
        finally { pending.current = false; setBusy(false); }
    }
    async function reload() { try { const { annotations } = await listAnnotations(material.project); const latest = annotations.find((item) => item.id === id); if (latest) { setRevision(latest.revision); setLatestBody(latest.body); setConflict(false); } } catch (error) { setError(String(error)); } }
    return <ModalOverlay isOpen isDismissable={!busy} onOpenChange={(open) => { if (!open) close(); }} className="fixed inset-0 z-[150] flex items-center justify-center bg-overlay/50 p-4"><Modal className="w-full max-w-lg rounded-xl bg-primary p-6 shadow-xl"><Dialog aria-label={t("materials.annotationTitle")} className="flex flex-col gap-4 outline-none">
        <div><h2 className="text-lg font-semibold">{t("materials.annotationTitle")}</h2><p className="mt-1 break-words text-sm text-tertiary">{material.title} · {material.digest.slice(0, 12)}</p></div>
        <TextArea label={t("materials.note")} hint={t("materials.noteHint")} placeholder={t("materials.notePlaceholder")} value={body} onChange={setBody} rows={5} isDisabled={busy} />
        {error && <p role="alert" className="text-sm text-error-primary">{conflict ? t("materials.conflict") : error}</p>}
        {conflict && <Button size="sm" color="secondary" onClick={() => void reload()}>{t("materials.reload")}</Button>}
        {latestBody !== null && <details open><summary className="text-sm">{t("materials.latestAnnotation")}</summary><pre className="mt-2 whitespace-pre-wrap rounded bg-secondary p-3 text-xs">{latestBody}</pre></details>}
        <div className="flex justify-end gap-2">{annotation && <Button size="sm" color="secondary-destructive" isDisabled={busy || conflict} onClick={() => void save(true)}>{t("materials.delete")}</Button>}<Button size="sm" color="secondary" isDisabled={busy} onClick={close}>{t("materials.cancel")}</Button><Button size="sm" isLoading={busy} isDisabled={conflict} onClick={() => void save()}>{t("materials.save")}</Button></div>
    </Dialog></Modal></ModalOverlay>;
}

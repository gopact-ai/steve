import { useEffect, useState } from "react";
import { Dialog, Modal, ModalOverlay } from "react-aria-components";
import { Button } from "@/components/base/buttons/button";
import { Input } from "@/components/base/input/input";
import { getMaterial, listAnnotations, materialBlob, refKey } from "@/lib/api/material";
import type { Material, MaterialRef, MaterialAnnotation, MaterialSelector } from "@/lib/types";
import { useMaterial } from "@/providers/material-provider";
import { useI18n } from "@/providers/locale-provider";
import { MaterialActions } from "./material-actions";

export function MaterialShelf({ project }: { project: string }) {
    const store = useMaterial(); const { t } = useI18n(); const [annotations, setAnnotations] = useState<MaterialAnnotation[]>([]); const [opened, setOpened] = useState<MaterialRef | null>(null); const [error, setError] = useState("");
    const pins = store.pins(project);
    useEffect(() => { const controller = new AbortController(); void listAnnotations(project, controller.signal).then((value) => { setAnnotations(value.annotations || []); setError(""); }).catch((error) => { if (!controller.signal.aborted) setError(String(error)); }); return () => controller.abort(); }, [project, store.revision]);
    async function edit(annotation: MaterialAnnotation) { try { const material = await getMaterial(project, annotation.ref.id); store.annotate({ material, ref: annotation.ref, annotation }); } catch (error) { setError(String(error)); } }
    return <section className="flex min-w-0 flex-col gap-4 py-4"><p className="text-xs text-tertiary">{t("materials.shelfHint")}</p>
        {!pins.length && <p className="text-sm text-tertiary">{t("materials.empty")}</p>}
        <ul className="flex flex-col gap-2">{pins.map((ref) => <li key={refKey(ref)} className="rounded-lg bg-primary p-3"><button type="button" className="w-full truncate text-left text-sm font-medium" onClick={() => setOpened(ref)}>{ref.title}</button><div className="mt-1 flex gap-3 text-xs text-tertiary"><span>{t(`materials.${ref.kind}`)}</span><button type="button" className="underline" onClick={() => { try { store.unpin(project, ref); } catch (error) { setError(String(error)); } }}>{t("materials.unpin")}</button></div></li>)}</ul>
        <h3 className="text-sm font-semibold">{t("materials.annotations")}</h3>
        {!annotations.length && <p className="text-xs text-tertiary">{t("materials.noAnnotations")}</p>}
        <ul className="flex flex-col gap-3">{annotations.map((annotation) => <li key={annotation.id} className="border-b border-secondary pb-3"><button type="button" className="text-left text-sm whitespace-pre-wrap break-words" onClick={() => void edit(annotation)}>{annotation.body || t("materials.annotate")}</button><div className="mt-1 text-xs text-tertiary"><button type="button" className="underline" onClick={() => setOpened(annotation.ref)}>{t("materials.open")}</button> · {annotation.ref.id.slice(0, 14)} · v{annotation.revision}</div></li>)}</ul>
        {error && <p role="alert" className="text-xs text-error-primary">{error}</p>}
        {opened && <MaterialPreview key={refKey(opened)} project={project} anchor={opened} onClose={() => setOpened(null)} />}
    </section>;
}

export function MaterialPreview({ project, anchor, onClose }: { project: string; anchor: MaterialRef; onClose: () => void }) {
    const { t } = useI18n(); const [material, setMaterial] = useState<Material | null>(null); const [url, setURL] = useState(""); const [text, setText] = useState(""); const [error, setError] = useState("");
    const [selector, setSelector] = useState<MaterialSelector | undefined>(anchor.selector); const [rect, setRect] = useState(() => { const value = anchor.selector?.rect || { x: 0, y: 0, width: 1, height: 1 }; return { x: String(value.x), y: String(value.y), width: String(value.width), height: String(value.height) }; });
    useEffect(() => { const controller = new AbortController(); setMaterial(null); setURL(""); setText(""); setError(""); let objectURL = ""; void Promise.all([getMaterial(project, anchor.id, controller.signal), materialBlob(project, anchor.id, controller.signal)]).then(async ([metadata, blob]) => { if (controller.signal.aborted) return; setMaterial(metadata); objectURL = URL.createObjectURL(blob); setURL(objectURL); if (metadata.kind === "text") { const value = await blob.text(); if (!controller.signal.aborted) setText(value); } }).catch((error) => { if (!controller.signal.aborted) setError(String(error)); }); return () => { controller.abort(); if (objectURL) URL.revokeObjectURL(objectURL); }; }, [project, anchor.id]);
    function applyRect() { const value = { x: Number(rect.x), y: Number(rect.y), width: Number(rect.width), height: Number(rect.height) }; if (Object.values(rect).some((text) => !text.trim()) || Object.values(value).some((n) => !Number.isFinite(n)) || value.x < 0 || value.y < 0 || value.width <= 0 || value.height <= 0 || value.x + value.width > 1 || value.y + value.height > 1) { setError(t("materials.rectInvalid")); return; } setSelector({ kind: "rect", rect: value }); setError(""); }
    return <ModalOverlay isOpen isDismissable onOpenChange={(open) => { if (!open) onClose(); }} className="fixed inset-0 z-[140] flex items-center justify-center bg-overlay/50 p-3"><Modal className="max-h-[95dvh] w-full max-w-4xl overflow-y-auto rounded-xl bg-primary p-5 shadow-xl"><Dialog aria-label={t("materials.preview")} className="flex min-w-0 flex-col gap-4 outline-none"><header className="flex items-center gap-3"><h2 className="min-w-0 flex-1 break-words text-lg font-semibold">{material?.title || t("materials.loading")}</h2>{material && <MaterialActions material={material} selector={selector} />}<Button size="sm" color="secondary" onClick={onClose}>{t("materials.close")}</Button></header>
        {error && <p role="alert" className="text-sm text-error-primary">{error}</p>}
        {material?.kind === "image" && url && <><img src={url} alt={material.title} width={material.width} height={material.height} style={{ imageOrientation: "none" }} className="max-h-[55dvh] w-full object-contain" /><details><summary className="cursor-pointer text-sm">{t("materials.rect")}</summary><p className="my-2 text-xs text-tertiary">{t("materials.rectHint")}</p><div className="grid grid-cols-2 gap-2 sm:grid-cols-4">{([['x','rectX'],['y','rectY'],['width','rectWidth'],['height','rectHeight']] as const).map(([key, label]) => <Input key={key} type="text" inputMode="decimal" label={t(`materials.${label}`)} value={rect[key]} onChange={(value) => setRect((old) => ({ ...old, [key]: value }))} />)}</div><div className="mt-3 flex gap-2"><Button size="sm" onClick={applyRect}>{t("materials.rectApply")}</Button><Button size="sm" color="secondary" onClick={() => setSelector(undefined)}>{t("materials.rectClear")}</Button></div></details></>}
        {selector?.kind === "rect" && selector.rect && <p role="status" className="text-xs text-brand-secondary">{t("materials.selectedRect", selector.rect)}</p>}
        {selector?.kind === "lines" && <p className="text-xs text-brand-secondary">{t("materials.selection", { start: selector.start || 1, end: selector.end || 1 })}</p>}
        {material?.kind === "text" && <pre className="max-h-[60dvh] overflow-auto whitespace-pre-wrap break-words rounded-lg bg-secondary p-4 font-mono text-xs">{selector?.kind === "lines" ? text.replace(/\r\n/g, "\n").split("\n").slice((selector.start || 1) - 1, selector.end).join("\n") : selector?.kind === "quote" ? selector.quote : text}</pre>}
        {url && material && <a href={url} download={material.title} className="self-start text-sm underline">{t("materials.download")}</a>}
        {material && <p className="break-all text-xs text-tertiary">{t("materials.captured")} · {material.digest} · {material.source.path || material.source.reply_id || material.source.kind}</p>}
    </Dialog></Modal></ModalOverlay>;
}

export function MaterialReferences({ items }: { items: import("@/lib/types").FrozenMaterial[] }) {
    const { t } = useI18n(); const [opened, setOpened] = useState<(typeof items)[number] | null>(null);
    return <div className="flex flex-wrap gap-2" aria-label={t("materials.referenced")}>{items.map((item) => <button type="button" key={refKey(item.ref)} className="rounded-md border border-secondary bg-secondary px-2 py-1 text-xs text-secondary" onClick={() => setOpened(item)}>{item.material.title}{item.ref.selector?.kind === "lines" ? ` · L${item.ref.selector.start}–L${item.ref.selector.end}` : ""}</button>)}{opened && <MaterialPreview project={opened.material.project} anchor={opened.ref} onClose={() => setOpened(null)} />}</div>;
}

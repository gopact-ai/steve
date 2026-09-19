import { useEffect, useState } from "react";
import { File02, FileAttachment03, FileCode01, Image01 } from "@untitledui/icons";
import { materialBlob, refKey } from "@/lib/api/material";
import { bytes } from "@/lib/format";
import type { FrozenMaterial, MaterialRef, MaterialSelector } from "@/lib/types";
import { useI18n } from "@/providers/locale-provider";
import { MaterialPreview } from "./material-shelf";

// What a line carried is drawn on the line, not named on it: a picture
// sent with a question shows as the picture, a cut of a file shows the
// lines that were cut, and anything the page cannot draw is still a
// thing with a name, a kind and a size rather than a bare title.

// One fetch per material for the page. The same picture is drawn on the
// line that sent it and on the reply that read it, and a long thread
// scrolls past it again and again; the URL outlives the components that
// use it on purpose, so scrolling back does not refetch.
const loading = new Map<string, Promise<string>>();
function objectURL(project: string, id: string): Promise<string> {
    const key = `${project}:${id}`;
    let pending = loading.get(key);
    if (!pending) {
        pending = materialBlob(project, id).then((blob) => URL.createObjectURL(blob));
        pending.catch(() => loading.delete(key));
        loading.set(key, pending);
    }
    return pending;
}

function useThumbnail(project: string, id: string) {
    const [url, setURL] = useState("");
    const [failed, setFailed] = useState(false);
    useEffect(() => {
        let live = true;
        setURL(""); setFailed(false);
        void objectURL(project, id).then((value) => { if (live) setURL(value); }).catch(() => { if (live) setFailed(true); });
        return () => { live = false; };
    }, [project, id]);
    return { url, failed };
}

// partOf names the cut a reference kept, so a whole file and twenty of
// its lines do not read as the same attachment.
function partOf(selector: MaterialSelector | undefined, t: ReturnType<typeof useI18n>["t"]): string {
    if (selector?.kind === "lines") return `L${selector.start || 1}–L${selector.end || selector.start || 1}`;
    if (selector?.kind === "rect") return t("materials.rect");
    return "";
}

// A cut of text is worth reading where it was referenced; a whole file
// is not, so only a selection brings its words into the transcript.
function excerptOf(item: FrozenMaterial): string {
    const selector = item.ref.selector;
    if (selector?.kind === "quote") return (selector.quote || item.text || "").trim();
    if (selector?.kind === "lines") return (item.text || "").trim();
    return "";
}

export function MaterialAttachments({ items, refs, label, align }: { items?: FrozenMaterial[]; refs?: MaterialRef[]; label?: string; align?: "end" }) {
    const { t } = useI18n();
    const [opened, setOpened] = useState<FrozenMaterial | null>(null);
    const shown = items || [];
    // A reference the coordinator could not resolve still happened: say
    // so rather than drop the attachment from the line silently.
    const missing = (refs || []).filter((ref) => !shown.some((item) => refKey(item.ref) === refKey(ref)));
    if (!shown.length && !missing.length) return null;
    return <div className={`flex min-w-0 flex-wrap gap-2 ${align === "end" ? "justify-end" : ""}`} aria-label={label ?? t("materials.referenced")}>
        {shown.map((item) => <Attachment key={refKey(item.ref)} item={item} onOpen={() => setOpened(item)} />)}
        {missing.map((ref) => <span key={refKey(ref)} className="rounded-md border border-secondary px-2.5 py-1.5 text-xs text-quaternary" title={ref.id}>{t("materials.unresolved")}</span>)}
        {opened && <MaterialPreview key={refKey(opened.ref)} project={opened.material.project} anchor={opened.ref} onClose={() => setOpened(null)} />}
    </div>;
}

function Attachment({ item, onOpen }: { item: FrozenMaterial; onOpen: () => void }) {
    if (item.material.kind === "image") return <ImageAttachment item={item} onOpen={onOpen} />;
    if (excerptOf(item)) return <QuoteAttachment item={item} onOpen={onOpen} />;
    return <FileAttachment item={item} onOpen={onOpen} />;
}

function ImageAttachment({ item, onOpen }: { item: FrozenMaterial; onOpen: () => void }) {
    const { t } = useI18n();
    const { material } = item;
    const { url, failed } = useThumbnail(material.project, material.id);
    if (failed) return <FileAttachment item={item} onOpen={onOpen} />;
    const ratio = material.width && material.height ? material.width / material.height : 4 / 3;
    return <button type="button" onClick={onOpen} title={material.title} aria-label={t("materials.openAttachment", { title: material.title })}
        className="flex min-w-0 max-w-full flex-col gap-1 rounded-lg border border-secondary bg-secondary p-1 hover:border-brand">
        {url
            ? <img src={url} alt={material.title} width={material.width} height={material.height} style={{ imageOrientation: "none" }} className="max-h-60 w-auto max-w-full rounded-md object-contain" />
            : <span aria-hidden="true" style={{ aspectRatio: ratio }} className="block h-24 max-w-full animate-pulse rounded-md bg-primary" />}
        <span className="max-w-60 truncate px-1 pb-0.5 text-left text-xs text-tertiary">{material.title}</span>
    </button>;
}

function QuoteAttachment({ item, onOpen }: { item: FrozenMaterial; onOpen: () => void }) {
    const { t } = useI18n();
    const { material, ref } = item;
    const part = partOf(ref.selector, t);
    return <button type="button" onClick={onOpen} title={material.title}
        className="flex w-full min-w-0 max-w-xl flex-col gap-1.5 rounded-lg border border-secondary bg-secondary px-3 py-2 text-left hover:border-brand">
        <span className="flex min-w-0 items-center gap-2 text-xs text-tertiary">
            <FileCode01 aria-hidden="true" className="size-3.5 shrink-0" />
            <span className="min-w-0 truncate">{material.title}{part ? ` · ${part}` : ""}</span>
        </span>
        <span className="line-clamp-4 border-l-2 border-secondary pl-2 font-mono text-xs whitespace-pre-wrap text-secondary [overflow-wrap:anywhere]">{excerptOf(item).slice(0, 800)}</span>
    </button>;
}

function FileAttachment({ item, onOpen }: { item: FrozenMaterial; onOpen: () => void }) {
    const { t, locale } = useI18n();
    const { material, ref } = item;
    const Icon = material.kind === "image" ? Image01 : material.kind === "text" ? File02 : FileAttachment03;
    const part = partOf(ref.selector, t);
    return <button type="button" onClick={onOpen} title={`${material.title} · ${material.mime}`} aria-label={t("materials.openAttachment", { title: material.title })}
        className="flex min-w-0 max-w-full items-center gap-2 rounded-lg border border-secondary bg-secondary px-2.5 py-1.5 text-left text-xs text-secondary hover:border-brand">
        <Icon aria-hidden="true" className="size-4 shrink-0 text-fg-quaternary" />
        <span className="min-w-0 truncate">{material.title}{part ? ` · ${part}` : ""}</span>
        <span className="shrink-0 tabular-nums text-quaternary">{bytes(material.size, locale)}</span>
    </button>;
}

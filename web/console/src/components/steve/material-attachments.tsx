import { useEffect, useState } from "react";
import { File02, FileAttachment03, FileCode01, Image01 } from "@untitledui/icons";
import { materialBlob, refKey } from "@/lib/api/material";
import { bytes } from "@/lib/format";
import type { DraftMaterial, FrozenMaterial, MaterialRef, MaterialSelector } from "@/lib/types";
import { useI18n } from "@/providers/locale-provider";
import { MaterialPreview } from "./material-shelf";

// What a line carried is drawn on the line, not named on it: a picture
// sent with a question shows as the picture, a cut of a file shows the
// lines that were cut, and anything the page cannot draw is still a
// thing with a name, a kind and a size rather than a bare title.

// Concurrent copies share a content read. Once the last copy leaves the
// page, its request and blob URL are retired rather than accumulating across
// conversations. Returning to that image deliberately fetches it again.
interface ThumbnailResource {
    controller: AbortController;
    promise: Promise<string>;
    users: number;
    url: string;
}
const thumbnails = new Map<string, ThumbnailResource>();
function acquireThumbnail(project: string, id: string) {
    const key = JSON.stringify([project, id]);
    let resource = thumbnails.get(key);
    if (!resource) {
        const current: ThumbnailResource = {
            controller: new AbortController(), promise: Promise.resolve(""), users: 0, url: "",
        };
        current.promise = materialBlob(project, id, current.controller.signal).then(blob => {
            // Some transports finish despite abort. An old completion cannot
            // allocate a URL or replace a new reader for the same identity.
            if (!current.users || current.controller.signal.aborted || thumbnails.get(key) !== current) return "";
            current.url = URL.createObjectURL(blob);
            return current.url;
        }).catch(error => {
            // A later consumer can retry a failed read even while an older
            // copy shows its fallback. Stale failures cannot evict new leases.
            if (thumbnails.get(key) === current) thumbnails.delete(key);
            throw error;
        });
        thumbnails.set(key, current);
        resource = current;
    }
    resource.users++;
    const owned = resource;
    let released = false;
    return {
        promise: owned.promise,
        release() {
            if (released) return;
            released = true;
            if (--owned.users) return;
            if (thumbnails.get(key) === owned) thumbnails.delete(key);
            owned.controller.abort();
            if (owned.url) URL.revokeObjectURL(owned.url);
        },
    };
}

function useThumbnail(project: string, id: string, enabled = true) {
    const [url, setURL] = useState("");
    const [failed, setFailed] = useState(false);
    useEffect(() => {
        let live = true;
        setURL(""); setFailed(false);
        if (!enabled) return;
        const lease = acquireThumbnail(project, id);
        void lease.promise.then(value => { if (live) setURL(value); }).catch(() => { if (live) setFailed(true); });
        return () => { live = false; lease.release(); };
    }, [project, id, enabled]);
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

export function DraftAttachment({ item, onOpen, onRemove }: { item: DraftMaterial; onOpen: () => void; onRemove: () => void }) {
    const { t, locale } = useI18n();
    const { url, failed } = useThumbnail(item.project, item.id, item.kind === "image");
    const Icon = item.kind === "image" ? Image01 : item.kind === "text" ? File02 : FileAttachment03;
    const part = partOf(item.selector, t);
    return <li className="flex min-w-0 max-w-full items-center gap-1.5 rounded-lg border border-secondary bg-secondary p-1">
        <button type="button" onClick={onOpen} title={`${item.title} · ${item.mime}`} aria-label={t("materials.openAttachment", { title: item.title })}
            className="flex min-w-0 flex-1 items-center gap-2 rounded-md px-1.5 py-1 text-left text-xs text-secondary hover:bg-primary">
            {item.kind === "image" && url && !failed
                ? <img src={url} alt={item.title} width={32} height={32} className="size-8 shrink-0 rounded object-contain" />
                : <Icon aria-hidden="true" className="size-4 shrink-0 text-fg-quaternary" />}
            <span className="min-w-0 truncate">{item.title}{part ? ` · ${part}` : ""}</span>
            <span className="shrink-0 tabular-nums text-quaternary">{t(`materials.${item.kind}`)} · {bytes(item.size, locale)}</span>
        </button>
        <button type="button" aria-label={t("materials.remove", { title: item.title })} onClick={onRemove}
            className="min-h-8 min-w-8 rounded-md px-1 text-quaternary hover:bg-primary hover:text-primary">×</button>
    </li>;
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

import { createContext, useEffect, useRef, useCallback, useContext, useState, type ReactNode } from "react";
import { useFleet } from "@/lib/fleet";
import { addDraftMaterial } from "@/lib/drafts";
import { refKey } from "@/lib/api/material";
import type { Material, MaterialRef, DraftMaterial, MaterialAnnotation } from "@/lib/types";
import { useI18n } from "./locale-provider";
import { AnnotationEditor } from "@/components/steve/annotation-editor";

export interface MaterialTarget { conversation: string; project: string; title: string }
interface Editor { material: Material; ref: MaterialRef; annotation?: MaterialAnnotation }
interface MaterialContextValue {
    target: MaterialTarget | null; setTarget: (target: MaterialTarget | null) => void;
    add: (material: Material, ref: MaterialRef, target: MaterialTarget) => Promise<void>;
    pin: (material: Material, ref: MaterialRef) => void; unpin: (project: string, ref: MaterialRef) => void;
    pins: (project: string) => DraftMaterial[]; annotate: (editor: Editor) => void;
    sideRequest: { project: string; nonce: number } | null; revision: number; changed: () => void;
}
const Context = createContext<MaterialContextValue | null>(null);
export function MaterialProvider({ children }: { children: ReactNode }) {
    const { snap } = useFleet(); const { t } = useI18n();
    const [target, setTarget] = useState<MaterialTarget | null>(null);
    const liveTarget=useRef(target);liveTarget.current=target;
    const [editor, setEditor] = useState<Editor | null>(null);
    const [revision, setRevision] = useState(0);
    const [sideRequest, setSideRequest] = useState<{ project: string; nonce: number } | null>(null);
    const [notice, setNotice] = useState("");
    useEffect(()=>{if(!notice)return;const timer=window.setTimeout(()=>setNotice(""),4000);return()=>window.clearTimeout(timer)},[notice]);
    const changed = useCallback(() => setRevision((value) => value + 1), []);
    const storageKey = (project: string) => `steve.material.pins:${window.location.origin}:${snap.hub.node}:${project}`;
    const pins = (project: string): DraftMaterial[] => { try { const saved = JSON.parse(localStorage.getItem(storageKey(project)) || "[]"); return Array.isArray(saved) ? saved.filter((item) => item && typeof item.id === "string" && item.project === project && typeof item.title === "string" && ["text", "image", "binary"].includes(item.kind)) : []; } catch { return []; } };
    const item = (material: Material, ref: MaterialRef): DraftMaterial => ({ ...ref, title: material.title, project: material.project, kind: material.kind, mime: material.mime, size: material.size });
    const add = async (material: Material, ref: MaterialRef, to: MaterialTarget) => {
        if (material.project !== to.project || (liveTarget.current?.conversation===to.conversation && liveTarget.current.project!==to.project)) throw new Error(t("materials.wrongProject"));
        if (!await addDraftMaterial(to.conversation, item(material, ref))) throw new Error(t("materials.sourceUnavailable"));
        setNotice(t("materials.added", { title: to.title }));
    };
    const pin = (material: Material, ref: MaterialRef) => {
        if (!snap.hub.node) throw new Error(t("materials.loading"));
        const current = pins(material.project);
        localStorage.setItem(storageKey(material.project), JSON.stringify([...current.filter((entry) => refKey(entry) !== refKey(ref)), item(material, ref)]));
        changed(); setSideRequest({ project: material.project, nonce: Date.now() }); setNotice(t("materials.pinned"));
    };
    const unpin = (project: string, ref: MaterialRef) => { localStorage.setItem(storageKey(project), JSON.stringify(pins(project).filter((entry) => refKey(entry) !== refKey(ref)))); changed(); };
    return <Context.Provider value={{ target, setTarget, add, pin, unpin, pins, annotate: setEditor, sideRequest, revision, changed }}>{children}
        {notice && <div role="status" className="pointer-events-none fixed bottom-4 left-1/2 -translate-x-1/2 z-[140] flex max-w-sm items-center gap-3 rounded-lg border border-secondary bg-primary p-3 text-sm text-primary shadow-lg"><span>{notice}</span><button type="button" className="pointer-events-auto" aria-label={t("materials.close")} onClick={() => setNotice("")}>×</button></div>}
        {editor && <AnnotationEditor key={editor.annotation?.id || refKey(editor.ref)} material={editor.material} anchor={editor.ref} annotation={editor.annotation} onClose={() => setEditor(null)} onSaved={() => { changed(); setNotice(t("materials.saved")); }} />}
    </Context.Provider>;
}
export function useMaterial() { const value = useContext(Context); if (!value) throw new Error("useMaterial requires MaterialProvider"); return value; }

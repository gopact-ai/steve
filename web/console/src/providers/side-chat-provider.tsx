import { createContext, useCallback, useContext, useRef, useState, type ReactNode } from "react";
import { addDraftMaterial } from "@/lib/drafts";
import { fetchContext } from "@/lib/api/console";
import { bindSideConversation } from "@/lib/side-chat";
import type { Locale } from "@/lib/i18n";
import { UnsentRequestError } from "@/lib/http";
import type { Material, MaterialRef } from "@/lib/types";
import { useI18n } from "./locale-provider";

export interface SideChatOpen { material: Material; ref: MaterialRef; excerpt: string; originConversation?: string }
export interface SideSession { id: string; project: string; title: string; excerpt: string; originConversation?: string; bindingCommandID: string; bindingLocale: Locale; bound: boolean; binding?: boolean; error?: string }
interface SideChatValue { session: SideSession | null; open: (input: SideChatOpen) => void; close: () => void; ensureBound: (session: SideSession) => Promise<void>; report: (id: string, error: string) => void }
const Context = createContext<SideChatValue | null>(null);
const storageKey = "steve.side-conversations";
function initial(): Record<string, SideSession> { try { const saved = JSON.parse(sessionStorage.getItem(storageKey) || "{}"); return Object.fromEntries(Object.entries(saved).filter(([project, item]) => item && typeof (item as SideSession).id === "string" && (item as SideSession).id.startsWith("console:side-") && (item as SideSession).project === project && typeof (item as SideSession).title === "string" && typeof (item as SideSession).excerpt === "string" && typeof (item as SideSession).bindingCommandID === "string").map(([project, item]) => [project, { ...(item as SideSession), binding: false, bindingLocale: (item as SideSession).bindingLocale === "zh" ? "zh" : "en" }])); } catch { return {}; } }
const unique = () => { try { return crypto.randomUUID(); } catch { return `${Date.now()}-${Math.random().toString(36).slice(2)}`; } };

export function SideChatProvider({ children }: { children: ReactNode }) {
    const { t, locale } = useI18n(); const [sessions, setSessions] = useState(initial); const current = useRef(sessions); const [opened, setOpened] = useState<string | null>(null);
    const bindings = useRef(new Map<string, Promise<void>>()); const returnFocus = useRef<HTMLElement | null>(null);
    const save = useCallback((next: Record<string, SideSession>) => { sessionStorage.setItem(storageKey, JSON.stringify(next)); current.current = next; setSessions(next); }, []);
    const patch = useCallback((id: string, value: Partial<SideSession>) => { const entry = Object.values(current.current).find((item) => item.id === id); if (entry) save({ ...current.current, [entry.project]: { ...entry, ...value } }); }, [save]);
    const open = useCallback((input: SideChatOpen) => {
        if (input.ref.id !== input.material.id) throw new Error(t("sideChat.invalidSource"));
        const project = input.material.project;
        const entry = (Object.hasOwn(current.current,project) ? current.current[project] : undefined) || { id: `console:side-${unique()}`, project, title: input.material.title, excerpt: "", originConversation: input.originConversation, bindingCommandID: `side-binding-${unique()}`, bindingLocale: locale, bound: false };
        const next = { ...entry, excerpt: input.excerpt.slice(0, 2000), error: undefined };
        save({ ...current.current, [project]: next });
        if (!addDraftMaterial(entry.id, { ...input.ref, title: input.material.title, project, kind: input.material.kind, mime: input.material.mime, size: input.material.size })) throw new Error(t("sideChat.storage"));
        const focused = document.activeElement; if (focused instanceof HTMLElement && !focused.closest("[data-side-chat]")) returnFocus.current = focused;
        setOpened(project);
    }, [save, t, locale]);
    const close = useCallback(() => { setOpened(null); queueMicrotask(() => { if (returnFocus.current?.isConnected) returnFocus.current.focus({ preventScroll: true }); }); }, []);
    const report = useCallback((id: string, error: string) => { try { patch(id, { error, binding: false }); } catch { /* The durable submission still retains its failure. */ } }, [patch]);
    const ensureBound = useCallback((session: SideSession): Promise<void> => {
        const existing = bindings.current.get(session.id); if (existing) return existing;
        const work = (async () => {
            const latest = Object.hasOwn(current.current,session.project) ? current.current[session.project] : undefined;
            if (!latest || latest.id !== session.id) throw new UnsentRequestError(t("sideChat.projectChanged"));
            patch(session.id, { binding: true, error: undefined });
            try {
                if (!latest.bound) await bindSideConversation(session.id, session.project, session.bindingCommandID, session.bindingLocale);
                const { context } = await fetchContext(session.id);
                if (context?.project?.id !== session.project || !context.project.bound) throw new UnsentRequestError(t(latest.bound ? "sideChat.projectChanged" : "sideChat.boundFailed"));
                patch(session.id, { bound: true });
            } finally { patch(session.id, { binding: false }); }
        })().finally(() => bindings.current.delete(session.id));
        bindings.current.set(session.id, work); return work;
    }, [patch, t]);
    return <Context.Provider value={{ session: opened ? sessions[opened] || null : null, open, close, ensureBound, report }}>{children}</Context.Provider>;
}
export function useSideChat() { const value = useContext(Context); if (!value) throw new Error("useSideChat requires SideChatProvider"); return value; }

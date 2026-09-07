import { createContext, useCallback, useContext, useRef, useState, type ReactNode } from "react";
import { addDraftMaterial } from "@/lib/drafts";
import { fetchContext } from "@/lib/api/console";
import { bindSideConversation, selectSideAgent } from "@/lib/side-chat";
import type { Locale } from "@/lib/i18n";
import { UnsentRequestError } from "@/lib/http";
import type { Material, MaterialRef } from "@/lib/types";
import { useI18n } from "./locale-provider";

export interface SideChatOpen { material: Material; ref: MaterialRef; excerpt: string; originConversation?: string }
export interface SideSession { id: string; project: string; title: string; excerpt: string; originConversation?: string; bindingCommandID: string; bindingLocale: Locale; selectedAgent?: string; agentCommandID?: string; agentReviewRequired?: boolean; agentPrepared?: boolean; bound: boolean; binding?: boolean; error?: string }
interface SideChatValue { session: SideSession | null; open: (input: SideChatOpen) => Promise<void>; close: () => void; ensureBound: (session: SideSession) => Promise<void>; acceptWorkbenchAgent: (session: SideSession) => Promise<void>; report: (id: string, error: string) => void }
const Context = createContext<SideChatValue | null>(null);
const storageKey = "steve.side-conversations";
function readSessions(): Record<string, SideSession> { const saved = JSON.parse(localStorage.getItem(storageKey) || "{}"); return Object.fromEntries(Object.entries(saved).filter(([project, item]) => item && typeof (item as SideSession).id === "string" && (item as SideSession).id.startsWith("console:side-") && (item as SideSession).project === project && typeof (item as SideSession).title === "string" && typeof (item as SideSession).excerpt === "string" && typeof (item as SideSession).bindingCommandID === "string").map(([project, item]) => [project, { ...(item as SideSession), binding: false, bindingLocale: (item as SideSession).bindingLocale === "zh" ? "zh" : "en" }])); }
function initial(): Record<string, SideSession> { try { return readSessions(); } catch { return {}; } }
const unique = () => { try { return crypto.randomUUID(); } catch { return `${Date.now()}-${Math.random().toString(36).slice(2)}`; } };

export function SideChatProvider({ children }: { children: ReactNode }) {
    const { t, locale } = useI18n(); const [sessions, setSessions] = useState(initial); const current = useRef(sessions); const [opened, setOpened] = useState<string | null>(null);
    const bindings = useRef(new Map<string, Promise<void>>()); const returnFocus = useRef<HTMLElement | null>(null);
    const change = useCallback(async (update: (latest: Record<string, SideSession>) => Record<string, SideSession>) => {
        if (!navigator.locks) throw new Error(t("console.draftLockUnavailable"));
        return navigator.locks.request(storageKey, () => {
            const next = update(readSessions());
            localStorage.setItem(storageKey, JSON.stringify(next)); current.current = next; setSessions(next);
            return next;
        });
    }, [t]);
    const patch = useCallback(async (id: string, value: Partial<SideSession>) => {
        await change((latest) => { const entry = Object.values(latest).find((item) => item.id === id); return entry ? { ...latest, [entry.project]: { ...entry, ...value } } : latest; });
    }, [change]);
    const open = useCallback(async (input: SideChatOpen) => {
        if (input.ref.id !== input.material.id) throw new Error(t("sideChat.invalidSource"));
        const project = input.material.project;
        const latest = await change((latest) => {
            const entry = (Object.hasOwn(latest, project) ? latest[project] : undefined) || { id: `console:side-${unique()}`, project, title: input.material.title, excerpt: "", originConversation: input.originConversation, bindingCommandID: `side-binding-${unique()}`, bindingLocale: locale, bound: false };
            return { ...latest, [project]: { ...entry, excerpt: input.excerpt.slice(0, 2000), error: undefined } };
        });
        const entry = latest[project];
        if (!await addDraftMaterial(entry.id, { ...input.ref, title: input.material.title, project, kind: input.material.kind, mime: input.material.mime, size: input.material.size })) throw new Error(t("sideChat.storage"));
        const focused = document.activeElement; if (focused instanceof HTMLElement && !focused.closest("[data-side-chat]")) returnFocus.current = focused;
        setOpened(project);
    }, [change, t, locale]);
    const close = useCallback(() => { setOpened(null); queueMicrotask(() => { if (returnFocus.current?.isConnected) returnFocus.current.focus({ preventScroll: true }); }); }, []);
    const report = useCallback((id: string, error: string) => { void patch(id, { error, binding: false }).catch(() => {
        const entry = Object.values(current.current).find((item) => item.id === id);
        if (entry) { const next = { ...current.current, [entry.project]: { ...entry, error, binding: false } }; current.current = next; setSessions(next); }
    }); }, [patch]);
    const ensureBound = useCallback((session: SideSession): Promise<void> => {
        const existing = bindings.current.get(session.id); if (existing) return existing;
        const work = (async () => {
            let latest = Object.hasOwn(current.current,session.project) ? current.current[session.project] : undefined;
            if (!latest || latest.id !== session.id) throw new UnsentRequestError(t("sideChat.projectChanged"));
            await patch(session.id, { binding: true, error: undefined });
            try {
                if (!latest.bound) await bindSideConversation(session.id, session.project, latest.bindingCommandID, latest.bindingLocale);
                let { context } = await fetchContext(session.id);
                if (context?.conversation !== session.id || context?.project?.id !== session.project || !context.project.bound) throw new UnsentRequestError(t(latest.bound ? "sideChat.projectChanged" : "sideChat.boundFailed"));
                if (!latest.agentPrepared) {
                    if (latest.agentReviewRequired) throw new UnsentRequestError(t("sideChat.chooseAgent"));
                    if (!latest.selectedAgent) {
                        const origin = latest.originConversation ? (await fetchContext(latest.originConversation)).context : null;
                        if (origin?.conversation !== latest.originConversation || !origin?.agent?.id || origin.project?.id !== latest.project) {
                            await patch(session.id, { agentReviewRequired: true });
                            throw new UnsentRequestError(t("sideChat.originAgentUnavailable"));
                        }
                        const selected = await change((stored) => {
                            const entry = stored[session.project];
                            if (!entry || entry.id !== session.id) throw new UnsentRequestError(t("sideChat.projectChanged"));
                            return entry.selectedAgent ? stored : { ...stored, [session.project]: { ...entry, selectedAgent: origin.agent!.id, agentCommandID: `side-agent-${unique()}` } };
                        });
                        latest = selected[session.project];
                    }
                    const agent = context.agents.find((item) => item.id === latest!.selectedAgent && item.usable);
                    if (!agent || !latest.agentCommandID) {
                        await patch(session.id, { agentReviewRequired: true });
                        throw new UnsentRequestError(t("sideChat.originAgentUnavailable"));
                    }
                    await selectSideAgent(session.id, agent.id, latest.agentCommandID, latest.bindingLocale);
                    context = (await fetchContext(session.id)).context;
                    if (context?.conversation !== session.id || context?.project?.id !== session.project || !context.project.bound || context.agent?.id !== agent.id || !context.agents.some((item) => item.id === agent.id && item.usable)) throw new UnsentRequestError(t("sideChat.agentSelectionUnconfirmed"));
                    await patch(session.id, { bound: true, agentPrepared: true });
                } else if (!context.agent || !context.agents.some((item) => item.id === context?.agent?.id && item.usable)) {
                    await patch(session.id, { agentReviewRequired: true });
                    throw new UnsentRequestError(t("sideChat.chooseAgent"));
                }
            } finally { await patch(session.id, { binding: false }); }
        })().finally(() => bindings.current.delete(session.id));
        bindings.current.set(session.id, work); return work;
    }, [change, patch, t]);
    const acceptWorkbenchAgent = useCallback(async (session: SideSession) => {
        const { context } = await fetchContext(session.id);
        if (context?.conversation !== session.id || context?.project?.id !== session.project || !context.project.bound || !context.agent || !context.agents.some((item) => item.id === context?.agent?.id && item.usable)) throw new UnsentRequestError(t("sideChat.chooseAgent"));
        await patch(session.id, { bound: true, agentPrepared: true, agentReviewRequired: false, error: undefined });
    }, [patch, t]);
    return <Context.Provider value={{ session: opened ? sessions[opened] || null : null, open, close, ensureBound, acceptWorkbenchAgent, report }}>{children}</Context.Provider>;
}
export function useSideChat() { const value = useContext(Context); if (!value) throw new Error("useSideChat requires SideChatProvider"); return value; }

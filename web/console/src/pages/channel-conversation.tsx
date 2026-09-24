import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { LayoutLeft, X } from "@untitledui/icons";
import { useNavigate } from "react-router";
import { SessionsTree } from "@/components/steve/sessions-tree";
import { AssistantMessage, UserMessage } from "@/components/steve/message";
import { IconButton } from "@/components/steve/icon-button";
import { Sheet } from "@/components/steve/drawer";
import { ThemeMenu } from "@/components/steve/theme-menu";
import { useChannelConversation } from "@/hooks/use-channel-conversation";
import { useBreakpoint } from "@/hooks/use-breakpoint";
import { useResourceRead } from "@/hooks/use-resource-read";
import { useFleet, useIntent } from "@/lib/fleet";
import { useI18n } from "@/providers/locale-provider";
import { useMaterial } from "@/providers/material-provider";
import { useSideChat } from "@/providers/side-chat-provider";
import { fetchConversations, deleteConversation, updateConversation } from "@/lib/api/console";
import { conversationExecution, conversationKey, conversationURL, type ConversationTransport } from "@/lib/conversation-identity";
import { channelReplyKey } from "@/lib/channel-history";
import { message } from "@/lib/http";
import type { Conversation } from "@/lib/types";

// No Console controller, drafts, composer or mutation-capable transcript
// extras are mounted on this side of the transport boundary.
export function ChannelConversationPane({ id }: { id: string }) {
    const { t } = useI18n();
    const navigate = useNavigate();
    const snap = useFleet((fleet) => fleet.snap);
    const { intent, consume } = useIntent();
    const materials = useMaterial();
    const side = useSideChat();
    const history = useChannelConversation(id);
    const [list, setList] = useState<Conversation[]>([]);
    const [listError, setListError] = useState("");
    const [offline, setOffline] = useState(!navigator.onLine);
    const desktop = useBreakpoint("lg");
    const [mobileSessions, setMobileSessions] = useState(false);
    const [collapsed, setCollapsed] = useState(() => localStorage.getItem("steve.sessions.collapsed") === "1");
    const scroll = useRef<HTMLDivElement>(null);
    const follow = useRef(true);
    const prepending = useRef<{ height: number; top: number } | null>(null);
    const loadList = useResourceRead("channel-directory", fetchConversations,
        (data) => { setList(data.conversations || []); setListError(""); }, (error) => setListError(message(error)));
    useEffect(() => {
        loadList();
        const timer = window.setInterval(loadList, 10000);
        return () => window.clearInterval(timer);
    }, [loadList]);
    useEffect(() => {
        sessionStorage.setItem("steve.conversation", id);
        sessionStorage.setItem("steve.conversation.transport", "feishu");
        materials.setTarget(null);
        side.close();
    }, [id, materials.setTarget, side.close]);
    // Commands triggered elsewhere while reading a channel must not be
    // replayed into the next Console conversation after navigation.
    useEffect(() => { if (intent) consume(intent.n); }, [intent, consume]);
    useEffect(() => {
        const changed = () => setOffline(!navigator.onLine);
        window.addEventListener("online", changed); window.addEventListener("offline", changed);
        return () => { window.removeEventListener("online", changed); window.removeEventListener("offline", changed); };
    }, []);
    useLayoutEffect(() => {
        const el = scroll.current;
        if (!el) return;
        if (prepending.current && !history.loadingEarlier) {
            el.scrollTop = prepending.current.top + el.scrollHeight - prepending.current.height;
            prepending.current = null;
        } else if (follow.current && !prepending.current) el.scrollTop = el.scrollHeight;
    }, [history.data, history.loadingEarlier]);
    const pick = (next: string, transport: ConversationTransport = "console") => {
        setMobileSessions(false); navigate(conversationURL(next, transport));
    };
    const toggle = () => { setCollapsed(!collapsed); localStorage.setItem("steve.sessions.collapsed", collapsed ? "0" : "1"); };
    const sessions = (small = false) => <SessionsTree list={list} projects={snap.projects} current={id} currentTransport="feishu" onPick={pick}
        collapsed={!small && collapsed} onToggle={small ? () => setMobileSessions(false) : toggle} resizable={!small}
        onNew={(project) => navigate(`/console?new=1${project ? `&project=${encodeURIComponent(project)}` : ""}`)}
        onUpdate={(next, patch) => { if (!list.some((c) => c.id === next && c.transport !== "feishu" && !c.read_only)) return; void updateConversation(next, patch).then(loadList).catch((error) => setListError(message(error))); }}
        onDelete={async (next) => { if (!list.some((c) => c.id === next && c.transport !== "feishu" && !c.read_only)) return; await deleteConversation(next); loadList(); }} />;
    const conversation = history.data?.conversation || list.find((item) => conversationKey(item) === conversationKey({ id, transport: "feishu" }));
    const execution = offline || history.error ? "unknown" : history.data ? conversationExecution(history.data.conversation) : undefined;
    const status = history.loading ? t("channel.loading") : execution ? t(execution === "unknown" ? "channel.executionUnknown" : execution === "running" ? "channel.running" : "channel.idle") : "";
    const answered = new Set(history.data?.replies.filter((reply) => reply.kind !== "sent").map((reply) => reply.exchange_id).filter(Boolean));
    return <div className="console-workbench">
        {desktop && sessions()}
        {!desktop && mobileSessions && <Sheet label={t("console.sessions")} side="left" width={300} onClose={() => setMobileSessions(false)}>
            <IconButton className="sheet-close" label={t("console.closeSessions")} onClick={() => setMobileSessions(false)} icon={X} />{sessions(true)}
        </Sheet>}
        <section className="console-main" data-channel-conversation aria-label={t("channel.readOnly")}>
            <header className="console-toolbar">
                <IconButton label={t("console.sessions")} onClick={() => desktop ? toggle() : setMobileSessions(true)} icon={LayoutLeft} />
                <div className="console-heading"><h1 title={conversation?.title || id}>{conversation?.title || id}</h1>
                    <div className="console-location break-words [overflow-wrap:anywhere]">{[conversation?.project, conversation?.agent, t("channel.readOnly")].filter(Boolean).join(" · ")}</div>
                </div>
                <span role="status" className="console-status">{status}</span>
                <ThemeMenu />
            </header>
            <div className="border-b border-secondary px-4 py-3 text-sm text-tertiary">{t("channel.hint")}</div>
            {(offline || history.error) && <div role="alert" className="border-b border-secondary bg-warning-primary px-4 py-3 text-sm text-secondary break-words">
                <p>{t(offline ? "channel.offline" : "channel.readError")}</p>{history.error && <p>{history.error}</p>}
            </div>}
            {listError && <p role="alert" className="border-b border-secondary px-4 py-2 text-sm text-error-primary">{t("channel.listError")} {listError}</p>}
            <div ref={scroll} tabIndex={0} aria-label={t("channel.readOnly")} className="transcript-scroll min-h-0 flex-1 overflow-y-auto overflow-x-hidden"
                onScroll={(event) => { const el = event.currentTarget; follow.current = el.scrollHeight - el.clientHeight - el.scrollTop < 48; }}>
                <div className="transcript-messages">
                    {history.reset && <p role="status" className="text-sm text-tertiary">{t("channel.reset")}</p>}
                    {history.data?.next_cursor && <button type="button" disabled={history.refreshing} className="min-h-10 self-center rounded px-3 text-sm text-secondary hover:bg-secondary disabled:opacity-50"
                        onClick={() => { if (scroll.current) prepending.current = { height: scroll.current.scrollHeight, top: scroll.current.scrollTop }; history.loadEarlier(); }}>{t(history.loadingEarlier ? "channel.loadingEarlier" : "channel.earlier")}</button>}
                    {history.pageError && <p role="alert" className="text-sm text-error-primary">{t("channel.pageError")} {history.pageError}</p>}
                    {history.loading && <p role="status" className="text-sm text-tertiary">{t("channel.loading")}</p>}
                    {!history.loading && history.data?.replies.length === 0 && <p className="text-sm text-tertiary">{t("channel.empty")}</p>}
                    {history.data?.replies.filter((reply) => !reply.silent).map((reply) => <div key={channelReplyKey(reply)} className="min-w-0">
                        {reply.kind === "sent" ? <UserMessage r={reply} readOnly /> : <AssistantMessage r={reply} readOnly />}
                        {reply.kind === "sent" && reply.exchange_id && !answered.has(reply.exchange_id) && <p className="mt-2 text-right text-xs text-tertiary">{t("channel.unknown")}</p>}
                        {reply.kind !== "sent" && (reply.delivery !== undefined && reply.delivery !== "confirmed" || !reply.text) && <p className="mt-2 text-xs text-tertiary">{t(reply.delivery === "suppressed" ? "channel.suppressed" : reply.delivery === "unavailable" ? "channel.unavailable" : reply.delivery === "unconfirmed" ? "channel.unconfirmed" : reply.delivery === "confirmed" ? "channel.unavailable" : "channel.unknown")}</p>}
                    </div>)}
                </div>
            </div>
        </section>
    </div>;
}

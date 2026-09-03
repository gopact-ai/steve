import { useEffect, useRef, useState } from "react";
import Markdown from "react-markdown";
import remarkBreaks from "remark-breaks";
import { MessageChatSquare, Send01 } from "@untitledui/icons";
import { Avatar } from "@/components/base/avatar/avatar";
import { Badge } from "@/components/base/badges/badges";
import { Button } from "@/components/base/buttons/button";
import { Select } from "@/components/base/select/select";
import { TextArea } from "@/components/base/textarea/textarea";
import { fetchReplies, send, when } from "@/lib/api";
import { useFleet, useIntent } from "@/lib/fleet";
import type { Reply } from "@/lib/types";
import { Nothing } from "@/lib/ui";

const verbs = ["/fleet", "/tasks", "/plans", "/project", "/plan ", "/project use ", "/grant ", "/effects", "@"];

export function ConsolePage() {
    const { snap, consoleEvents, refresh } = useFleet();
    const { intent } = useIntent();
    const [conversation, setConversation] = useState("console:main");
    const [entries, setEntries] = useState<Reply[]>([]);
    const [known, setKnown] = useState<string[]>([]);
    const [enabled, setEnabled] = useState(true);
    const [text, setText] = useState("");
    const [busy, setBusy] = useState(false);
    const [status, setStatus] = useState("");
    const box = useRef<HTMLTextAreaElement>(null);
    const bottom = useRef<HTMLDivElement>(null);
    const seen = useRef(0);
    const handled = useRef(0);

    useEffect(() => {
        void (async () => {
            try {
                const data = await fetchReplies(conversation);
                setEnabled(data.enabled);
                setEntries(data.replies || []);
                setKnown(data.conversations || []);
            } catch (e) { setStatus(String(e)); }
        })();
    }, [conversation]);

    useEffect(() => {
        const fresh = consoleEvents.slice(seen.current);
        seen.current = consoleEvents.length;
        const mine = fresh.filter((ev) => ev.conversation === conversation);
        if (!mine.length) return;
        setEntries((list) => {
            const next = [...list];
            for (const ev of mine) {
                const kind = ev.kind.slice("console.".length);
                const r: Reply = { at: ev.at, conversation, kind, title: ev.title, text: kind === "sent" ? "" : ev.text || "", input: kind === "sent" ? ev.text : undefined };
                if (!next.some((x) => x.at === r.at && x.kind === r.kind && (x.text === r.text || x.input === r.input))) next.push(r);
            }
            return next.slice(-200);
        });
    }, [consoleEvents, conversation]);

    useEffect(() => { bottom.current?.scrollIntoView({ block: "end" }); }, [entries]);

    useEffect(() => {
        if (!intent || intent.n === handled.current) return;
        handled.current = intent.n;
        if (intent.mode === "fill") { setText(intent.text + " "); box.current?.focus(); }
        else void submit(intent.text);
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [intent]);

    async function submit(line?: string) {
        const input = (line ?? text).trim();
        if (!input || busy) return;
        setText("");
        setBusy(true);
        setStatus("running…");
        try {
            await send(conversation, input);
            setStatus("");
        } catch (e) {
            setStatus(String(e).replace(/^Error: /, ""));
        } finally {
            setBusy(false);
            refresh();
            box.current?.focus();
        }
    }

    const conversations = Array.from(new Set(["console:main", ...known, ...snap.tasks.map((t) => t.channel || "").filter((c) => c.startsWith("console:"))])).sort();
    const items = conversations.map((c) => ({ id: c, label: c }));

    return (
        <div className="flex h-full flex-col">
            <header className="flex items-center gap-3 border-b border-secondary bg-primary px-6 py-3">
                <div className="w-56">
                    <Select aria-label="Conversation" size="sm" selectedKey={conversation} onSelectionChange={(k) => k && setConversation(String(k))} items={items}>
                        {(item) => <Select.Item id={item.id}>{item.label}</Select.Item>}
                    </Select>
                </div>
                <div className="flex flex-wrap gap-1.5">
                    {verbs.map((v) => (
                        <button key={v} type="button" onClick={() => { setText(v); box.current?.focus(); }}
                            className="rounded-md px-2 py-0.5 font-mono text-xs text-tertiary ring-1 ring-secondary ring-inset hover:bg-secondary hover:text-primary">
                            {v.trim()}
                        </button>
                    ))}
                </div>
                <span className="ml-auto text-xs text-tertiary">{status}</span>
            </header>

            <div className="min-h-0 flex-1 overflow-auto px-6 py-5">
                {!enabled && <Nothing icon={MessageChatSquare} title="The console is off">Set feishu.owner_open_id — the console acts as the owner.</Nothing>}
                {enabled && entries.length === 0 && (
                    <Nothing icon={MessageChatSquare} title="Nothing said here yet">Talk to the current agent, or start with a verb like /fleet.</Nothing>
                )}
                <div className="mx-auto flex max-w-4xl flex-col gap-5">
                    {entries.map((r, i) => <Message key={i} r={r} />)}
                    <div ref={bottom} />
                </div>
            </div>

            <div className="border-t border-secondary bg-primary px-6 py-4">
                <div className="mx-auto flex max-w-4xl items-end gap-3">
                    <TextArea
                        aria-label="Message"
                        textAreaRef={box}
                        value={text}
                        rows={2}
                        placeholder="说话就是派活；/plan、/project use、/tasks、/grant、/approve、/effects 和飞书里一样。Enter 发送，Shift+Enter 换行。"
                        onChange={(v) => setText(v)}
                        onKeyDown={(e) => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); void submit(); } }}
                        className="flex-1"
                    />
                    <Button size="lg" color="primary" iconTrailing={Send01} isLoading={busy} isDisabled={!text.trim()} onClick={() => void submit()}>
                        Send
                    </Button>
                </div>
            </div>
        </div>
    );
}

function Message({ r }: { r: Reply }) {
    if (r.kind === "sent") {
        return (
            <div className="flex justify-end">
                <div className="flex max-w-[80%] flex-col items-end gap-1">
                    <span className="text-xs text-quaternary">you · {when(r.at)}</span>
                    <div className="rounded-2xl rounded-tr-sm bg-brand-solid px-4 py-2.5 text-sm text-white shadow-xs whitespace-pre-wrap">{r.input}</div>
                </div>
            </div>
        );
    }
    const tone = r.error ? "error" : r.kind === "milestone" ? "success" : r.kind === "notice" ? "warning" : "gray";
    return (
        <div className="flex gap-3">
            <Avatar size="sm" initials="S" alt="steve" className="mt-5 shrink-0" />
            <div className="flex max-w-[85%] flex-col gap-1">
                <div className="flex items-center gap-2 text-xs text-quaternary">
                    <span>steve · {when(r.at)}</span>
                    {r.kind !== "reply" && <Badge type="pill-color" size="sm" color={tone}>{r.kind}</Badge>}
                    {r.error && <Badge type="pill-color" size="sm" color="error">error</Badge>}
                </div>
                <div className={`rounded-2xl rounded-tl-sm bg-primary px-4 py-3 shadow-xs ring-1 ring-secondary ring-inset ${r.error ? "ring-error" : ""}`}>
                    {r.title && <div className="mb-1 text-sm font-semibold text-primary">{r.title}</div>}
                    <div className="md prose prose-sm max-w-none">
                        <Markdown remarkPlugins={[remarkBreaks]}>{r.text}</Markdown>
                    </div>
                </div>
            </div>
        </div>
    );
}

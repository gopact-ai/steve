import { useEffect, useRef, useState } from "react";
import { fetchReplies, send, when } from "../api";
import type { Event, Reply, Snapshot } from "../types";

const hints = ["/fleet", "/tasks", "/plans", "/project", "/plan ", "/project use ", "/grant ", "/effects", "@"];

export function Console({ snap, events, prefill, run, onRefresh }: {
  snap: Snapshot; events: Event[]; prefill: { text: string; n: number }; run: { text: string; n: number }; onRefresh: () => void;
}) {
  const [conversation, setConversation] = useState("console:main");
  const [entries, setEntries] = useState<Reply[]>([]);
  const [enabled, setEnabled] = useState(true);
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("");
  const box = useRef<HTMLTextAreaElement>(null);
  const bottom = useRef<HTMLDivElement>(null);
  const seen = useRef(0);

  useEffect(() => {
    void (async () => {
      try {
        const data = await fetchReplies(conversation);
        setEnabled(data.enabled);
        setEntries(data.replies || []);
      } catch (e) { setStatus(String(e)); }
    })();
  }, [conversation]);

  // Console events arrive on the stream too: milestones and notices that
  // nobody typed show up without a reload.
  useEffect(() => {
    const fresh = events.slice(seen.current);
    seen.current = events.length;
    const mine = fresh.filter((ev) => ev.conversation === conversation);
    if (mine.length === 0) return;
    setEntries((list) => {
      const next = [...list];
      for (const ev of mine) {
        const kind = ev.kind.slice("console.".length);
        const r: Reply = { at: ev.at, conversation, kind, title: ev.title, text: kind === "sent" ? "" : ev.text || "", input: kind === "sent" ? ev.text : undefined };
        const dup = next.some((x) => x.at === r.at && x.kind === r.kind && (x.text === r.text || x.input === r.input));
        if (!dup) next.push(r);
      }
      return next.slice(-200);
    });
  }, [events, conversation]);

  useEffect(() => { bottom.current?.scrollIntoView({ block: "end" }); }, [entries]);
  useEffect(() => { if (prefill.n) { setText(prefill.text + " "); box.current?.focus(); } }, [prefill]);
  useEffect(() => { if (run.n) void submit(run.text); /* eslint-disable-line react-hooks/exhaustive-deps */ }, [run]);

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
      onRefresh();
      box.current?.focus();
    }
  }

  const conversations = Array.from(new Set(["console:main", ...snap.tasks.map((t) => t.channel || "").filter((c) => c.startsWith("console:"))]));

  return (
    <div className="console">
      <div className="convbar">
        <select value={conversation} onChange={(e) => setConversation(e.target.value)}>
          {conversations.map((c) => <option key={c} value={c}>{c}</option>)}
          <option value="__new">+ new conversation…</option>
        </select>
        {conversation === "__new" && (
          <input autoFocus placeholder="name" onKeyDown={(e) => {
            if (e.key === "Enter") setConversation("console:" + (e.target as HTMLInputElement).value.trim().replace(/\s+/g, "-"));
          }} />
        )}
        <div className="hints">
          {hints.map((h) => <button key={h} className="hint" onClick={() => { setText(h); box.current?.focus(); }}>{h.trim()}</button>)}
        </div>
        <span className="grow" />
        <span className="status">{status}</span>
      </div>
      <div className="transcript">
        {!enabled && <div className="empty">The console is off: set feishu.owner_open_id — it acts as the owner.</div>}
        {enabled && entries.length === 0 && <div className="empty">Nothing said here yet. Talk to the current agent, or use a verb.</div>}
        {entries.map((r, i) => <Message key={i} r={r} />)}
        <div ref={bottom} />
      </div>
      <div className="composer">
        <textarea ref={box} value={text} rows={2} placeholder="说话就是派活；/plan、/project use、/tasks、/grant、/approve、/effects 和飞书里一样。Enter 发送，Shift+Enter 换行。"
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); void submit(); } }} />
        <button disabled={busy || !text.trim()} onClick={() => void submit()}>{busy ? "…" : "Send"}</button>
      </div>
    </div>
  );
}

function Message({ r }: { r: Reply }) {
  const cls = r.kind === "sent" ? "sent" : r.error ? "err" : r.kind;
  return (
    <div className={`msg ${cls}`}>
      <div className="stamp"><span>{when(r.at)}</span><span>{r.kind === "sent" ? "you" : r.kind}</span></div>
      <div className="bubble">
        {r.title && r.kind !== "sent" && <div className="title">{r.title}</div>}
        <pre>{r.kind === "sent" ? r.input : r.text}</pre>
      </div>
    </div>
  );
}

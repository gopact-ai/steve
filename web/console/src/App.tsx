import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { eventsURL, fetchState, token, when } from "./api";
import type { Event, Snapshot } from "./types";
import { Console } from "./views/Console";
import { Fleet } from "./views/Fleet";
import { Tasks } from "./views/Tasks";
import { Plans } from "./views/Plans";
import { Ledger } from "./views/Ledger";
import { Activity } from "./views/Activity";

type View = "console" | "fleet" | "tasks" | "plans" | "ledger" | "activity";

const empty: Snapshot = {
  at: "", hub: { node: "", started: "" }, nodes: [], agents: [], tasks: [], plans: [], attempts: [], landings: [],
  facts: { reservations: [], attestations: [], replicas: [], disclosures: [], effects: [], grants: [] },
};

export function App() {
  const [view, setView] = useState<View>(() => (window.location.hash.slice(1) as View) || "console");
  const [snap, setSnap] = useState<Snapshot>(empty);
  const [live, setLive] = useState<"connecting" | "live" | "reconnecting" | "unauthorized">("connecting");
  const [events, setEvents] = useState<Event[]>([]);
  const [consoleEvents, setConsoleEvents] = useState<Event[]>([]);
  const [prefill, setPrefill] = useState<{ text: string; n: number }>({ text: "", n: 0 });
  const [run, setRun] = useState<{ text: string; n: number }>({ text: "", n: 0 });
  const pending = useRef<number | null>(null);

  const refresh = useCallback(async () => {
    try {
      setSnap(await fetchState());
      setLive((s) => (s === "unauthorized" ? "connecting" : s));
    } catch (e) {
      if (String(e).includes("401") || String(e).includes("unauthorized")) setLive("unauthorized");
    }
  }, []);

  const scheduleRefresh = useCallback(() => {
    if (pending.current) return;
    pending.current = window.setTimeout(() => { pending.current = null; void refresh(); }, 250);
  }, [refresh]);

  useEffect(() => {
    void refresh();
    const floor = window.setInterval(() => void refresh(), 10000);
    let source: EventSource | null = null;
    let retry: number | null = null;
    const connect = () => {
      source = new EventSource(eventsURL());
      source.onopen = () => setLive("live");
      source.onmessage = (e) => {
        const ev = JSON.parse(e.data) as Event;
        if (ev.kind.startsWith("console.")) setConsoleEvents((list) => [...list.slice(-199), ev]);
        else setEvents((list) => [ev, ...list].slice(0, 300));
        scheduleRefresh();
      };
      source.onerror = () => {
        setLive("reconnecting");
        source?.close();
        retry = window.setTimeout(connect, 3000);
      };
    };
    connect();
    return () => { window.clearInterval(floor); source?.close(); if (retry) window.clearTimeout(retry); };
  }, [refresh, scheduleRefresh]);

  useEffect(() => { window.location.hash = view; }, [view]);

  // act runs a verb from any view and jumps to the console to watch it.
  const act = useCallback((text: string) => { setRun({ text, n: Date.now() }); setView("console"); }, []);
  const fill = useCallback((text: string) => { setPrefill({ text, n: Date.now() }); setView("console"); }, []);

  const counts = useMemo(() => ({
    fleet: snap.nodes.length + snap.agents.length,
    tasks: snap.tasks.filter((t) => t.state === "running" || t.state === "blocked" || t.state === "review").length,
    plans: snap.plans.length,
    ledger: snap.facts.disclosures.length + snap.facts.effects.length,
    activity: events.length,
  }), [snap, events]);

  const attention = snap.facts.disclosures.length + snap.facts.effects.length;

  return (
    <div className="shell">
      <aside className="side">
        <div className="brand"><b>steve</b><span>console</span></div>
        <nav className="nav">
          {([
            ["console", "Console", null],
            ["fleet", "Fleet", counts.fleet],
            ["tasks", "Tasks", counts.tasks || null],
            ["plans", "Plans", counts.plans || null],
            ["ledger", "Ledger", attention || null],
            ["activity", "Activity", counts.activity || null],
          ] as [View, string, number | null][]).map(([id, label, n]) => (
            <button key={id} className={view === id ? "on" : ""} onClick={() => setView(id)}>
              <span>{label}</span>{n ? <span className="count">{n}</span> : null}
            </button>
          ))}
        </nav>
        <div className="foot">
          <div><span className={`dot ${live === "live" ? "on" : live === "unauthorized" ? "off" : ""}`} />{live}</div>
          <div>hub <b>{snap.hub.node || "—"}</b></div>
          <div>{snap.nodes.filter((n) => n.up).length}/{snap.nodes.length} nodes up · {snap.attempts.length} running</div>
          <div>{snap.at ? when(snap.at) : ""}{token ? "" : " · no token"}</div>
        </div>
      </aside>
      <main className="main">
        <header className="top">
          <h1>{titleOf(view)}</h1>
          <span className="meta">{subtitleOf(view, snap)}</span>
          <span className="grow" />
          {live === "unauthorized" && <span className="pill pill-bad">token rejected — open the page with ?token=…</span>}
        </header>
        <div className="content">
          {view === "console" && <Console snap={snap} events={consoleEvents} prefill={prefill} run={run} onRefresh={scheduleRefresh} />}
          {view === "fleet" && <Fleet snap={snap} onFill={fill} />}
          {view === "tasks" && <Tasks snap={snap} onAct={act} />}
          {view === "plans" && <Plans snap={snap} onAct={act} />}
          {view === "ledger" && <Ledger snap={snap} onAct={act} />}
          {view === "activity" && <Activity events={events} />}
        </div>
      </main>
    </div>
  );
}

function titleOf(v: View) {
  return { console: "Console", fleet: "Fleet", tasks: "Tasks", plans: "Plans", ledger: "Ledger", activity: "Activity" }[v];
}

function subtitleOf(v: View, s: Snapshot) {
  switch (v) {
    case "console": return "the same verbs as the chat, as the owner";
    case "fleet": return `${s.nodes.length} nodes · ${s.agents.length} agents`;
    case "tasks": return `${s.tasks.length} tasks`;
    case "plans": return `${s.plans.length} plans · ${s.landings.length} landings`;
    case "ledger": return `${s.attempts.length} attempts live · ${s.facts.disclosures.length} awaiting approval · ${s.facts.effects.length} unknown effects`;
    case "activity": return "what the fleet is doing, as it happens";
  }
}

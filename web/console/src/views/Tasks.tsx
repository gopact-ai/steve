import type { Snapshot, Task } from "../types";
import { Card, Empty, Pill, Table, Where, toneOf } from "../ui";

export function Tasks({ snap, onAct }: { snap: Snapshot; onAct: (t: string) => void }) {
  const byID = new Map(snap.tasks.map((t) => [t.id, t]));
  const roots = snap.tasks.filter((t) => !t.parent || !byID.has(t.parent));
  const rows: [Task, number][] = [];
  const walk = (t: Task, depth: number) => {
    rows.push([t, depth]);
    snap.tasks.filter((c) => c.parent === t.id).forEach((c) => walk(c, depth + 1));
  };
  roots.forEach((t) => walk(t, 0));
  const holds = (s: string) => ["running", "blocked", "review", "paused", "draft"].includes(s);
  return (
    <Card title="Tasks" aside="budget is turns and elapsed; children draw on their parent">
      {rows.length === 0 ? <Empty>No tasks yet.</Empty> :
        <Table head={["task", "state", "goal", "member", "where", "project", "turns", "elapsed", ""]} rows={rows.map(([t, depth]) => {
          const pct = t.max_turns ? Math.min(100, Math.round((100 * t.turns) / t.max_turns)) : 0;
          return [
            <span style={{ paddingLeft: depth * 16 }}>{depth ? "└ " : ""}<b>#{t.id}</b></span>,
            <Pill tone={toneOf(t.state)}>{t.state}</Pill>,
            <span>{t.goal}</span>,
            t.member || "—",
            <Where node={t.node} />,
            <span className="dim">{t.project_id || "—"}</span>,
            <div><span className="mono">{t.turns}/{t.max_turns}</span><div className="bar"><i className={pct > 80 ? "hot" : ""} style={{ width: pct + "%" }} /></div></div>,
            <span className="dim">{t.elapsed || ""}{t.max_elapsed ? ` / ${t.max_elapsed}` : ""}</span>,
            holds(t.state) ? <span>
              {t.state === "paused"
                ? <button className="act" onClick={() => onAct(`/tasks resume ${t.id}`)}>resume</button>
                : <button className="act" onClick={() => onAct(`/tasks pause ${t.id}`)}>pause</button>}
              <button className="act danger" onClick={() => onAct(`/tasks cancel ${t.id}`)}>cancel</button>
              <button className="act" onClick={() => onAct(`/tasks ${t.id}`)}>detail</button>
            </span> : <button className="act" onClick={() => onAct(`/tasks ${t.id}`)}>detail</button>,
          ];
        })} />}
    </Card>
  );
}

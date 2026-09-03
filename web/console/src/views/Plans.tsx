import type { Snapshot, Step } from "../types";
import { Card, Empty, Pill, Table, Where, toneOf } from "../ui";
import { short, when } from "../api";

export function Plans({ snap, onAct }: { snap: Snapshot; onAct: (t: string) => void }) {
  return (
    <div>
      {snap.plans.length === 0 && <Card title="Plans"><Empty>No plans yet — try <code>/plan</code> a goal in the console.</Empty></Card>}
      {snap.plans.map((p) => (
        <Card key={p.id} title={<span>plan {p.id} <span className="dim">rev {p.rev}</span> · {p.goal}</span>}
          aside={<span>task #{p.task_id} · by {p.by} · {p.because}{p.base ? ` · base ${short(p.base)}` : ""} <button className="act" onClick={() => onAct(`/plans ${p.id}`)}>revisions</button></span>}>
          <div className="steps">{p.steps.map((s) => <StepRow key={s.id} s={s} />)}</div>
        </Card>
      ))}
      <Card title="Landings" aside="results merged into a canonical workspace">
        {snap.landings.length === 0 ? <Empty>Nothing landed yet.</Empty> :
          <Table head={["when", "project", "state", "artifact", "paths", "note"]} rows={snap.landings.map((l) => [
            <span className="dim">{when(l.at)}</span>, l.project, <Pill tone={toneOf(l.state)}>{l.state}</Pill>,
            <code className="mono">{short(l.artifact)}</code>, <span className="num">{l.paths}</span>, <span style={{ color: "var(--bad)" }}>{l.error || ""}</span>,
          ])} />}
      </Card>
    </div>
  );
}

function StepRow({ s }: { s: Step }) {
  const after = [...(s.needs || []), ...(s.merge || [])];
  return (
    <div className="step">
      <div><Pill tone={toneOf(s.state)}>{s.state}</Pill></div>
      <div className="id">{s.id}{s.attempts && s.attempts > 1 ? <span className="dim"> ×{s.attempts}</span> : null}<br />
        <span className="dim">{s.agent || "—"} @ </span><Where node={s.node} /></div>
      <div>
        <div>{s.goal}</div>
        <div className="after">
          {after.length ? `after ${after.join(", ")} · ` : ""}{(s.requires || []).length ? `requires ${s.requires!.join(", ")} · ` : ""}
          verify {s.verify || "—"}{s.artifact ? ` · artifact ${short(s.artifact)}` : ""}
        </div>
        {s.error && <div className="err">{s.error}</div>}
        {s.context && (s.context.refs?.length || s.context.findings?.length) ? (
          <div className="after">{s.context.refs?.length ? `refs ${s.context.refs.join(", ")}` : ""}{s.context.findings?.length ? ` · findings ${s.context.findings.join(" | ")}` : ""}</div>
        ) : null}
      </div>
    </div>
  );
}

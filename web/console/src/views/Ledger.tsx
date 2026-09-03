import type { Snapshot } from "../types";
import { Card, Empty, Pill, Table, toneOf } from "../ui";
import { short, when } from "../api";

export function Ledger({ snap, onAct }: { snap: Snapshot; onAct: (t: string) => void }) {
  const f = snap.facts;
  return (
    <div className="grid two">
      <Card title="Disclosures awaiting the owner" aside="sealed content leaves only with approval">
        {f.disclosures.length === 0 ? <Empty>Nothing waiting.</Empty> :
          <Table head={["id", "project", "task", "requester", "bytes", "since", ""]} rows={f.disclosures.map((d) => [
            <code className="mono">{d.id}</code>, d.project, d.task_id ? `#${d.task_id}` : "—", d.requester, <span className="num">{d.bytes}</span>, <span className="dim">{when(d.at)}</span>,
            <span><button className="act primary" onClick={() => onAct(`/approve ${d.id}`)}>approve</button><button className="act danger" onClick={() => onAct(`/deny ${d.id}`)}>deny</button></span>,
          ])} />}
      </Card>
      <Card title="Effects with an unknown outcome" aside="a person decides; the same call stays blocked until then">
        {f.effects.length === 0 ? <Empty>None.</Empty> :
          <Table head={["id", "tool", "task", "attempt", "error", "at", ""]} rows={f.effects.map((e) => [
            <code className="mono">{e.id}</code>, e.tool, `#${e.task_id}`, <code className="mono">{e.attempt}</code>, <span style={{ color: "var(--bad)" }}>{e.error || ""}</span>, <span className="dim">{when(e.at)}</span>,
            <span><button className="act" onClick={() => onAct(`/effects ${e.id} happened`)}>happened</button><button className="act" onClick={() => onAct(`/effects ${e.id} new`)}>new</button></span>,
          ])} />}
      </Card>
      <Card title="Reservations" aside="capacity held for steps not yet started">
        {f.reservations.length === 0 ? <Empty>No capacity reserved.</Empty> :
          <Table head={["id", "endpoint", "for", "region", "expires"]} rows={f.reservations.map((r) => [
            <code className="mono">{r.id}</code>, <code className="mono">{r.endpoint}</code>, r.for, <span className="dim">{r.region || "—"}</span>, <span className="dim">{when(r.expires_at)}</span>,
          ])} />}
      </Card>
      <Card title="Grants" aside="owner is admin everywhere; the rest is granted">
        {f.grants.length === 0 ? <Empty>No grants; the projects' defaults apply.</Empty> :
          <Table head={["project", "principal", "role", "by"]} rows={f.grants.map((g) => [g.project, <code className="mono">{g.principal}</code>, <Pill tone="muted">{g.role}</Pill>, <span className="dim">{g.by}</span>])} />}
      </Card>
      <Card title="Attestations" aside="verdicts on record before a name is bound">
        {f.attestations.length === 0 ? <Empty>No verdicts yet.</Empty> :
          <Table head={["artifact", "step", "kind", "verifier", "verdict", "attempt", "at"]} rows={f.attestations.map((a, i) => [
            <code className="mono" key={i}>{short(a.artifact)}</code>, a.step || "—", a.kind, <span className="muted">{a.verifier}</span>, <Pill tone={toneOf(a.verdict)}>{a.verdict}</Pill>, <code className="mono">{a.attempt}</code>, <span className="dim">{when(a.at)}</span>,
          ])} />}
      </Card>
      <Card title="Replicas" aside="copies on nodes, by node generation">
        {f.replicas.length === 0 ? <Empty>No copies on nodes.</Empty> :
          <Table head={["artifact", "node", "generation", "state", "note", "at"]} rows={f.replicas.map((r, i) => [
            <code className="mono" key={i}>{short(r.artifact)}</code>, r.node, <span className="num">{r.generation}</span>, <Pill tone={toneOf(r.state)}>{r.state}</Pill>, <span className="dim">{r.note || ""}</span>, <span className="dim">{when(r.at)}</span>,
          ])} />}
      </Card>
    </div>
  );
}

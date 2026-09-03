import type { Snapshot } from "../types";
import { Card, Empty, Pill, Table, Where, toneOf } from "../ui";
import { when } from "../api";

export function Fleet({ snap, onFill }: { snap: Snapshot; onFill: (t: string) => void }) {
  return (
    <div className="grid two">
      <Card title="Nodes" aside={`${snap.nodes.filter((n) => n.up).length} up`}>
        {snap.nodes.length === 0 ? <Empty>Hub only: no remote nodes configured.</Empty> :
          <Table head={["node", "state", "level", "region", "capabilities", "harnesses", "since"]} rows={snap.nodes.map((n) => [
            <b>{n.name}</b>,
            <Pill tone={n.up ? "ok" : "bad"}>{n.up ? "up" : "down"}</Pill>,
            n.level || "internal",
            <span className="dim">{n.region || "—"}</span>,
            <div className="tags">{(n.capabilities || []).map((c) => <span key={c} className="tag">{c}</span>)}</div>,
            <div>{(n.harnesses || []).map((h) => (
              <div key={h.id}>{h.missing ? <span style={{ color: "var(--bad)" }}>{h.id} ✗</span> : h.id}
                {h.slots ? <span className="dim"> · {h.slots} slots</span> : null}
                {h.models?.length ? <span className="dim"> ({h.models.join(", ")})</span> : null}
              </div>))}</div>,
            <span className="dim">{n.up ? when(n.since) : n.last_error}</span>,
          ])} />}
      </Card>
      <Card title="Agents" aside="agent = machine + harness + model">
        <Table head={["", "agent", "state", "where", "harness", "model", "level", "requires", "why"]} rows={snap.agents.map((a) => [
          <button className="act" onClick={() => onFill("@" + a.id)}>@</button>,
          <b>{a.id}</b>,
          <Pill tone={a.eligible ? "ok" : "warn"}>{a.eligible ? "ready" : "blocked"}</Pill>,
          <Where node={a.node} />,
          a.harness,
          <span className="dim">{a.model || "—"}</span>,
          <span className="dim">{a.level || "internal"}{a.slots ? ` · ${a.slots} slots` : ""}{a.region ? ` · ${a.region}` : ""}</span>,
          <div className="tags">{(a.requires || []).map((c) => <span key={c} className="tag">{c}</span>)}</div>,
          <span style={{ color: "var(--bad)" }}>{a.why || ""}</span>,
        ])} />
      </Card>
      <Card title="Attempts in flight" aside={`${snap.attempts.length}`} className="span">
        {snap.attempts.length === 0 ? <Empty>Nothing running.</Empty> :
          <Table head={["attempt", "kind", "state", "agent", "where", "project", "scope", "leases", "since"]} rows={snap.attempts.map((a) => [
            <code className="mono">{a.id}</code>, a.kind, <Pill tone={toneOf(a.state)}>{a.state}</Pill>, a.agent || "—", <Where node={a.node} />,
            a.project, <span className="dim">{a.scope}</span>,
            <div className="tags">{(a.leases || []).map((l) => <span key={l} className="tag mono">{l}</span>)}</div>,
            <span className="dim">{when(a.started_at)}</span>,
          ])} />}
      </Card>
    </div>
  );
}

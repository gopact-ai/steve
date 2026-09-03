import type { ReactNode } from "react";

export type Tone = "ok" | "bad" | "busy" | "muted" | "warn";

export function Pill({ tone, children }: { tone: Tone; children: ReactNode }) {
  return <span className={`pill pill-${tone}`}>{children}</span>;
}

export function toneOf(state: string): Tone {
  switch (state) {
    case "done": case "bound": case "committed": case "verified": case "up": case "ready": case "pass": case "succeeded":
      return "ok";
    case "failed": case "down": case "lost": case "expired": case "bind-conflict": case "fail":
    case "merge-conflicted": case "apply-conflicted": case "commit-conflicted": case "cancelled": case "outcome-unknown":
      return "bad";
    case "running": case "applying": case "transferring": case "present": case "prepared": case "leased": case "verifying":
      return "busy";
    case "quarantined": case "paused": case "blocked": case "review": case "recovery-pending":
      return "warn";
    default:
      return "muted";
  }
}

export function Card({ title, aside, children, className }: { title?: ReactNode; aside?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={`card ${className ?? ""}`}>
      {(title || aside) && (
        <header className="card-head">
          <div className="card-title">{title}</div>
          <div className="card-aside">{aside}</div>
        </header>
      )}
      {children}
    </section>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>;
}

export function Table({ head, rows }: { head: string[]; rows: ReactNode[][] }) {
  if (rows.length === 0) return null;
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>{head.map((h) => <th key={h}>{h}</th>)}</tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr key={i}>{r.map((c, j) => <td key={j}>{c}</td>)}</tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function Mono({ children }: { children: ReactNode }) {
  return <code className="mono">{children}</code>;
}

export function Where({ node }: { node?: string }) {
  return <span className="where">{node || "hub"}</span>;
}

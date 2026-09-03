import type { Event } from "../types";
import { Card, Empty } from "../ui";
import { when } from "../api";

export function Activity({ events }: { events: Event[] }) {
  return (
    <Card title="Activity" aside="the change stream, newest first">
      {events.length === 0 ? <Empty>Quiet. Events appear here as steps start, finish, and land.</Empty> :
        <div className="feed">{events.map((ev, i) => (
          <div key={i}><span className="dim">{when(ev.at)}</span> <span className="k">{ev.kind}</span>
            {ev.plan_id ? ` plan ${ev.plan_id}` : ""}{ev.step_id ? ` · ${ev.step_id}` : ""}{ev.state ? ` → ${ev.state}` : ""}
            {ev.detail ? <span className="dim"> {ev.detail}</span> : null}</div>
        ))}</div>}
    </Card>
  );
}

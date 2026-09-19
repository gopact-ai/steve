import { useEffect, useRef } from "react";
import { useFleet } from "@/lib/fleet";
import { managementDependencyKey, managementEventAffects, type ManagementDomain } from "@/lib/management-refresh";

export function useManagementRefresh(domain: ManagementDomain, load: () => Promise<void>) {
    const { snap, events, live } = useFleet();
    const dependency = managementDependencyKey(domain, snap);
    const head = useRef(events[0]);
    const seen = useRef(new Set<string>());
    const previousLive = useRef(live);

    useEffect(() => { void load(); }, [load, dependency]);
    useEffect(() => {
        let changed = false;
        for (const event of events) {
            if (event === head.current) break;
            if (!managementEventAffects(domain, event)) continue;
            const key = JSON.stringify(event);
            if (seen.current.has(key)) continue;
            seen.current.add(key);
            changed = true;
        }
        head.current = events[0];
        while (seen.current.size > 32) seen.current.delete(seen.current.values().next().value!);
        if (changed) void load();
    }, [domain, events, load]);
    useEffect(() => {
        if (live === "live" && previousLive.current === "reconnecting") void load();
        previousLive.current = live;
    }, [live, load]);
    useEffect(() => {
        // No owner revision/event covers external config edits or probes yet.
        // Recovery is page-local, not coupled to fleet observation/traffic.
        const timer = window.setInterval(() => { if (!document.hidden) void load(); }, 30000);
        const visible = () => { if (!document.hidden) void load(); };
        document.addEventListener("visibilitychange", visible);
        return () => { window.clearInterval(timer); document.removeEventListener("visibilitychange", visible); };
    }, [load]);
}

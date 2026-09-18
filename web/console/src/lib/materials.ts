import { useEffect, useState } from "react";
import { listMaterials } from "./api/material";
import type { Material } from "./types";

// A project's materials are read once and held, so the rail can badge
// the tab without a second request when the shelf opens.
const cache = new Map<string, Material[]>();

// useProjectMaterials is what a project has frozen: uploads, captures
// from a file or a reply. It is the shelf's real contents — pinning is
// only this browser's shortlist on top of it.
export function useProjectMaterials(project?: string, revision = 0) {
    const [materials, setMaterials] = useState<Material[]>(() => (project ? cache.get(project) ?? [] : []));
    const [error, setError] = useState("");
    const [loaded, setLoaded] = useState(() => !!project && cache.has(project));
    useEffect(() => {
        if (!project) { setMaterials([]); setLoaded(false); return; }
        setMaterials(cache.get(project) ?? []);
        setLoaded(cache.has(project));
        const stop = new AbortController();
        listMaterials(project, stop.signal)
            .then((data) => {
                const list = data.materials || [];
                cache.set(project, list);
                setMaterials(list);
                setError("");
                setLoaded(true);
            })
            .catch((e: unknown) => { if (!stop.signal.aborted) setError(e instanceof Error ? e.message : String(e)); });
        return () => stop.abort();
    }, [project, revision]);
    return { materials, error, loaded };
}

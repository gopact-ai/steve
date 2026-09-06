import type { SourceHealth } from "./types";

// A detailed contribution supersedes the aggregate ledger health. Older
// snapshots expose only the aggregate; a missing source is not a failure.
export function unavailableSource(sources: SourceHealth[], name: string): SourceHealth | undefined {
    const source = sources.find((source) => source.name === name) ?? sources.find((source) => source.name === "ledger");
    return source && (!source.wired || source.error) ? source : undefined;
}

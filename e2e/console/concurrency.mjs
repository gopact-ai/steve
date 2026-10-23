// Concurrency for console suites that run independent cases on a pool of
// browser contexts. Unset or empty: min(cap, cores / 2), at least 1. A
// positive integer is used as given; 1 runs the cases serially. Anything
// else (0, negative, fractional, non-numeric) is an error rather than a
// silent fallback to the parallel default.
import os from "node:os";

export function concurrency(name, cap) {
    const raw = process.env[name];
    if (raw === undefined || raw.trim() === "") return Math.min(cap, Math.max(1, Math.floor(os.availableParallelism() / 2)));
    if (!/^[1-9][0-9]*$/.test(raw.trim())) throw new Error(`${name}=${JSON.stringify(raw)}: expected a positive integer (1 runs serially), or unset for the default`);
    return Number(raw.trim());
}

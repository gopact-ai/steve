import { useCallback, useLayoutEffect, useRef } from "react";

// useEventCallback is a handler whose identity never changes but which
// always runs the latest closure. A memoised child can then keep its
// rendering while the parent re-renders for reasons the child does not
// care about — a streaming turn, mostly.
export function useEventCallback<A extends unknown[], R>(fn: (...args: A) => R): (...args: A) => R {
    const latest = useRef(fn);
    useLayoutEffect(() => { latest.current = fn; });
    return useCallback((...args: A) => latest.current(...args), []);
}

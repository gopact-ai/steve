import type { MaterialRef } from "./types";
export const refKey = (ref: MaterialRef) => JSON.stringify([ref.id, ref.selector?.kind, ref.selector?.start, ref.selector?.end, ref.selector?.quote, ref.selector?.rect?.x, ref.selector?.rect?.y, ref.selector?.rect?.width, ref.selector?.rect?.height]);
export const wireRef = ({ id, selector }: MaterialRef): MaterialRef => ({ id, ...(selector ? { selector } : {}) });

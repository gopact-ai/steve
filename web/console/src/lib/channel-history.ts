import type { Reply } from "./types";

export const channelReplyKey = (reply: Reply) => reply.id || JSON.stringify([reply.exchange_id, reply.kind, reply.at, reply.input]);
export const channelTurnKey = (reply: Reply) => reply.exchange_id || channelReplyKey(reply);
const unique = (replies: Reply[]) => [...new Map(replies.map((reply) => [channelReplyKey(reply), reply])).values()];

// Each page is authoritative for its complete turns, in server order. A late
// answer belongs beside its input, not after the next input already held in a
// Map. Never sort by time: receipt and input timestamps can interleave.
export function mergeChannelHistory(held: Reply[], page: Reply[], earlier = false): { replies: Reply[]; reset: boolean } {
    if (earlier) {
        const pageTurns = new Set(page.map(channelTurnKey));
        return { replies: unique([...page, ...held.filter((reply) => !pageTurns.has(channelTurnKey(reply)))]), reset: false };
    }
    const pageTurns = new Set(page.map(channelTurnKey));
    const overlap = held.findIndex((reply) => pageTurns.has(channelTurnKey(reply)));
    if (held.length > 0 && overlap < 0) return { replies: unique(page), reset: true };
    return { replies: unique([...held.slice(0, Math.max(0, overlap)), ...page]), reset: false };
}

// Refresh only turns already loaded. A cursor page can cross the oldest loaded
// boundary; those unrequested older turns must not expand the polling range.
// Replace whole turns in place so late replies follow their own input, and a
// newer delivery state replaces (rather than duplicates) its prior receipt.
export function revalidateChannelHistory(held: Reply[], page: Reply[]): Reply[] {
    const turns = new Map<string, Reply[]>();
    for (const reply of page) {
        const key = channelTurnKey(reply);
        const turn = turns.get(key);
        if (turn) turn.push(reply);
        else turns.set(key, [reply]);
    }
    const seen = new Set<string>();
    return held.flatMap((reply) => {
        const key = channelTurnKey(reply);
        const turn = turns.get(key);
        if (!turn) return [reply];
        if (seen.has(key)) return [];
        seen.add(key);
        return unique(turn);
    });
}

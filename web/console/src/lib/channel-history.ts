import type { Reply } from "./types";

export const channelReplyKey = (reply: Reply) => reply.id || JSON.stringify([reply.exchange_id, reply.kind, reply.at, reply.input]);
const turnKey = (reply: Reply) => reply.exchange_id || channelReplyKey(reply);
const unique = (replies: Reply[]) => [...new Map(replies.map((reply) => [channelReplyKey(reply), reply])).values()];

// Each page is authoritative for its complete turns, in server order. A late
// answer belongs beside its input, not after the next input already held in a
// Map. Never sort by time: receipt and input timestamps can interleave.
export function mergeChannelHistory(held: Reply[], page: Reply[], earlier = false): { replies: Reply[]; reset: boolean } {
    if (earlier) {
        const heldTurns = new Set(held.map(turnKey));
        return { replies: unique([...page.filter((reply) => !heldTurns.has(turnKey(reply))), ...held]), reset: false };
    }
    const pageTurns = new Set(page.map(turnKey));
    const overlap = held.findIndex((reply) => pageTurns.has(turnKey(reply)));
    if (held.length > 0 && overlap < 0) return { replies: unique(page), reset: true };
    return { replies: unique([...held.slice(0, Math.max(0, overlap)), ...page]), reset: false };
}

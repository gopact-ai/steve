// eventCursor follows the ids of the event stream: "<epoch>.<n>", counted
// per run of the hub. A reconnect resumes after last(), and accept drops
// an event the stream replays that was already delivered. An id from
// another epoch starts the count again, as reset does when the stream
// says it cannot continue from last().
export function eventCursor() {
    let epoch = "", count = 0, lastID = "";
    return {
        last: () => lastID,
        reset() { epoch = ""; count = 0; lastID = ""; },
        // A message without an id (a stream that sends none) is delivered.
        accept(id: string | undefined): boolean {
            if (!id) return true;
            const dot = id.lastIndexOf(".");
            const n = Number(id.slice(dot + 1));
            if (dot <= 0 || !Number.isSafeInteger(n) || n <= 0) return true;
            const from = id.slice(0, dot);
            if (from === epoch && n <= count) return false;
            epoch = from;
            count = n;
            lastID = id;
            return true;
        },
    };
}

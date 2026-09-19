// Go time.Time uses RFC3339Nano on the wire. Arrival cursors are not owner
// revisions, and parsing a timestamp as milliseconds loses event ordering.
export function eventTime(at: string): bigint {
    const parts = /^(\d{4})-(\d\d)-(\d\d)T(\d\d):(\d\d):(\d\d)(?:\.(\d{1,9}))?(Z|[+-]\d\d:\d\d)$/.exec(at);
    if (!parts) return 0n;
    const [, year, month, day, hour, minute, second, fraction = "", zone] = parts;
    const date = new Date(0);
    date.setUTCFullYear(Number(year), Number(month) - 1, Number(day));
    date.setUTCHours(Number(hour), Number(minute), Number(second), 0);
    const offset = zone === "Z" ? 0 : (Number(zone.slice(1, 3)) * 60 + Number(zone.slice(4))) * (zone[0] === "+" ? 1 : -1);
    return BigInt(date.getTime() - offset * 60000) * 1000000n + BigInt(fraction.padEnd(9, "0"));
}

export function compareEventTime(a: string, b: string): number {
    const left = eventTime(a), right = eventTime(b);
    return left < right ? -1 : left > right ? 1 : 0;
}

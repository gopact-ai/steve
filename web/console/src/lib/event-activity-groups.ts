import type { EventJournalEntry } from "./assistant-types";

export function eventActivityGroups(events: EventJournalEntry[]) {
    const groups: { event: EventJournalEntry; count: number }[] = [];
    for (const event of events) {
        const previous = groups[groups.length - 1];
        if (previous && previous.event.source === event.source && previous.event.type === event.type && previous.event.subject === event.subject && previous.event.routed === event.routed) previous.count++;
        else groups.push({ event, count: 1 });
    }
    return groups;
}

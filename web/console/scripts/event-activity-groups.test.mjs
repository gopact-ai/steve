import assert from "node:assert/strict";
import test from "node:test";
import { eventActivityGroups } from "../src/lib/event-activity-groups.ts";

test("the digest merges consecutive equal events without mixing sources, contents or routing states", () => {
    const event = { id: "newest", source: "mail", type: "mail.received", subject: "Invoice", routed: true, at: "2026-10-12T10:00:00Z" };
    const records = [event, { ...event, id: "second" }, { ...event, id: "third", subject: "Meeting" }, { ...event, id: "fourth", source: "other-mailbox" }, { ...event, id: "fifth", routed: false }, { ...event, id: "sixth" }];
    const digest = eventActivityGroups(records);
    assert.deepEqual(digest.map(({ event, count }) => [event.id, count]), [["newest", 2], ["third", 1], ["fourth", 1], ["fifth", 1], ["sixth", 1]]);
    assert.equal(digest.reduce((sum, group) => sum + group.count, 0), records.length);
    assert.equal(records[0], event, "grouping mutated the latest original record");
    assert.deepEqual(eventActivityGroups([]), []);
    for (const different of [{ source: "other-mailbox" }, { type: "mail.sent" }, { subject: "Meeting" }, { routed: false }]) {
        assert.equal(eventActivityGroups([event, { ...event, ...different, id: "different" }]).length, 2, `merged a distinct event: ${JSON.stringify(different)}`);
    }
});

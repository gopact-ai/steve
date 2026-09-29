import assert from "node:assert/strict";
import test from "node:test";
import { channelPollDelay } from "../src/lib/settings-channels.ts";

const now = Date.parse("2026-09-29T10:00:00Z");
const at = (offset) => new Date(now + offset).toISOString();

test("a startup retry is read just after its next attempt, within bounds", () => {
    assert.equal(channelPollDelay(at(10_000), false, now), 11_500);
    assert.equal(channelPollDelay(at(-60_000), false, now), 2_000);
    assert.equal(channelPollDelay(at(10 * 60_000), false, now), 30_000);
    assert.equal(channelPollDelay("not a time", false, now), 30_000);
});

test("a reconnect is read with each attempt, a connected channel still slowly", () => {
    assert.equal(channelPollDelay(undefined, true, now), 5_000);
    assert.equal(channelPollDelay(undefined, false, now), 30_000, "a loss after the page loaded goes unnoticed");
});

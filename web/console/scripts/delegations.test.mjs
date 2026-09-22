import assert from "node:assert/strict";
import test from "node:test";
import { withDelegations } from "../src/lib/delegations.ts";

test("restored and unrelated child snapshots preserve historical reply identity", () => {
    const step = { id: "#child", kind: "delegate", state: "done" };
    const reply = { id: "reply", process: { steps: [step], reasoning: "retained" } };
    const children = { [step.id]: { step, n: 1 }, "#other": { step: { id: "#other" }, n: 2 } };
    assert.equal(withDelegations(reply, children), reply);
    assert.equal(withDelegations(reply, {}), reply);
    const newer = { ...step, state: "failed" };
    const projected = withDelegations(reply, { ...children, [step.id]: { step: newer, n: 3 } });
    assert.notEqual(projected, reply);
    assert.equal(projected.process.steps[0], newer);
    assert.equal(projected.process.reasoning, "retained");
    assert.equal(reply.process.steps[0], step, "projection never mutates retained history");
});

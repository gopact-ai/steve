import assert from "node:assert/strict";
import test from "node:test";
import { consoleTaskConversation } from "../src/lib/task-transport.ts";

test("task controls use explicit transport and preserve opaque conversation IDs", () => {
    assert.equal(consoleTaskConversation({ transport: "feishu", channel: "console:opaque-native-id" }), null);
    assert.equal(consoleTaskConversation({ channel: "console:unknown" }), null);
    assert.equal(consoleTaskConversation({ transport: "", channel: "console:unknown" }), null);
    assert.equal(consoleTaskConversation({ transport: "console", channel: "" }), null);
    assert.equal(consoleTaskConversation({ transport: "console" }), null);
    assert.equal(consoleTaskConversation({ transport: "console", channel: "opaque-without-prefix" }), "opaque-without-prefix");
    assert.equal(consoleTaskConversation({ transport: "console", channel: "console:main" }), "console:main");
});

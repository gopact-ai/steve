import assert from "node:assert/strict";
import { changeMapFormat, settingsDraft, parseSettings } from "../../web/console/src/lib/settings-draft.ts";

const settings = (env, headers = {}) => ({ harnesses: {}, tools: [], declares: [], capabilities: [], mcp_servers: { sample: { type: "stdio", command: "sample", env, headers } } });

for (const original of [
    settings({ CERT: "first\nEXTRA=value" }),
    settings({ CERT: "-----BEGIN CERTIFICATE-----\nabc==\n-----END CERTIFICATE-----\n", EMPTY: "", SPACED: " value ", CRLF: "first\r\nlast", ESCAPED: 'a "quote" and \\path' }),
    settings(JSON.parse('{"":"empty key","A=B":"value","__proto__":"plain data"," spaced ":"unchanged","LINE\\nBREAK":"v"}')),
    settings({}, { Authorization: "Bearer value==", Spaced: " value ", "X:Name": "colon key", "": "empty key" }),
]) {
    const draft = settingsDraft(original);
    assert.deepEqual(parseSettings(draft), original, "Opening and saving must preserve every key and value");
}

const draft = settingsDraft(settings({ CERT: "first\nlast" }));
assert.equal(draft.mcp_servers[0].value.envFormat, "json", "Multiline input must be editable in explicit JSON mode");
draft.mcp_servers[0].value.envText = JSON.stringify({ CERT: "edited\nsecond=line\n", EMPTY: "" }, null, 2);
assert.deepEqual(parseSettings(draft).mcp_servers.sample.env, { CERT: "edited\nsecond=line\n", EMPTY: "" }, "Editing multiline JSON must not create extra environment variables");

for (const invalid of ['{"A":"one","A":"two"}', '{"A":"one","\\u0041":"two"}', '{"A":{},"A":"overwritten"}', '{"A":5}', '[]', 'null', '{"A":{"nested":"value"}}', '{"A":"unfinished']) {
    draft.mcp_servers[0].value.envText = invalid;
    assert.throws(() => parseSettings(draft), /JSON|名称重复/);
    assert.equal(draft.mcp_servers[0].value.envText, invalid, "Rejected edits must retain their raw text");
}

const plain = settingsDraft(settings({ FOO: "bar", EMPTY: "", SPACED: " value ", EQUALS: "a=b=c" }, { Accept: "text/plain", Spaced: " value ", Colon: "a:b" }));
assert.equal(plain.mcp_servers[0].value.envFormat, "lines");
assert.equal(plain.mcp_servers[0].value.headersFormat, "lines");
assert.deepEqual(parseSettings(plain), settings({ FOO: "bar", EMPTY: "", SPACED: " value ", EQUALS: "a=b=c" }, { Accept: "text/plain", Spaced: " value ", Colon: "a:b" }));
for (const [text, separator] of [[plain.mcp_servers[0].value.envText, "="], [plain.mcp_servers[0].value.headersText, ":"]]) {
    const json = changeMapFormat(text, "lines", "json", separator, "配置");
    assert.equal(changeMapFormat(json, "json", "lines", separator, "配置"), text, "Changing modes preserves every simple value");
}
assert.throws(() => changeMapFormat('{"CERT":"a\\nb"}', "json", "lines", "=", "配置"), /请继续使用 JSON/);
assert.throws(() => changeMapFormat('{"":"value"}', "json", "lines", "=", "配置"), /请继续使用 JSON/);
assert.throws(() => changeMapFormat("unfinished", "lines", "json", "=", "配置"), /第 1 行/);
plain.mcp_servers[0].value.envText = "FOO=bar\nFOO=other";
assert.throws(() => parseSettings(plain), /名称重复/);

console.log("PASS settings map round-trip, multiline editing, explicit formats, empty/special keys and rejected raw drafts");

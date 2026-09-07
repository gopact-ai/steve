import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import test from "node:test";
import ts from "../../web/console/node_modules/typescript/lib/typescript.js";
import { catalogs } from "../../web/console/src/lib/i18n/catalog.ts";
import { errorText, normalizeLocale, resolveLocale, translate } from "../../web/console/src/lib/i18n.ts";
import { dateTime, number, relative, when } from "../../web/console/src/lib/format.ts";
import { fmtSeconds, fmtTokens, labelsFor, spend } from "../../web/console/src/lib/labels.ts";
import { parseSettings, settingsDraft } from "../../web/console/src/lib/settings-draft.ts";

test("every message has both translations and matching parameters", async () => {
    assert.deepEqual(Object.keys(catalogs.zh).sort(), Object.keys(catalogs.en).sort());
    const parameters = (value) => [...new Set([...value.matchAll(/\{(\w+)\}/g)].map((match) => match[1]))].sort();
    for (const [key, value] of Object.entries(catalogs.zh)) {
        assert.ok(value.trim(), `Empty zh message: ${key}`);
        assert.ok(catalogs.en[key]?.trim(), `Missing en message: ${key}`);
        assert.deepEqual(parameters(value), parameters(catalogs.en[key]), `Different parameters: ${key}`);
    }
    const used = new Set();
    const directory = new URL("../../web/console/src/lib/i18n/", import.meta.url);
    for (const file of readdirSync(directory).filter((name) => name.endsWith(".ts") && name !== "catalog.ts")) {
        const domain = await import(new URL(file, directory));
        for (const [name, messages] of Object.entries(domain).filter(([name]) => name.endsWith("Zh"))) {
            assert.deepEqual(Object.keys(messages).sort(), Object.keys(domain[name.slice(0, -2) + "En"]).sort(), `Unpaired domain: ${file}`);
            for (const key of Object.keys(messages)) {
                assert.ok(!used.has(key), `Duplicate message key: ${key}`);
                assert.equal(catalogs.zh[key], messages[key], `Domain missing from catalog: ${file}/${key}`);
                used.add(key);
            }
        }
    }
    assert.equal(used.size, Object.keys(catalogs.zh).length);
});

test("locale preference overrides browser language without affecting message data", () => {
    assert.equal(normalizeLocale("zh_CN.UTF-8"), "zh");
    assert.equal(normalizeLocale("en-GB"), "en");
    assert.equal(normalizeLocale("fr"), undefined);
    assert.equal(resolveLocale("en", ["zh-CN"]), "en");
    assert.equal(resolveLocale("system", ["fr", "zh-HK", "en-US"]), "zh");
    assert.equal(resolveLocale("system", ["en-US", "zh-CN"]), "en");
    assert.equal(resolveLocale("system", ["fr"]), "en");
    assert.equal(translate("en", "common.save"), "Save");
    assert.equal(translate("zh", "common.save"), "保存");
    assert.equal(translate("en", "connection.devices", { online: 1, total: 2 }), "Devices online: 1/2");
    assert.equal(translate("en", "format.context", { count: "<script>{unchanged}</script>" }), "Context <script>{unchanged}</script>");
    assert.throws(() => translate("en", "connection.devices", { online: 1 }), /Missing translation parameter/);
});

test("display formatting uses explicit locale and preserves protocol values", () => {
    assert.equal(number(12345, "en"), "12,345");
    assert.equal(dateTime("2026-09-07T00:00:00Z", "en", { timeZone: "UTC", year: "numeric", month: "2-digit", day: "2-digit" }), "09/07/2026");
    assert.equal(dateTime("invalid", "zh"), "—");
    assert.equal(when(undefined, "en"), "");
    const now = Date.parse("2026-09-07T00:00:00Z");
    assert.equal(relative("2026-09-07T00:00:00Z", "en", now), "Just now");
    assert.match(relative("2026-09-06T23:58:00Z", "zh", now), /2.*前/);
    assert.match(relative("2026-09-06T23:58:00Z", "en", now), /2.*ago/);
    assert.equal(relative("invalid", "en", now), "—");
    assert.equal(fmtTokens(1200, "en"), "1.2k");
    assert.equal(fmtSeconds(61, "en"), "1m 1s");
    assert.equal(fmtSeconds(61, "zh"), "1 分 1 秒");
    assert.equal(spend(undefined, "en"), "Not reported");
    assert.equal(labelsFor("en").taskState.running, "In progress");
    assert.equal(labelsFor("zh").taskState.running, "进行中");
    assert.equal(labelsFor("zh").level.sealed, "sealed");
});

test("an existing validation error changes language without reparsing or altering its draft", () => {
    const draft = settingsDraft({ harnesses: {}, tools: [], declares: [], capabilities: [], mcp_servers: { sample: { type: "stdio", env: {}, headers: {} } } });
    draft.mcp_servers[0].value.envText = "UNFINISHED";
    let failure;
    try { parseSettings(draft); } catch (error) { failure = error; }
    assert.ok(failure instanceof Error);
    assert.match(errorText(failure, "zh"), /sample 环境变量第 1 行/);
    assert.match(errorText(failure, "en"), /Line 1 of sample Environment variables/);
    assert.equal(draft.mcp_servers[0].value.envText, "UNFINISHED");
    assert.equal(errorText(new Error("External raw detail: 原文"), "en"), "External raw detail: 原文");
});

test("request locale changes headers without changing authentication or write identity", async () => {
    const previous = { window: globalThis.window, sessionStorage: globalThis.sessionStorage, fetch: globalThis.fetch };
    globalThis.window = { location: { search: "?token=i18n-test-token" } };
    globalThis.sessionStorage = { getItem: () => "i18n-test-token", setItem() {} };
    const requests = [];
    globalThis.fetch = async (path, options) => { requests.push({ path, options }); return { ok: true, json: async () => ({ ok: true }) }; };
    try {
        const source = readFileSync(new URL("../../web/console/src/lib/http.ts", import.meta.url), "utf8");
        const code = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
        const http = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);
        const body = { command_id: "same-id", input: "原始输入\n  remains unchanged" };
        http.setRequestLocale("zh"); await http.request("/console/queue", { method: "POST", body });
        http.setRequestLocale("en"); await http.request("/console/queue", { method: "POST", body });
        assert.equal(requests[0].options.headers.get("Accept-Language"), "zh-CN");
        assert.equal(requests[1].options.headers.get("Accept-Language"), "en");
        assert.ok(requests.every(({ options, path }) => options.headers.get("Authorization") === "Bearer i18n-test-token" && path === "./console/queue"));
        assert.equal(requests[0].options.body, requests[1].options.body);
    } finally {
        for (const [key, value] of Object.entries(previous)) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
    }
});

import assert from "node:assert/strict";
import test from "node:test";
import { loadLocale, translate } from "../../web/console/src/lib/i18n.ts";
import { describeHistory, familyOf } from "../../web/console/src/lib/history-lines.ts";
import { stateWordIn } from "../../web/console/src/lib/states.ts";

await Promise.all([loadLocale("zh"), loadLocale("en")]);

const zh = (key, params) => translate("zh", key, params);
const en = (key, params) => translate("en", key, params);
const named = { "node-9e10": "dev-sg", "node-4bbf": "Steve's MacBook" };
const name = (id) => named[id] || id;
const state = (locale) => (value) => stateWordIn(value, locale === "zh" ? zh : en);

const read = (entry, locale = "zh") => describeHistory(entry, locale === "zh" ? zh : en, name, state(locale));

test("a machine is named, and its build facts sit beside the sentence", () => {
    const line = read({
        at: "2026-09-17T02:42:27Z", kind: "observe.node.up", subject: "node-9e10",
        text: "node-9e10 connected: n251-239-109 linux/amd64, build b1bfb0e",
        data: { host: "n251-239-109", os: "linux", arch: "amd64", build: "b1bfb0e" },
    });
    assert.equal(line.family, "machine");
    assert.equal(line.tone, "good");
    assert.equal(line.title, "dev-sg 接入集群");
    assert.deepEqual(line.facts, ["n251-239-109", "linux/amd64", "版本 b1bfb0e"]);
    // The node ID belongs to the raw record, not to a column of its own.
    assert.equal(line.mono, undefined);
    assert.equal(read({ at: "", kind: "observe.node.up", subject: "node-9e10", text: "", data: { os: "linux", arch: "amd64" } }, "en").title, "dev-sg joined the cluster");
});

test("a machine that simply went away does not read as an error", () => {
    const closed = read({ at: "", kind: "observe.node.down", subject: "node-9e10", text: "…", data: { reason: "disconnected" } });
    assert.equal(closed.title, "dev-sg 断开连接");
    assert.equal(closed.note, "连接已关闭");
    const broke = read({ at: "", kind: "observe.node.down", subject: "node-9e10", text: "…", data: { reason: "dial tcp: i/o timeout" } });
    assert.equal(broke.note, "与 dev-sg 的连接中途断开");
});

test("a failed skill bundle says so, and keeps the reason", () => {
    const ok = read({ at: "", kind: "observe.node.skills", subject: "node-9e10", text: "…", data: { hash: "3a654c44c9c9", count: "1" } });
    assert.equal(ok.family, "skills");
    assert.equal(ok.title, "dev-sg 收到技能包");
    assert.deepEqual(ok.facts, ["1 个技能", "技能包 3a654c44c9c9"]);
    const bad = read({ at: "", kind: "observe.node.skills", subject: "node-9e10", text: "…", data: { hash: "3a654c44c9c9", error: "disk full" } });
    assert.equal(bad.tone, "bad");
    assert.equal(bad.note, "disk full");
});

test("ability changes are phrased one per fact", () => {
    const line = read({
        at: "", kind: "observe.node.manifest", subject: "node-9e10", text: "…",
        data: { changes: "hardware:amd64\t\tavailable\nharness:codex\tavailable\tunavailable\nharness:grok\tavailable\t" },
    });
    assert.equal(line.title, "dev-sg 的可用能力有 3 项变化");
    assert.deepEqual(line.facts, ["hardware:amd64 新增（available）", "harness:codex：available → unavailable", "harness:grok 不再可用"]);
});

test("a record written before the facts were kept apart is still readable", () => {
    // Everything recorded before this page learned to read structure has
    // only the server's sentence; the machine is still named from it.
    const line = read({
        at: "", kind: "observe.node.manifest", subject: "node-9e10",
        text: "node-9e10: hardware:amd64 appeared (available); hardware:cpu appeared (available)",
    });
    assert.equal(line.title, "dev-sg 的可用能力有 2 项变化");
    assert.deepEqual(line.facts, ["hardware:amd64 appeared (available)", "hardware:cpu appeared (available)"]);
    const up = read({ at: "", kind: "observe.node.up", subject: "node-9e10", text: "node-9e10 connected: …" });
    assert.equal(up.title, "dev-sg 接入集群");
    assert.deepEqual(up.facts, []);
});

test("a ledger transition is named by its operation and speaks the page's state words", () => {
    const line = read({ at: "", kind: "ledger", operation: "att-1234", text: "…", from: "running", to: "failed", actor: "codex" });
    assert.equal(line.family, "ledger");
    assert.equal(line.tone, "bad");
    assert.equal(line.title, "执行尝试 进行中 → 失败");
    assert.deepEqual(line.facts, ["由 codex 触发"]);
    assert.equal(line.mono, "att-1234");
    const opened = read({ at: "", kind: "ledger", operation: "land-9", text: "…", to: "proposed", actor: "" });
    assert.equal(opened.title, "产物落地开始，状态 待定");
    assert.deepEqual(opened.facts, ["由 steve 触发"]);
});

test("an unknown kind is shown as the server wrote it rather than guessed at", () => {
    const line = read({ at: "", kind: "observe.something.new", subject: "x", text: "the server said this" });
    assert.equal(line.title, "the server said this");
    assert.equal(line.family, "other");
    assert.equal(familyOf({ at: "", kind: "observe.content.degraded", subject: "x", text: "" }), "content");
});

test("a transport failure names the machine instead of quoting the socket", () => {
    // The record a real upgrade produced: the node restarted while the
    // bundle was in flight, and Go wrote the node ID and both addresses.
    const raw = 'node "node-9e10" at 127.0.0.1:37633: read tcp 127.0.0.1:64624->127.0.0.1:64612: read: connection reset by peer';
    const line = read({ at: "", kind: "observe.node.skills", subject: "node-9e10", text: raw, data: { hash: "3a654c44c9c9", error: raw } });
    assert.equal(line.title, "dev-sg 安装技能包失败");
    assert.equal(line.note, "与 dev-sg 的连接中途断开");
    assert.equal(read({ at: "", kind: "observe.node.skills", subject: "node-9e10", text: raw, data: { error: raw } }, "en").note, "the link to dev-sg dropped part-way through");
});

test("an unrecognised failure keeps its words but loses the plumbing", () => {
    const line = read({
        at: "", kind: "observe.node.skills", subject: "node-9e10", text: "…",
        data: { error: 'bundle rejected by node "node-9e10" at 10.251.239.109:7701: checksum mismatch' },
    });
    assert.equal(line.note, 'bundle rejected by node "dev-sg": checksum mismatch');
    // A node nobody can name keeps its ID: it is still searchable.
    const unknown = read({ at: "", kind: "observe.node.skills", subject: "node-abcd", text: "…", data: { error: "node-0123456789abcdef0123456789abcdef refused the bundle" } });
    assert.equal(unknown.note, "node-0123456789abcdef0123456789abcdef refused the bundle");
});

test("a restart names the machine, whether a person or automatic start ran it and from where", () => {
    const manual = read({
        at: "", kind: "observe.node.restart", subject: "node-9e10", text: "node-9e10: restart by node-4bbf: restarted",
        data: { by: "node-4bbf", trigger: "manual", outcome: "restarted" },
    });
    assert.equal(manual.family, "machine");
    assert.equal(familyOf({ at: "", kind: "observe.node.restart", subject: "node-9e10", text: "" }), "machine");
    assert.equal(manual.tone, "good");
    assert.equal(manual.label, "节点重启");
    assert.equal(manual.title, "dev-sg 的节点进程已重启");
    assert.deepEqual(manual.facts, ["手动", "由 Steve's MacBook 执行"]);
    assert.equal(manual.note, undefined);
    const started = read({ at: "", kind: "observe.node.restart", subject: "node-9e10", text: "…", data: { by: "node-4bbf", trigger: "automatic", outcome: "started" } });
    assert.equal(started.tone, "good");
    assert.equal(started.title, "dev-sg 的节点进程已启动");
    assert.deepEqual(started.facts, ["自动拉起", "由 Steve's MacBook 执行"]);
    const english = read({ at: "", kind: "observe.node.restart", subject: "node-9e10", text: "…", data: { by: "node-4bbf", trigger: "automatic", outcome: "started" } }, "en");
    assert.equal(english.title, "The node process on dev-sg was started");
    assert.deepEqual(english.facts, ["Automatic start", "Run by Steve's MacBook"]);
});

test("a restart that found the process running, failed, or ended automatic start says so and keeps why", () => {
    const running = read({ at: "", kind: "observe.node.restart", subject: "node-9e10", text: "…", data: { by: "node-4bbf", trigger: "automatic", outcome: "running", reason: "The peer process 4242 is still running" } });
    assert.equal(running.tone, "warn");
    assert.equal(running.title, "dev-sg 的节点进程仍在运行，没有自动重启");
    assert.equal(running.note, "The peer process 4242 is still running");
    const failed = read({ at: "", kind: "observe.node.restart", subject: "node-9e10", text: "…", data: { by: "node-4bbf", trigger: "manual", outcome: "failed", reason: "The peer did not stay running" } });
    assert.equal(failed.tone, "bad");
    assert.equal(failed.title, "dev-sg 的节点重启没有成功");
    assert.equal(failed.note, "The peer did not stay running");
    const stopped = read({ at: "", kind: "observe.node.restart", subject: "node-9e10", text: "…", data: { by: "node-4bbf", trigger: "automatic", outcome: "stopped", reason: "The peer did not stay running" } });
    assert.equal(stopped.tone, "bad");
    assert.equal(stopped.title, "dev-sg 的自动拉起已停止");
    assert.equal(read({ at: "", kind: "observe.node.restart", subject: "node-9e10", text: "…", data: { outcome: "stopped" } }, "en").title, "Automatic start stopped for dev-sg");
    // An outcome this page does not know is shown as the server wrote it.
    const unknown = read({ at: "", kind: "observe.node.restart", subject: "node-9e10", text: "node-9e10: restart by node-4bbf: paused", data: { by: "node-4bbf", trigger: "manual", outcome: "paused" } });
    assert.equal(unknown.title, "node-9e10: restart by node-4bbf: paused");
    assert.equal(unknown.family, "machine");
});

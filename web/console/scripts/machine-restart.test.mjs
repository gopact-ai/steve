import assert from "node:assert/strict";
import test from "node:test";
import { loadLocale, translate } from "../src/lib/i18n.ts";
import { autoStartLine, restartConfirms, restartHiddenBy, restartOffered, restartPollDelay, restartRunning, restartShown } from "../src/lib/machine-restart.ts";

await Promise.all([loadLocale("zh"), loadLocale("en")]);
const zh = (key, params) => translate("zh", key, params);
const en = (key, params) => translate("en", key, params);
const time = (at) => `<${at}>`;
const auto = (fields) => ({ attempts: 0, limit: 5, ...fields });

test("a machine is offered a restart once this console's node answered for it, never the coordinator", () => {
    assert.equal(restartOffered("worker", {}), true);
    assert.equal(restartOffered(undefined, { auto_start: auto({ state: "watching" }) }), true);
    assert.equal(restartOffered("hub", {}), false, "the coordinator restarts with its application, not over SSH");
    assert.equal(restartOffered("worker", null), false, "nothing is offered before the node answered, or after it refused");
});

test("a restart is confirmed first only while the machine is online", () => {
    assert.equal(restartConfirms(true), true);
    assert.equal(restartConfirms(false), false);
});

test("a refusal hides the restart; a passing failure keeps what was shown", () => {
    for (const status of [400, 403, 404, 501]) assert.equal(restartHiddenBy(status), true, `${status}`);
    for (const status of [500, 502, 503, 504]) assert.equal(restartHiddenBy(status), false, `${status}`);
});

test("a running restart is followed every second, a quiet machine every few", () => {
    assert.equal(restartRunning({ restart: { status: "installing" } }), true);
    assert.equal(restartRunning({ restart: { status: "connected" } }), false);
    assert.equal(restartRunning({}), false);
    assert.equal(restartRunning(null), false);
    assert.equal(restartPollDelay({ restart: { status: "installing" } }, false), 1000);
    assert.equal(restartPollDelay({ auto_start: auto({ state: "attempting" }) }, false), 1000);
    assert.equal(restartPollDelay({}, true), 1000, "a restart asked here is followed before its status says so");
    assert.equal(restartPollDelay({ restart: { status: "needs_attention" }, auto_start: auto({ state: "stopped" }) }, false), 5000);
    assert.equal(restartPollDelay(null, false), 5000);
});

test("a running restart shows, then the one asked for here until a later one ran, then one that needs attention", () => {
    const restart = (plan_id, status) => ({ plan_id, status, steps: [] });
    const asked = restart("restart-1", "connected");
    assert.deepEqual(restartShown({ restart: restart("restart-2", "installing"), automatic: true }, asked, false), { restart: restart("restart-2", "installing"), automatic: true });
    assert.deepEqual(restartShown({ restart: restart("restart-1", "installing") }, null, false), { restart: restart("restart-1", "installing"), automatic: false });
    assert.deepEqual(restartShown({ restart: restart("restart-0", "needs_attention") }, null, true), { automatic: false }, "a restart asked here that has not answered shows nothing yet");
    assert.deepEqual(restartShown({ restart: asked }, asked, false), { restart: asked, automatic: false });
    assert.deepEqual(restartShown({}, asked, false), { restart: asked, automatic: false });
    assert.deepEqual(restartShown({ restart: restart("restart-2", "connected"), automatic: true }, asked, false), { automatic: false }, "a later restart that brought the machine back replaces it");
    assert.deepEqual(restartShown({ restart: restart("restart-2", "needs_attention"), automatic: true }, asked, false), { restart: restart("restart-2", "needs_attention"), automatic: true });
    assert.deepEqual(restartShown({ restart: restart("restart-2", "connected") }, null, false), { automatic: false }, "a settled restart that brought the machine back is its last restart");
});

test("automatic start says what it watches for, what it is doing, and how many starts it made", () => {
    assert.equal(autoStartLine(undefined, zh, time), null, "a node that does not watch the machine says nothing");
    assert.deepEqual(autoStartLine(auto({ state: "watching" }), zh, time), { tone: "quiet", title: "机器离线且节点进程不在时，会经 SSH 自动启动" });
    assert.deepEqual(autoStartLine(auto({ state: "watching", attempts: 2 }), zh, time), { tone: "info", title: "已自动启动，等待机器稳定在线（第 2/5 次）" });
    assert.deepEqual(autoStartLine(auto({ state: "waiting", offline_since: "2026-09-29T08:00:00Z" }), zh, time), { tone: "warn", title: "<2026-09-29T08:00:00Z> 起离线，稍后检查节点进程" });
    assert.deepEqual(autoStartLine(auto({ state: "unreachable", last_error: "ssh: connect to host dev port 22: Connection refused" }), zh, time), { tone: "warn", title: "SSH 连不上这台机器，暂不自动启动", detail: "ssh: connect to host dev port 22: Connection refused" });
    assert.deepEqual(autoStartLine(auto({ state: "attempting", attempts: 1 }), zh, time), { tone: "info", title: "正在自动启动（第 2/5 次）" });
    assert.deepEqual(autoStartLine(auto({ state: "retrying", attempts: 2, next_at: "2026-09-29T08:05:00Z", last_error: "The peer did not stay running" }), zh, time),
        { tone: "warn", title: "自动启动没有成功，将在 <2026-09-29T08:05:00Z> 再试", detail: "The peer did not stay running", hint: "已尝试 2/5 次" });
    assert.deepEqual(autoStartLine(auto({ state: "retrying", next_at: "2026-09-29T08:05:00Z", last_error: "An upgrade is running" }), zh, time),
        { tone: "warn", title: "自动启动没有成功，将在 <2026-09-29T08:05:00Z> 再试", detail: "An upgrade is running" }, "a start another operation held off counts nothing");
    assert.equal(autoStartLine(auto({ state: "attempting", attempts: 0 }), en, time).title, "Starting automatically (attempt 1 of 5)");
});

test("a machine that blocks automatic start says why, what clears it, and when it is tried again", () => {
    const blocked = auto({ state: "blocked", next_at: "2026-09-29T08:10:00Z", last_error: "Another installation holds the lock", suggestion: "Remove ~/steve-bin/.install-lock if nothing is installing" });
    assert.deepEqual(autoStartLine(blocked, zh, time),
        { tone: "warn", title: "自动启动被挡住，将在 <2026-09-29T08:10:00Z> 再试", detail: "Another installation holds the lock", hint: "Remove ~/steve-bin/.install-lock if nothing is installing" });
    assert.equal(autoStartLine(blocked, en, time).title, "Automatic start is blocked; trying again at <2026-09-29T08:10:00Z>");
});

test("a running peer is left alone and pointed at the manual restart; a stopped one says why and how it resumes", () => {
    assert.deepEqual(autoStartLine(auto({ state: "peer_running", reason: "The peer process is still running" }), zh, time),
        { tone: "warn", title: "节点进程还在运行，但机器没有回到集群", hint: "自动拉起不会结束它；如果它卡住了，请点「重启节点」" });
    assert.deepEqual(autoStartLine(auto({ state: "stopped", attempts: 5, last_error: "The peer did not stay running" }), zh, time),
        { tone: "bad", title: "已停止自动拉起", detail: "The peer did not stay running", hint: "机器稳定在线一段时间或手动重启成功后会恢复" });
    const stopped = autoStartLine(auto({ state: "stopped", attempts: 5, last_error: "x" }), en, time);
    assert.equal(stopped.title, "Automatic start has stopped");
    assert.equal(stopped.hint, "It resumes once the machine stays online for a while, or after a manual restart succeeds");
    assert.equal(autoStartLine(auto({ state: "peer_running" }), en, time).hint, "Automatic start never ends it; if it is stuck, use Restart node");
    assert.deepEqual(autoStartLine(auto({ state: "resting" }), en, time), { tone: "quiet", title: "resting" }, "a state this page does not know is shown as it came");
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js';
import { OnePassword } from '../src/onepassword.mjs';
import { Store } from '../src/store.mjs';
import { MailManager } from '../src/manager.mjs';
import { MailClientWorker } from '../src/client-worker.mjs';
import { adminServer } from '../src/admin-server.mjs';

const itemID = 'a'.repeat(26), vaultID = 'v'.repeat(26), secret = 'private-fixture-password';
const google = { id: itemID, category: 'LOGIN', title: 'Google: owner', vault: { id: vaultID }, urls: [{ href: 'https://accounts.google.com' }], tags: ['Work'] };
function cliFixture() {
  const calls = []; let address = 'owner@gmail.com'; let items = [google,
    { ...google, id: 'b'.repeat(26), title: 'Apple', urls: [{ href: 'https://appleid.apple.com' }] },
    { ...google, id: 'c'.repeat(26), archived: true },
    { ...google, id: 'd'.repeat(26), urls: [{ href: 'https://accounts.google.com.evil.example' }] }];
  const executor = async (command, args, options) => {
    calls.push({ command, args, options });
    assert.equal(options.shell, false); assert.equal(options.env.OP_INCLUDE_ARCHIVE, 'false');
    if (args[1] === 'list') return { stdout: JSON.stringify(items) };
    if (args.includes('--otp')) return { stdout: '123456\n' };
    return { stdout: JSON.stringify([{ id: 'username', value: address }, ...(args.includes('--reveal') ? [{ id: 'password', value: secret }] : [])]) };
  };
  return { calls, executor, setAddress(value) { address = value; }, setItems(value) { items = value; } };
}
function setup(t, factory) {
  const dir = mkdtempSync(join(tmpdir(), 'steve-1password-')); const store = new Store(dir, Buffer.alloc(32, 7));
  const config = { accounts: [], steve_url: 'http://127.0.0.1:1', onepassword: { vaults: ['vibe-coding'], auto_login: true } };
  const manager = new MailManager(config, store, { adapters: new Map(), inflight: new Set() }, factory);
  const fixture = cliFixture(); manager.onepassword = new OnePassword(config.onepassword, fixture.executor);
  t.after(async () => { await manager.jobs.close(); store.close(); rmSync(dir, { recursive: true, force: true }); });
  return { manager, store, dir, fixture };
}
test('official CLI discovery is vault-scoped, excludes Apple/archived/lookalike items and reads only usernames', async t => {
  const { manager, fixture } = setup(t);
  await assert.rejects(manager.previewOnePassword('Personal'), /owner-configured/); assert.equal(fixture.calls.length, 0);
  const draft = await manager.previewOnePassword('vibe-coding');
  assert.equal(draft.items.length, 1); assert.equal(draft.skipped, 3); assert.equal(draft.items[0].group, 'vibe-coding');
  assert.equal(draft.items[0].login_method, 'onepassword'); assert.equal(draft.items[0].has_password, false);
  assert.equal(fixture.calls.length, 2); assert.ok(fixture.calls.every(call => !call.args.includes('--reveal')));
  assert.ok(!JSON.stringify(draft).includes(itemID)); assert.ok(!JSON.stringify(draft).includes(vaultID));
});
test('credential references survive restart and selected commits without persisting passwords or exporting references', async t => {
  const { manager, store, dir } = setup(t);
  const draft = await manager.previewOnePassword('vibe-coding'); const input = { draft_id: draft.id, rows: ['0'] };
  const saved = manager.commit(input); assert.deepEqual(manager.commit(input).imported, saved.imported);
  const restored = new MailManager(manager.config, store, manager.runner);
  const account = restored.get(saved.imported[0]); assert.equal(account.credential_ref.item_id, itemID);
  assert.equal(account.password, ''); assert.equal(restored.publicAccount(account).login_method, 'onepassword');
  assert.ok(!readFileSync(join(dir, 'mail.sqlite')).includes(Buffer.from(itemID)));
  assert.ok(!JSON.stringify(store.secret(`draft:${draft.id}`)).includes(itemID));
  const exportText = Buffer.from(manager.export({ format: 'json', include_secrets: true }).data, 'base64').toString();
  assert.ok(!exportText.includes(itemID)); assert.ok(!exportText.includes(secret));
  // Linking an already managed mailbox is useful and preserves its account ID.
  const again = await manager.previewOnePassword('vibe-coding'); assert.equal(again.items[0].duplicate, true);
  assert.deepEqual(manager.commit({ draft_id: again.id, rows: ['0'] }).imported, saved.imported);
  manager.archive(account.id); assert.equal(manager.accounts[0].credential_ref, undefined);
});
test('current password and OTP go only to the private browser; changed identities and CLI errors are sanitized', async t => {
  const { manager, store, fixture } = setup(t);
  const draft = await manager.previewOnePassword('vibe-coding'); const [accountID] = manager.commit({ draft_id: draft.id, rows: ['0'] }).imported;
  const account = manager.get(accountID);
  assert.deepEqual(await manager.onepassword.credentials(account), { username: account.address, password: secret });
  assert.equal(await manager.onepassword.otp(account), '123456');
  assert.ok(fixture.calls.find(call => call.args.includes('--reveal')).args.includes(`--vault=${vaultID}`));
  fixture.setAddress('other@gmail.com'); await assert.rejects(manager.onepassword.credentials(account), /address changed/);
  fixture.setItems([]); await assert.rejects(manager.onepassword.credentials(account), /no longer/);
  const bad = new OnePassword(manager.config.onepassword, async () => { throw new Error(secret); });
  await assert.rejects(bad.preview('vibe-coding'), error => !error.message.includes(secret) && /denied/.test(error.message));
  assert.ok(!JSON.stringify(store.secret('managed:accounts')).includes(secret));
});
test('automatic login waits for the SDK receipt and reports owner challenges without returning private URLs', async t => {
  let finish; const done = new Promise(resolve => { finish = resolve; });
  const { manager } = setup(t, async (account, store, hooks) => { await hooks.google('https://accounts.google.com/private-oauth-url'); await done; return { poll() {} }; });
  manager.configureApps({ google_credentials: JSON.stringify({ installed: { client_id: 'fixture', client_secret: 'private-app-secret' } }) });
  const draft = await manager.previewOnePassword('vibe-coding'); const [id] = manager.commit({ draft_id: draft.id, rows: ['0'] }).imported;
  let called = 0;
  manager.loginBrowser = { enqueue(operation) { return Promise.resolve().then(operation); }, async login(account, url, hooks) { called++; assert.equal(account.id, id); hooks.progress('owner_action_required', 'Confirm the passkey in the Steve login browser'); }, async close() { finish(); } };
  const first = manager.jobs.start(id); const second = manager.jobs.start(id); assert.equal(first.id, second.id); assert.equal(first.stage, 'queued');
  await new Promise(resolve => setImmediate(resolve)); assert.equal(called, 1);
  const waiting = manager.jobs.status(first.id, true);
  assert.equal(waiting.state, 'waiting'); assert.equal(waiting.method, 'onepassword'); assert.equal(waiting.needs_owner, true);
  assert.ok(!('url' in waiting)); assert.equal(manager.authorized(manager.get(id)), false);
  finish(); await manager.jobs.jobs.get(first.id).promise;
  assert.equal(manager.jobs.status(first.id, true).state, 'authorized'); assert.equal(manager.jobs.status(first.id, true).needs_owner, false);
});
test('real stdio MCP connects to the authenticated local broker, replays imports and hides CLI credentials', async t => {
  const { manager, fixture } = setup(t);
  const worker = new MailClientWorker(manager); const token = 'fixture-private-administration-token-32-chars';
  const server = await adminServer(manager, token, 0, worker); t.after(() => server.close());
  const transport = new StdioClientTransport({ command: process.execPath, args: [new URL('../src/cli.mjs', import.meta.url).pathname, 'mcp'],
    env: { STEVE_MAIL_ADMIN_URL: server.url, STEVE_MAIL_ADMIN_TOKEN: token }, stderr: 'pipe' });
  let stderr = ''; transport.stderr.on('data', data => { stderr += data.toString(); });
  const client = new Client({ name: 'steve-fixture-agent', version: '1.0' }); await client.connect(transport);
  t.after(() => client.close());
  const tools = await client.listTools(); assert.equal(tools.tools.length, 7); assert.ok(!tools.tools.some(tool => /password_get|item_get|shell/.test(tool.name)));
  const call = async (name, args) => { const value = await client.callTool({ name, arguments: { request_id: randomUUID(), ...args } }); assert.ok(!JSON.stringify(value).includes(secret)); return value; };
  const status = JSON.parse((await call('mail_1password_status', {})).content[0].text); assert.deepEqual(status.vaults, ['vibe-coding']);
  const request_id = randomUUID();
  const preview = await call('mail_1password_preview', { request_id, vault: 'vibe-coding' });
  const draft = JSON.parse(preview.content[0].text); assert.equal(draft.items[0].address, 'owner@gmail.com');
  const again = await call('mail_1password_preview', { request_id, vault: 'vibe-coding' }); assert.deepEqual(again, preview); assert.equal(fixture.calls.length, 2);
  const denied = await call('mail_1password_preview', { request_id, vault: 'Personal' }); assert.equal(denied.isError, true);
  const saved = JSON.parse((await call('mail_import_commit', { draft_id: draft.id, rows: ['0'] })).content[0].text); assert.equal(saved.imported.length, 1);
  const blocked = await call('mail_login_start', { account_id: saved.imported[0] }); assert.equal(blocked.isError, true); // App setup is required, never false success.
  assert.ok(!stderr.includes(secret)); assert.ok(!stderr.includes(token));
});
test('async client invocations await the preview and concurrent MCP retries produce one receipt', async t => {
  const { manager, store } = setup(t); let scans = 0; const original = manager.previewOnePassword.bind(manager);
  manager.previewOnePassword = async vault => { scans++; await new Promise(resolve => setTimeout(resolve, 10)); return original(vault); };
  const worker = new MailClientWorker(manager); const input = { request_id: randomUUID(), capability: 'mail.1password.preview', arguments: { vault: 'vibe-coding' } };
  const receipts = await Promise.all([worker.invoke(input), worker.invoke(input)]); assert.deepEqual(receipts[0], receipts[1]); assert.equal(scans, 1);
  store.secret('managed:client', { id: 'fixture', token: 'fixture-token', conversation: 'fixture' });
  worker.registration = { generation: 1 }; worker.registeredID = 'fixture'; worker.heartbeatAt = Date.now();
  let completion; worker.request = async (client, path, body) => path.endsWith('/claim') ? { found: true, invocation: { id: 'fixture-invocation', claim_token: 'fixture-claim', request: { capability: input.capability, arguments: input.arguments } } } : (completion = body);
  await worker.tick(); assert.equal(completion.state, 'done'); assert.equal(completion.result.items[0].address, 'owner@gmail.com');
  await worker.tick(); assert.equal(scans, 2); // Durable completion bypasses a third scan.
});

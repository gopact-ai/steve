import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { zipSync, strToU8 } from 'fflate';
import { Store } from '../src/store.mjs';
import { MailManager } from '../src/manager.mjs';
import { MailClientWorker } from '../src/client-worker.mjs';
import { adminServer } from '../src/admin-server.mjs';
import { readImport, writeExport } from '../src/formats.mjs';

const secret = 'fixture-password,with\nquotes"';
const csv = 'Title,Website,Username,Password,Archived,Notes\r\n"Personal, Gmail",https://mail.google.com,owner@gmail.com,"fixture-password,with\nquotes""",false,\r\nCloud,https://icloud.com,owner@icloud.com,apple-app-password,false,App-specific password\r\nOther,https://example.org,other@example.org,irrelevant,false,\r\nOld,https://outlook.live.com,old@outlook.com,old,true,\r\nDuplicate,,owner@gmail.com,something,false,\r\n';
function setup(t, factory) {
  const dir = mkdtempSync(join(tmpdir(), 'steve-mail-management-')); const key = Buffer.alloc(32, 6);
  const store = new Store(dir, key); const config = { accounts: [], steve_url: 'http://127.0.0.1:1' };
  const manager = new MailManager(config, store, { adapters: new Map(), inflight: new Set() }, factory);
  t.after(async () => { await manager.jobs.close(); store.close(); rmSync(dir, { recursive: true, force: true }); });
  return { manager, store, dir, key, config };
}
test('real 1Password CSV headers, escaped fields, service filtering and duplicate preview preserve credentials privately', t => {
  const { manager, store, dir } = setup(t);
  const draft = manager.preview({ name: 'passwords.csv', data: Buffer.from(csv).toString('base64') });
  assert.equal(draft.items.length, 2); assert.equal(draft.skipped, 2);
  assert.equal(draft.items[0].title, 'Personal, Gmail'); assert.equal(draft.items[1].credential_kind, 'app_password');
  assert.ok(!JSON.stringify(draft).includes('fixture-password'));
  assert.ok(!readFileSync(join(dir, 'mail.sqlite')).includes(Buffer.from('apple-app-password')));
  const first = manager.commit({ draft_id: draft.id, rows: draft.items.map(row => row.id) });
  const retry = manager.commit({ draft_id: draft.id, rows: draft.items.map(row => row.id) });
  assert.deepEqual(retry.imported, first.imported); assert.equal(manager.snapshot().items.length, 2);
  assert.equal(store.secret('managed:accounts')[1].app_password, 'apple-app-password');
  const publicData = JSON.stringify(manager.snapshot()); assert.ok(!publicData.includes('apple-app-password')); assert.ok(!publicData.includes('fixture-password'));
  const restored = new MailManager(manager.config, store, manager.runner);
  assert.equal(restored.snapshot().items.length, 2);
});
test('CSV, 1PUX and JSON exports round-trip mailbox logins, exclude OAuth tokens and omit secrets by default', t => {
  const { manager, store } = setup(t);
  manager.add({ provider: 'icloud', address: 'owner@icloud.com', group: 'Personal', tags: ['family', 'bills'], password: secret, credential_kind: 'app_password' });
  manager.add({ provider: 'hotmail', address: 'owner@hotmail.com', password: 'login-password' });
  store.secret('google:unused', { refresh_token: 'never-export-oauth' });
  for (const format of ['csv', '1pux', 'json']) {
    const publicFile = manager.export({ format }); const empty = readImport(publicFile.name, Buffer.from(publicFile.data, 'base64'));
    assert.equal(empty.accounts.length, 2); assert.ok(empty.accounts.every(a => a.password === ''));
    assert.equal(empty.accounts[0].group, 'Personal'); assert.deepEqual(empty.accounts[0].tags, ['family', 'bills']);
    const fullFile = manager.export({ format, include_secrets: true }); const full = readImport(fullFile.name, Buffer.from(fullFile.data, 'base64'));
    assert.equal(full.accounts[0].password, secret); assert.equal(full.accounts[0].credential_kind, 'app_password');
    assert.ok(!Buffer.from(fullFile.data, 'base64').includes(Buffer.from('never-export-oauth')));
  }
});
test('1PUX custom app password fields take precedence, archived and non-mail items are ignored', () => {
  const data = { accounts: [{ vaults: [{ attrs: { name: 'Family' }, items: [{ state: 'active', categoryUuid: '001', overview: { title: 'Cloud', tags: ['shared'], url: 'https://icloud.com' }, details: {
    loginFields: [{ designation: 'username', value: 'owner@icloud.com' }, { designation: 'password', value: 'main-apple-password' }],
    sections: [{ fields: [{ title: 'App-specific password', value: { concealed: 'app-specific-value' } }] }],
  } }, { state: 'archived', categoryUuid: '001' }] }] }] };
  const zip = zipSync({ 'export.attributes': strToU8('{"version":3}'), 'export.data': strToU8(JSON.stringify(data)), 'files/ignored': new Uint8Array(4000) });
  const imported = readImport('export.1pux', Buffer.from(zip));
  assert.equal(imported.accounts[0].password, 'app-specific-value'); assert.equal(imported.accounts[0].credential_kind, 'app_password');
  assert.equal(imported.accounts[0].group, 'Family'); assert.deepEqual(imported.accounts[0].tags, ['shared']);
});
test('fifty classified mailboxes retain categories after reload and batch changes are scoped and atomic', t => {
  const { manager, store } = setup(t);
  const rows = Array.from({ length: 50 }, (_, i) => manager.add({ provider: 'gmail', address: `owner${i}@gmail.com`, group: i < 25 ? 'Work' : 'Personal', tags: ['billing'] }));
  const selected = rows.slice(0, 10).map(row => row.id);
  manager.batch({ ids: selected, group: 'Clients', enabled: false });
  const restored = new MailManager(manager.config, store, manager.runner);
  assert.equal(restored.snapshot().items.length, 50);
  assert.ok(selected.every(id => restored.get(id).group === 'Clients' && restored.get(id).enabled === false));
  assert.equal(restored.get(rows[10].id).group, 'Work'); assert.equal(restored.get(rows[25].id).group, 'Personal');
  restored.runner.inflight.add(selected[1]);
  assert.throws(() => restored.batch({ ids: selected, group: 'Changed', enabled: true }), /finish/);
  assert.equal(restored.get(selected[0]).group, 'Clients'); assert.equal(restored.get(selected[0]).enabled, false);
  restored.runner.inflight.clear();
  assert.throws(() => restored.update(selected[0], { enabled: true, group: 'x'.repeat(81) }), /category/);
  assert.equal(restored.get(selected[0]).enabled, false);
  for (const format of ['csv', '1pux', 'json']) {
    const file = restored.export({ format, ids: selected }); const roundtrip = readImport(file.name, Buffer.from(file.data, 'base64'));
    assert.equal(roundtrip.accounts.length, 10); assert.ok(roundtrip.accounts.every(row => row.group === 'Clients' && row.tags[0] === 'billing'));
  }
});
test('malformed imports and compressed oversized data fail with credential-free errors', () => {
  assert.throws(() => readImport('bad.csv', Buffer.from('title,password\nsecret,"bad')), error => !error.message.includes('secret'));
  const zip = zipSync({ 'export.attributes': strToU8('{"version":3}'), 'export.data': new Uint8Array(9 * 1024 * 1024) });
  assert.throws(() => readImport('large.1pux', Buffer.from(zip)), /exceeds/);
  assert.throws(() => readImport('bad.json', Buffer.from('{"accounts":"password-value"}')), error => !error.message.includes('password-value'));
});
test('AI capabilities operate on opaque preview IDs and login receipts, never raw passwords or exported tokens', async t => {
  let calls = 0;
  const { manager } = setup(t, async (account, store, hooks) => { calls++; await hooks.google('https://accounts.google.com/o/oauth2/v2/auth?state=fixture'); return { poll() {} }; });
  manager.configureApps({ google_credentials: JSON.stringify({ installed: { client_id: 'fixture-client', client_secret: 'fixture-app-secret' } }) });
  const draft = manager.preview({ name: 'logins.csv', data: Buffer.from(csv).toString('base64') });
  const worker = new MailClientWorker(manager);
  assert.ok(!JSON.stringify(worker.execute('mail.import.preview', { draft_id: draft.id })).includes('apple-app-password'));
  assert.throws(() => worker.execute('mail.import.commit', { draft_id: draft.id, rows: ['0'], password: 'forbidden' }), /Unsupported/);
  const receipt = worker.execute('mail.import.commit', { draft_id: draft.id, rows: ['0'] });
  const job = worker.execute('mail.login.start', { account_id: receipt.imported[0] });
  assert.equal(calls, 1); assert.ok(!('url' in job));
  const same = worker.execute('mail.login.start', { account_id: receipt.imported[0] }); assert.equal(same.id, job.id); assert.equal(calls, 1);
  await manager.jobs.jobs.get(job.id).promise;
  assert.equal(worker.execute('mail.login.status', { job_id: job.id }).state, 'authorized');
  assert.ok(!JSON.stringify(worker.execute('mail.accounts', {})).includes('fixture-app-secret'));
  assert.throws(() => worker.execute('mail.export', {}), /Unsupported/);
});
test('local administration rejects unauthorized and browser-origin requests and returns only sanitized projections', async t => {
  const { manager } = setup(t); const token = 'fixture-administration-token-32-characters';
  const server = await adminServer(manager, token, 0); t.after(() => server.close());
  assert.equal((await fetch(server.url + '/accounts')).status, 401);
  assert.equal((await fetch(server.url + '/accounts', { headers: { Authorization: `Bearer ${token}`, Origin: server.url } })).status, 401);
  const headers = { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' };
  const saved = await fetch(server.url + '/accounts', { method: 'POST', headers, body: JSON.stringify({ provider: 'icloud', address: 'owner@icloud.com', password: secret, credential_kind: 'app_password' }) });
  assert.equal(saved.status, 200); assert.ok(!(await saved.text()).includes('fixture-password'));
  const snapshot = await (await fetch(server.url + '/accounts', { headers })).json(); assert.equal(snapshot.items.length, 1);
  manager.archive(snapshot.items[0].id); assert.equal(manager.snapshot().items.length, 0);
  const restored = manager.add({ provider: 'icloud', address: 'owner@icloud.com', password: 'new-app-password', credential_kind: 'app_password' });
  assert.equal(restored.id, snapshot.items[0].id); assert.equal(manager.get(restored.id).app_password, 'new-app-password');
});

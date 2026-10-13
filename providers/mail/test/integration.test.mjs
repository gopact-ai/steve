import test from 'node:test';
import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { createInterface } from 'node:readline';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { Store } from '../src/store.mjs';
import { Steve } from '../src/steve.mjs';
import { envelope } from '../src/message.mjs';

test('mail publication, lost response and scoped revocation use the real Steve HTTP service and ledger', {
  skip: process.env.STEVE_MAIL_INTEGRATION !== '1', timeout: 180000,
}, async t => {
  const root = fileURLToPath(new URL('../../../', import.meta.url));
  const temp = mkdtempSync(join(tmpdir(), 'steve-mail-integration-'));
  const binary = join(temp, 'assistant-fixture');
  const built = spawnSync('go', ['build', '-o', binary, './e2e/assistantfixture'], { cwd: root, encoding: 'utf8', timeout: 120000 });
  assert.equal(built.status, 0, built.stderr);
  const child = spawn(binary, [], { cwd: root, stdio: ['ignore', 'pipe', 'pipe'] });
  t.after(async () => {
    child.kill('SIGTERM');
    await new Promise(resolve => { if (child.exitCode !== null) resolve(); else child.once('exit', resolve); });
    rmSync(temp, { recursive: true, force: true });
  });
  const info = await new Promise((resolve, reject) => {
    const lines = createInterface({ input: child.stdout });
    const timer = setTimeout(() => reject(new Error('Steve fixture did not start')), 30000);
    lines.once('line', line => { clearTimeout(timer); lines.close(); resolve(JSON.parse(line)); });
    child.once('exit', () => { clearTimeout(timer); reject(new Error('Steve fixture exited')); });
  });
  const ownerHeaders = { Authorization: 'Bearer assistant-fixture-owner', 'Content-Type': 'application/json' };
  const enrolled = await fetch(`${info.url}/console/connections`, { method: 'POST', headers: ownerHeaders,
    body: JSON.stringify({ idempotency_key: 'sdk-mail-provider-test', name: 'SDK Mail Provider', kind: 'provider', event_types: ['mail.received'] }) });
  assert.equal(enrolled.status, 200);
  const { token, grant } = await enrolled.json();
  process.env.STEVE_MAIL_INTEGRATION_TOKEN = token;
  t.after(() => delete process.env.STEVE_MAIL_INTEGRATION_TOKEN);
  const config = { steve_url: info.url, steve_token_env: 'STEVE_MAIL_INTEGRATION_TOKEN' };
  const dir = join(temp, 'mail-state');
  let store = new Store(dir, Buffer.alloc(32, 9));
  let steve = new Steve(config, store);
  await steve.register();
  await steve.heartbeat();
  const account = { id: 'gmail-test', provider: 'gmail', address: 'owner@example.test' };
  store.account(account);
  const original = envelope(account, { id: 'immutable-one', receivedAt: new Date().toISOString(), subject: 'New real-service receipt', body: 'Fixture email external data' });
  store.stage(account, { history: '200' }, [original]);
  const realFetch = steve.fetch;
  steve.fetch = async (...args) => { await realFetch(...args); throw new Error('Lost durable server response'); };
  await assert.rejects(steve.drain(account.id));
  assert.equal(store.pending(account.id).length, 1);
  store.close();
  store = new Store(dir, Buffer.alloc(32, 9));
  t.after(() => store.close());
  steve = new Steve(config, store);
  await steve.register();
  assert.equal(await steve.drain(account.id), 1);
  assert.equal(store.pending(account.id).length, 0);
  const response = await fetch(`${info.url}/console/business-events`, { headers: ownerHeaders });
  const journal = await response.json();
  assert.equal(journal.items.filter(e => e.key === original.key).length, 1, 'uncertain acceptance must retain the original event identity');
  const event = journal.items.find(e => e.key === original.key);
  const details = await (await fetch(`${info.url}/console/business-events/${event.id}`, { headers: ownerHeaders })).json();
  assert.equal(details.payload.body, original.payload.body);
  assert.equal(details.contract.name, 'New email received');
  assert.equal((await fetch(`${info.url}/console/business-events`, { headers: { Authorization: `Bearer ${token}` } })).status, 401);
  assert.equal((await fetch(`${info.url}/console/connections/${grant.id}`, { method: 'DELETE', headers: ownerHeaders })).status, 200);
  await assert.rejects(steve.heartbeat(), e => [401, 403].includes(e.statusCode));
});

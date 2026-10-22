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

test('registration, contracts and lost-response publication against the real Steve service', {
  skip: process.env.STEVE_GITHUB_INTEGRATION !== '1', timeout: 180000,
}, async (t) => {
  const root = fileURLToPath(new URL('../../../', import.meta.url));
  const temp = mkdtempSync(join(tmpdir(), 'steve-github-integration-'));
  const binary = join(temp, 'assistant-fixture');
  const built = spawnSync('go', ['build', '-o', binary, './e2e/assistantfixture'], { cwd: root, encoding: 'utf8', timeout: 120000 });
  assert.equal(built.status, 0, built.stderr);
  const child = spawn(binary, [], { cwd: root, stdio: ['ignore', 'pipe', 'pipe'] });
  t.after(async () => {
    child.kill('SIGTERM');
    await new Promise((resolve) => { if (child.exitCode !== null) resolve(); else child.once('exit', resolve); });
    rmSync(temp, { recursive: true, force: true });
  });
  const info = await new Promise((resolve, reject) => {
    const lines = createInterface({ input: child.stdout });
    const timer = setTimeout(() => reject(new Error('Steve fixture did not start')), 30000);
    lines.once('line', (line) => { clearTimeout(timer); lines.close(); resolve(JSON.parse(line)); });
    child.once('exit', () => { clearTimeout(timer); reject(new Error('Steve fixture exited')); });
  });
  const owner = { Authorization: 'Bearer assistant-fixture-owner', 'Content-Type': 'application/json' };
  const enrolled = await fetch(`${info.url}/console/connections`, { method: 'POST', headers: owner,
    body: JSON.stringify({ idempotency_key: 'github-provider-test', name: 'GitHub Provider', kind: 'provider', event_types: ['github.*'] }) });
  assert.equal(enrolled.status, 200, await enrolled.clone().text());
  const { token } = await enrolled.json();
  process.env.STEVE_GITHUB_INTEGRATION_TOKEN = token;
  t.after(() => delete process.env.STEVE_GITHUB_INTEGRATION_TOKEN);
  const config = { steve_url: info.url, steve_token_env: 'STEVE_GITHUB_INTEGRATION_TOKEN' };
  const store = new Store(join(temp, 'state'));
  t.after(() => store.close());
  const steve = new Steve(config, store);
  await steve.register();
  await steve.heartbeat();
  const envelope = { key: 'github/acme/widget/checks/abc', type: 'github.checks.failed', schema_version: '1', subject: 'acme/widget#1',
    payload: { repository: 'acme/widget', number: 1, url: 'https://github.com/acme/widget/pull/1', head_sha: 'abc', checks: [{ name: 'test', conclusion: 'failure' }] } };
  store.stage('acme/widget', '2026-10-22T00:00:00Z', [], [envelope]);
  const real = steve.fetch;
  steve.fetch = async (...args) => { await real(...args); throw new Error('Lost durable server response'); };
  await assert.rejects(steve.drain());
  steve.fetch = real;
  assert.equal(await steve.drain(), 1);
  const journal = await (await fetch(`${info.url}/console/business-events`, { headers: owner })).json();
  const events = journal.items.filter((item) => item.key === envelope.key);
  assert.equal(events.length, 1, 'retry after a lost response created a second event');
  const details = await (await fetch(`${info.url}/console/business-events/${events[0].id}`, { headers: owner })).json();
  assert.equal(details.contract.name, 'Checks failed');
  assert.equal(details.payload.head_sha, 'abc');
});

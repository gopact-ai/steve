import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, readFileSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Store } from '../src/store.mjs';
import { envelope, BODY_LIMIT } from '../src/message.mjs';
import { Runner } from '../src/runner.mjs';
import { Steve } from '../src/steve.mjs';

const account = { id: 'gmail-a', provider: 'gmail', address: 'owner@example.test' };
const message = { id: 'one', subject: 'Planning', body: 'External data', receivedAt: '2026-10-13T10:00:00Z' };
function fixture(t) {
  const dir = mkdtempSync(join(tmpdir(), 'steve-mail-'));
  const key = Buffer.alloc(32, 7);
  const store = new Store(dir, key);
  t.after(() => { store.close(); rmSync(dir, { recursive: true, force: true }); });
  return { dir, key, store };
}

test('cursor and immutable outbox survive a lost publish response and restart', async t => {
  const { dir, key, store } = fixture(t);
  store.account(account);
  const original = envelope(account, message);
  store.stage(account, { history: '9007199254740999' }, [original]);
  const requests = [];
  const config = { steve_url: 'http://localhost', steve_token_env: 'STEVE_MAIL_TEST_TOKEN' };
  process.env.STEVE_MAIL_TEST_TOKEN = 'connection-fixture';
  t.after(() => delete process.env.STEVE_MAIL_TEST_TOKEN);
  let lose = true;
  const steve = new Steve(config, store, async (url, options) => {
    requests.push(JSON.parse(options.body));
    if (lose) { lose = false; throw new Error('Response lost after durable server acceptance'); }
    return { ok: true, json: async () => ({ id: 'event-original' }) };
  });
  await assert.rejects(steve.drain(account.id));
  const reopened = new Store(dir, key);
  try {
    assert.deepEqual(reopened.account(account).cursor, { history: '9007199254740999' });
    await new Steve(config, reopened, steve.fetch).drain(account.id);
    assert.deepEqual(requests, [original, original]);
    assert.equal(reopened.pending(account.id).length, 0);
    reopened.stage(account, { history: 'new' }, [envelope(account, { ...message, body: 'Changed later' })]);
    assert.equal(reopened.pending(account.id).length, 0);
    assert.equal(reopened.seen(original.key), true);
  } finally { reopened.close(); }
});

test('outbox capacity failure rolls back new messages and synchronization cursor', t => {
  const { store } = fixture(t);
  store.account(account);
  store.stage(account, { history: 'old' }, []);
  const many = Array.from({ length: 1001 }, (_, i) => envelope(account, { ...message, id: `id-${i}` }));
  assert.throws(() => store.stage(account, { history: 'new' }, many), /full/);
  assert.equal(store.account(account).cursor.history, 'old');
  assert.equal(store.pending(account.id).length, 0);
});

test('credential cache is encrypted, authenticated and isolated from message state', t => {
  const { store, dir, key } = fixture(t);
  const credentials = { refresh_token: 'sensitive-fixture-value' };
  store.secret('google:a', credentials);
  assert.deepEqual(store.secret('google:a'), credentials);
  assert.equal(readFileSync(join(dir, 'mail.sqlite')).includes(Buffer.from(credentials.refresh_token)), false);
  assert.equal(statSync(join(dir, 'mail.sqlite')).mode & 0o777, 0o600);
  const wrong = new Store(dir, Buffer.alloc(32, 8));
  try { assert.throws(() => wrong.secret('google:a')); } finally { wrong.close(); }
  store.db.prepare('INSERT INTO secrets SELECT ?,value FROM secrets WHERE key=?').run('google:b', 'google:a');
  assert.throws(() => store.secret('google:b'), 'a cache copied to another account must fail authentication');
  assert.equal(key.length, 32);
});

test('login binding does not start synchronization, and account identities cannot be repointed', t => {
  const { store } = fixture(t);
  assert.equal(store.account(account, false).cutoff, null);
  assert.ok(store.account(account).cutoff);
  assert.throws(() => store.account({ ...account, address: 'different@example.test' }), /identity changed/);
});

test('status can inspect an active private store without credential access or a write lock', t => {
  const { store, dir } = fixture(t);
  store.account(account);
  store.secret('google:a', { refresh_token: 'not-needed-for-status' });
  const reader = new Store(dir, Buffer.alloc(32), { readOnly: true });
  try {
    assert.equal(reader.status()[0].id, account.id);
    assert.throws(() => reader.set('mutate', 'forbidden'));
  } finally { reader.close(); }
});

test('body limits are byte based and account namespaces keep equal message IDs independent', () => {
  const mail = envelope(account, { ...message, body: '邮件正文'.repeat(20000) });
  assert.ok(Buffer.byteLength(mail.payload.body) <= BODY_LIMIT);
  assert.equal(mail.payload.body_truncated, true);
  assert.ok(Buffer.byteLength(JSON.stringify(mail.payload)) < 256 * 1024);
  assert.notEqual(mail.key, envelope({ ...account, id: 'gmail-b' }, message).key);
});

test('one unavailable mailbox does not block another, and failed publication keeps its outbox', async t => {
  const { store } = fixture(t);
  const other = { ...account, id: 'outlook-b', provider: 'outlook' };
  const rows = [];
  const steve = { drain: async id => { if (id === account.id && store.pending(id).length) throw new Error('Disconnected'); return 0; } };
  const runner = new Runner({ accounts: [account, other] }, store, steve, async a => ({
    poll: async () => ({ cursor: { position: 1 }, messages: [envelope(a, message)] }),
  }), row => rows.push(row));
  assert.equal(await runner.cycle(), false);
  assert.equal(rows.find(r => r.account === account.id).state, 'error');
  assert.equal(rows.find(r => r.account === other.id).state, 'ready');
  assert.equal(store.pending(account.id).length, 1);
  assert.equal(store.account(account).cursor.position, 1);
});

test('background polling keeps a healthy mailbox advancing while another SDK request waits', { timeout: 2000 }, async t => {
  const { store } = fixture(t);
  const fast = { ...account, id: 'fast', provider: 'outlook' };
  const stop = new AbortController();
  let release;
  const blocked = new Promise(resolve => { release = resolve; });
  let polls = 0;
  const runner = new Runner({ accounts: [account, fast], poll_seconds: 0.01 }, store, { drain: async () => 0 }, async a => ({
    async poll() {
      if (a.id === account.id) await blocked;
      else if (++polls === 2) { stop.abort(); release(); }
      return { cursor: { sequence: polls }, messages: [] };
    },
  }), () => {});
  await runner.run(stop.signal);
  assert.equal(polls, 2);
});

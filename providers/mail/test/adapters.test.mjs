import test from 'node:test';
import assert from 'node:assert/strict';
import { GmailInbox } from '../src/google.mjs';
import { MicrosoftInbox, graphCursor } from '../src/microsoft.mjs';
import { ICloudInbox } from '../src/icloud.mjs';

const cutoff = '2026-10-13T00:00:00Z';
const gmailAccount = { id: 'gmail', provider: 'gmail', address: 'owner@example.test' };
const gmailMessage = id => ({ id, internalDate: String(Date.parse(cutoff) + 10000), labelIds: ['INBOX'],
  payload: { mimeType: 'text/plain', headers: [{ name: 'Subject', value: 'A new message' }], body: { data: Buffer.from('Useful body').toString('base64url') } } });

test('Gmail initial connection records history without emitting historical mail', async () => {
  const api = { users: { getProfile: async () => ({ data: { historyId: '90071992547409999' } }) } };
  const result = await new GmailInbox(gmailAccount, api).poll(null, cutoff);
  assert.deepEqual(result, { cursor: { history: '90071992547409999' }, messages: [] });
});

test('Gmail history pagination retains its original start and only reads message additions', async () => {
  const queries = [];
  const api = { users: { history: { list: async q => { queries.push(q); return { data: {
    historyId: '999', nextPageToken: q.pageToken ? undefined : 'page-2', history: [{
      messages: [{ id: 'changed-read-state' }], messagesAdded: [{ message: { id: 'new' } }, { message: { id: 'new' } }],
    }],
  } }; } }, messages: { get: async q => ({ data: gmailMessage(q.id) }) } } };
  const adapter = new GmailInbox(gmailAccount, api);
  const page1 = await adapter.poll({ history: '123' }, cutoff);
  assert.equal(page1.messages.length, 1);
  assert.deepEqual(page1.cursor, { history: '123', page: 'page-2' });
  const page2 = await adapter.poll(page1.cursor, cutoff);
  assert.equal(queries[1].startHistoryId, '123');
  assert.deepEqual(queries[1].historyTypes, ['messageAdded']);
  assert.equal(page2.cursor.history, '999');
});

test('expired Gmail history performs a bounded inbox rescan from the original cutoff', async () => {
  let query;
  const api = { users: {
    history: { list: async () => { const e = new Error(); e.code = 404; throw e; } },
    getProfile: async () => ({ data: { historyId: 'snapshot-before-list' } }),
    messages: { list: async q => { query = q; return { data: { messages: [{ id: 'old' }, { id: 'new' }] } }; },
      get: async q => ({ data: q.id === 'old' ? { ...gmailMessage(q.id), internalDate: '1' } : gmailMessage(q.id) }) },
  } };
  const result = await new GmailInbox(gmailAccount, api).poll({ history: 'expired' }, cutoff);
  assert.equal(result.messages.length, 1);
  assert.equal(result.cursor.history, 'snapshot-before-list');
  assert.equal(query.q, `after:${Date.parse(cutoff) / 1000}`);
});

for (const provider of ['outlook', 'hotmail']) {
  test(`${provider} uses Graph created-message deltas and ignores deletion/read-state noise`, async () => {
    const urls = [], headers = [];
    const next = 'https://graph.microsoft.com/v1.0/me/mailFolders/inbox/messages/delta?$deltatoken=original';
    const account = { id: provider, provider, address: 'owner@example.test' };
    const api = { api(url) { urls.push(url); return { option() { return this; }, header(name, value) { headers.push([name, value]); return this; }, async get() {
      return { value: [
        { id: 'deleted', '@removed': { reason: 'deleted' } },
        { id: 'old', receivedDateTime: '2000-01-01T00:00:00Z' },
        { id: 'new', receivedDateTime: cutoff, subject: 'News', body: { contentType: 'html', content: '<p>Read me</p>' } },
      ], '@odata.deltaLink': next };
    } }; } };
    const adapter = new MicrosoftInbox(account, api);
    const result = await adapter.poll(null, cutoff);
    assert.match(urls[0], /changeType=created/);
    assert.equal(result.messages.length, 1);
    assert.equal(result.messages[0].payload.provider, provider);
    assert.equal(result.messages[0].payload.body, 'Read me');
    assert.match(headers[0][1], /ImmutableId/);
    await adapter.poll(result.cursor, cutoff);
    assert.equal(urls[1], next);
    assert.throws(() => graphCursor('https://attacker.example/v1.0/me'));
  });
}

test('Graph expired delta rescan keeps the original time boundary', async () => {
  const urls = [];
  const api = { api(url) { urls.push(url); return { option() { return this; }, header() { return this; }, async get() {
    if (urls.length === 1) { const error = new Error(); error.statusCode = 410; throw error; }
    return { value: [], '@odata.deltaLink': 'https://graph.microsoft.com/v1.0/me/mailFolders/inbox/messages/delta?$deltatoken=fresh' };
  } }; } };
  await new MicrosoftInbox({ id: 'outlook', provider: 'outlook' }, api).poll({ url: 'https://graph.microsoft.com/v1.0/expired' }, cutoff);
  assert.ok(decodeURIComponent(urls[1].replaceAll('+', ' ')).includes(`receivedDateTime ge ${cutoff}`));
});

test('iCloud preserves read-only mailbox state and uses a stable identity across UIDVALIDITY changes', async t => {
  process.env.STEVE_MAIL_TEST_ICLOUD = 'app-password-fixture';
  t.after(() => delete process.env.STEVE_MAIL_TEST_ICLOUD);
  const mail = Buffer.from('From: Sender <sender@example.test>\r\nTo: owner@example.test\r\nSubject: New mail\r\nMessage-ID: <one@example.test>\r\n\r\nContent');
  const calls = [];
  let validity = 1;
  const factory = options => {
    assert.equal(options.secure, true);
    assert.equal(options.logger, false);
    return { mailbox: { uidValidity: validity, uidNext: 4 }, connect: async () => {}, logout: async () => {},
      async getMailboxLock(name, opts) { assert.equal(opts.readOnly, true); return { release() {} }; },
      async search(query) { calls.push(query); return [1, 2, 3]; },
      async fetchOne(uid, query) { assert.equal(query.source.maxLength, 2 * 1024 * 1024); return { uid: Number(uid), size: mail.length, source: mail, internalDate: new Date(cutoff) }; },
    };
  };
  const adapter = new ICloudInbox({ id: 'icloud', provider: 'icloud', address: 'owner@example.test', password_env: 'STEVE_MAIL_TEST_ICLOUD' }, factory);
  assert.deepEqual((await adapter.poll(null, cutoff)).messages, []);
  const first = await adapter.poll({ validity: '1', uid: 2 }, cutoff);
  assert.equal(first.messages.length, 1);
  assert.deepEqual(calls[0], { uid: '3:3' });
  validity = 2;
  const reset = await adapter.poll(first.cursor, cutoff);
  assert.equal(reset.messages[0].key, first.messages[0].key);
  assert.equal(reset.cursor.validity, '2');
  assert.ok(calls[1].since instanceof Date);
});

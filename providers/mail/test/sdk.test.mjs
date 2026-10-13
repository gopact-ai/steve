import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { gmail } from '@googleapis/gmail';
import { OAuth2Client } from 'google-auth-library';
import { Client } from '@microsoft/microsoft-graph-client';
import { GmailInbox } from '../src/google.mjs';
import { MicrosoftInbox } from '../src/microsoft.mjs';

async function server(t, respond) {
  const instance = createServer(respond);
  await new Promise(resolve => instance.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => instance.close(resolve)));
  return `http://127.0.0.1:${instance.address().port}`;
}
const cutoff = '2026-10-13T00:00:00Z';

test('official Google SDK supplies OAuth headers and decodes Gmail history/messages', async t => {
  const requests = [];
  const url = await server(t, (req, res) => {
    requests.push({ url: req.url, auth: req.headers.authorization });
    res.setHeader('Content-Type', 'application/json');
    const route = new URL(req.url, 'http://fixture');
    if (route.pathname.endsWith('/history')) res.end(JSON.stringify({ historyId: '124', history: [{ messagesAdded: [{ message: { id: 'one' } }] }] }));
    else res.end(JSON.stringify({ id: 'one', labelIds: ['INBOX'], internalDate: String(Date.parse(cutoff)),
      payload: { mimeType: 'text/plain', headers: [{ name: 'Subject', value: 'SDK message' }], body: { data: Buffer.from('Official SDK body').toString('base64url') } } }));
  });
  const auth = new OAuth2Client();
  auth.setCredentials({ access_token: 'oauth-fixture', expiry_date: Date.now() + 3600000 });
  const api = gmail({ version: 'v1', auth, rootUrl: `${url}/` });
  const result = await new GmailInbox({ id: 'gmail', provider: 'gmail', address: 'owner@example.test' }, api).poll({ history: '123' }, cutoff);
  assert.equal(result.messages[0].payload.body, 'Official SDK body');
  assert.ok(requests.every(r => r.auth === 'Bearer oauth-fixture'));
  assert.ok(requests[0].url.includes('startHistoryId=123'));
  assert.ok(requests[1].url.includes('format=full'));
});

test('official Graph SDK sends delegated auth and immutable ID preference for both Microsoft mailbox brands', async t => {
  const requests = [];
  const url = await server(t, (req, res) => {
    requests.push({ url: req.url, auth: req.headers.authorization, prefer: req.headers.prefer });
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ value: [{ id: 'one', receivedDateTime: cutoff, subject: 'Graph message', body: { contentType: 'text', content: 'Official Graph body' } }],
      '@odata.deltaLink': 'https://graph.microsoft.com/v1.0/me/mailFolders/inbox/messages/delta?$deltatoken=next' }));
  });
  // Keep the SDK's production host checks intact. Redirect only this test's
  // final fetch transport to a disposable loopback fixture.
  const realFetch = globalThis.fetch;
  globalThis.fetch = (input, options) => {
    const parsed = new URL(input);
    assert.equal(parsed.origin, 'https://graph.microsoft.com');
    return realFetch(`${url}${parsed.pathname}${parsed.search}`, options);
  };
  t.after(() => { globalThis.fetch = realFetch; });
  const api = Client.initWithMiddleware({ authProvider: { getAccessToken: async () => 'graph-fixture' } });
  for (const provider of ['outlook', 'hotmail']) {
    const result = await new MicrosoftInbox({ id: provider, provider, address: 'owner@example.test' }, api).poll(null, cutoff);
    assert.equal(result.messages[0].payload.body, 'Official Graph body');
  }
  assert.ok(requests.every(r => r.auth === 'Bearer graph-fixture'));
  assert.ok(requests.every(r => r.prefer.includes('ImmutableId')));
  assert.ok(requests.every(r => r.url.includes('changeType=created')));
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:net';
import { ImapFlow } from 'imapflow';
import { ICloudInbox } from '../src/icloud.mjs';

test('ImapFlow uses EXAMINE and bounded BODY.PEEK on the wire without modifying flags', { timeout: 20000 }, async t => {
  const commands = [];
  const raw = Buffer.from('From: sender@example.test\r\nTo: owner@example.test\r\nSubject: IMAP SDK\r\nMessage-ID: <imap-one@example.test>\r\n\r\nA body from IMAP');
  const sockets = new Set();
  const server = createServer(socket => {
    sockets.add(socket);
    socket.on('close', () => sockets.delete(socket));
    socket.write('* OK [CAPABILITY IMAP4rev1] test ready\r\n');
    let buffer = '';
    socket.on('data', chunk => {
      buffer += chunk.toString();
      while (buffer.includes('\r\n')) {
        const end = buffer.indexOf('\r\n');
        const line = buffer.slice(0, end); buffer = buffer.slice(end + 2);
        const [tag, command] = line.split(' ');
        commands.push(line);
        if (command === 'CAPABILITY') socket.write(`* CAPABILITY IMAP4rev1\r\n${tag} OK capability\r\n`);
        else if (command === 'LOGIN') socket.write(`${tag} OK authenticated\r\n`);
        else if (command === 'LIST') socket.write(`* LIST () "/" "INBOX"\r\n${tag} OK list\r\n`);
        else if (command === 'LSUB') socket.write(`${tag} OK subscriptions\r\n`);
        else if (command === 'EXAMINE') socket.write(`* 3 EXISTS\r\n* FLAGS (\\Seen)\r\n* OK [UIDVALIDITY 1] validity\r\n* OK [UIDNEXT 4] next\r\n${tag} OK [READ-ONLY] selected\r\n`);
        else if (line.includes('UID SEARCH')) socket.write(`* SEARCH 3\r\n${tag} OK search\r\n`);
        else if (line.includes('UID FETCH')) {
          socket.write(`* 3 FETCH (UID 3 INTERNALDATE "13-Oct-2026 00:00:01 +0000" RFC822.SIZE ${raw.length} BODY[]<0> {${raw.length}}\r\n`);
          socket.write(raw); socket.write(`)\r\n${tag} OK fetched\r\n`);
        } else if (command === 'LOGOUT') socket.end(`* BYE goodbye\r\n${tag} OK logout\r\n`);
        else socket.write(`${tag} BAD unsupported test command\r\n`);
      }
    });
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => { for (const socket of sockets) socket.destroy(); return new Promise(resolve => server.close(resolve)); });
  process.env.STEVE_MAIL_WIRE_PASSWORD = 'fixture-password';
  t.after(() => delete process.env.STEVE_MAIL_WIRE_PASSWORD);
  const account = { id: 'icloud-wire', provider: 'icloud', address: 'owner@example.test', password_env: 'STEVE_MAIL_WIRE_PASSWORD' };
  const adapter = new ICloudInbox(account, options => new ImapFlow({ ...options, host: '127.0.0.1', port: server.address().port,
    secure: false, doSTARTTLS: false }));
  const result = await adapter.poll({ validity: '1', uid: 2 }, '2026-10-13T00:00:00Z');
  assert.equal(result.messages[0].payload.body, 'A body from IMAP');
  assert.ok(commands.some(c => c.includes('EXAMINE')));
  assert.ok(commands.some(c => /BODY.PEEK\[\]<0\.2097152>/.test(c)));
  assert.ok(commands.every(c => !/ (STORE|APPEND|EXPUNGE|SELECT) /.test(c)));
});

import { SetupError } from './errors.mjs';
import { createHash } from 'node:crypto';
import { ImapFlow } from 'imapflow';
import { simpleParser } from 'mailparser';
import { envelope, plainText } from './message.mjs';
import { requiredEnv } from './config.mjs';

export class ICloudInbox {
  constructor(account, factory = options => new ImapFlow(options)) { this.account = account; this.factory = factory; }
  async poll(cursor, cutoff) {
    const account = this.account;
    const client = this.factory({ host: 'imap.mail.me.com', port: 993, secure: true,
      auth: { user: account.username || account.address, pass: account.app_password || requiredEnv(account.password_env || 'STEVE_ICLOUD_APP_PASSWORD') },
      logger: false, emitLogs: false, connectionTimeout: 30000, greetingTimeout: 30000, socketTimeout: 60000 });
    let lock;
    const deadline = setTimeout(() => client.close(), 90000);
    try {
      await client.connect();
      lock = await client.getMailboxLock('INBOX', { readOnly: true });
      const validity = String(client.mailbox.uidValidity);
      const last = client.mailbox.uidNext - 1;
      if (!cursor) return { cursor: { validity, uid: last }, messages: [] };
      const reset = cursor.validity !== validity;
      const start = reset ? 1 : cursor.uid + 1;
      if (start > last) return { cursor: { validity, uid: last }, messages: [] };
      const query = reset ? { since: new Date(cutoff) } : { uid: `${start}:${last}` };
      const uids = (await client.search(query, { uid: true })).filter(uid => uid >= start && uid <= last).sort((a, b) => a - b).slice(0, 100);
      const messages = [];
      for (const uid of uids) {
        // ImapFlow issues BODY.PEEK and keeps the mailbox read-only. Limit raw
        // MIME bytes; attachment bodies are neither published nor separately fetched.
        const item = await client.fetchOne(String(uid), { uid: true, internalDate: true, size: true,
          source: { start: 0, maxLength: 2 * 1024 * 1024 } }, { uid: true });
        if (!item) continue;
        if (!item.source) throw new SetupError('iCloud fetch did not return the requested message');
        if (item.internalDate.getTime() < Date.parse(cutoff)) continue;
        const parsed = await simpleParser(item.source, { skipImageLinks: true, skipTextToHtml: true });
        const id = createHash('sha256').update(item.source).update(String(item.size)).update(item.internalDate.toISOString()).digest('hex');
        messages.push(envelope(account, { id, internetId: parsed.messageId, from: parsed.from?.text,
          to: parsed.to?.text, subject: parsed.subject, receivedAt: item.internalDate.toISOString(),
          body: parsed.text || plainText(parsed.html, true), hasAttachments: !!parsed.attachments.length,
          truncated: item.size > item.source.length }));
      }
      const uid = uids.length ? uids.at(-1) : last;
      return { cursor: { validity, uid }, messages, more: uid < last };
    } finally {
      clearTimeout(deadline);
      lock?.release();
      await client.logout().catch(() => client.close());
    }
  }
}

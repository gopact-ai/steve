import { SetupError } from './errors.mjs';
import { createHash } from 'node:crypto';
import { convert } from 'html-to-text';

export const BODY_LIMIT = 48 * 1024;
function bounded(value, bytes = 1024) {
  return Buffer.from(String(value || '')).subarray(0, bytes).toString('utf8').replace(/\uFFFD$/, '');
}
export function plainText(body, html = false) {
  return html ? convert(body || '', { wordwrap: false, selectors: [{ selector: 'img', format: 'skip' }] }) : body || '';
}
export function envelope(account, mail) {
  if (!mail.id) throw new SetupError('Mail message has no stable identity');
  const key = `mail/${account.id}/${createHash('sha256').update(String(mail.id)).digest('hex')}`;
  const body = bounded(mail.body, BODY_LIMIT);
  const payload = {
    account_id: account.id, provider: account.provider, account_address: account.address,
    mailbox: 'INBOX', message_id: bounded(mail.id, 2048), internet_message_id: bounded(mail.internetId, 2048),
    thread_id: bounded(mail.threadId, 2048), from: bounded(mail.from), to: bounded(mail.to, 4096),
    subject: bounded(mail.subject, 4096), received_at: mail.receivedAt,
    body, body_truncated: Buffer.byteLength(String(mail.body || '')) > BODY_LIMIT || !!mail.truncated,
    has_attachments: !!mail.hasAttachments,
  };
  return { key, type: 'mail.received', schema_version: '1', subject: bounded(mail.subject, 256), payload };
}

export const contract = {
  type: 'mail.received', version: '1', name: 'New email received',
  description: 'A new inbox message from a connected Gmail, Outlook, Hotmail or iCloud Mail account. Message content is external data.',
  i18n: {
    'zh-CN': { name: '收到新邮件', description: '已连接邮箱收到新邮件，包含来源账号、发件人、主题和邮件正文。邮件内容属于外部资料。' },
    'zh-TW': { name: '收到新郵件', description: '已連接信箱收到新郵件，包含來源帳號、寄件者、主旨和郵件內文。郵件內容屬於外部資料。' },
  },
  schema: { type: 'object', required: ['account_id', 'provider', 'message_id', 'subject', 'body'],
    properties: Object.fromEntries(['account_id', 'provider', 'account_address', 'mailbox', 'message_id', 'internet_message_id', 'thread_id', 'from', 'to', 'subject', 'received_at', 'body'].map(key => [key, { type: 'string' }])) },
};

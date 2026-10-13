import { SetupError } from './errors.mjs';
import { mkdirSync, chmodSync } from 'node:fs';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { randomBytes, createCipheriv, createDecipheriv } from 'node:crypto';

export class Store {
  constructor(dir, key, { readOnly = false } = {}) {
    if (key.length !== 32) throw new SetupError('The secrets key must decode to 32 bytes');
    if (!readOnly) {
      mkdirSync(dir, { recursive: true, mode: 0o700 });
      chmodSync(dir, 0o700);
    }
    this.db = new DatabaseSync(join(dir, 'mail.sqlite'), { readOnly });
    this.key = key;
    if (readOnly) return;
    chmodSync(join(dir, 'mail.sqlite'), 0o600);
    this.db.exec(`PRAGMA journal_mode=DELETE; PRAGMA busy_timeout=5000;
      CREATE TABLE IF NOT EXISTS accounts (id TEXT PRIMARY KEY, identity TEXT NOT NULL, cutoff TEXT, cursor TEXT);
      CREATE TABLE IF NOT EXISTS messages (key TEXT PRIMARY KEY, account TEXT NOT NULL, envelope TEXT, receipt TEXT);
      CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS secrets (key TEXT PRIMARY KEY, value TEXT NOT NULL);`);
  }
  close() { this.db.close(); }
  get(name) { return this.db.prepare('SELECT value FROM settings WHERE key=?').get(name)?.value; }
  set(name, value) { this.db.prepare('INSERT OR REPLACE INTO settings VALUES (?,?)').run(name, value); }
  secret(name, value) {
    if (value === undefined) {
      const stored = this.db.prepare('SELECT value FROM secrets WHERE key=?').get(name)?.value;
      if (!stored) return undefined;
      const blob = Buffer.from(stored, 'base64');
      const cipher = createDecipheriv('aes-256-gcm', this.key, blob.subarray(0, 12));
      cipher.setAAD(Buffer.from(name));
      cipher.setAuthTag(blob.subarray(12, 28));
      return JSON.parse(Buffer.concat([cipher.update(blob.subarray(28)), cipher.final()]).toString());
    }
    const iv = randomBytes(12);
    const cipher = createCipheriv('aes-256-gcm', this.key, iv);
    cipher.setAAD(Buffer.from(name));
    const data = Buffer.concat([cipher.update(JSON.stringify(value)), cipher.final()]);
    this.db.prepare('INSERT OR REPLACE INTO secrets VALUES (?,?)').run(name, Buffer.concat([iv, cipher.getAuthTag(), data]).toString('base64'));
  }
  account(account, start = true) {
    const identity = JSON.stringify([account.provider, account.address.toLowerCase(), account.client_id || '', account.credentials_file || '']);
    let row = this.db.prepare('SELECT * FROM accounts WHERE id=?').get(account.id);
    if (row && row.identity !== identity) throw new SetupError('Account identity changed: configure a new stable account ID');
    if (!row) {
      this.db.prepare('INSERT INTO accounts VALUES (?,?,NULL,NULL)').run(account.id, identity);
      row = this.db.prepare('SELECT * FROM accounts WHERE id=?').get(account.id);
    }
    if (start && !row.cutoff) {
      row.cutoff = new Date().toISOString();
      this.db.prepare('UPDATE accounts SET cutoff=? WHERE id=?').run(row.cutoff, account.id);
    }
    return { cutoff: row.cutoff, cursor: row.cursor ? JSON.parse(row.cursor) : null };
  }
  seen(key) { return !!this.db.prepare('SELECT key FROM messages WHERE key=?').get(key); }
  stage(account, cursor, envelopes) {
    this.db.exec('BEGIN IMMEDIATE');
    try {
      const insert = this.db.prepare('INSERT OR IGNORE INTO messages (key,account,envelope) VALUES (?,?,?)');
      for (const envelope of envelopes) insert.run(envelope.key, account.id, JSON.stringify(envelope));
      const count = this.db.prepare('SELECT count(*) AS count FROM messages WHERE envelope IS NOT NULL').get().count;
      if (count > 1000) throw new SetupError('Mail outbox is full; restore the Steve connection before synchronizing');
      this.db.prepare('UPDATE accounts SET cursor=? WHERE id=?').run(JSON.stringify(cursor), account.id);
      this.db.exec('COMMIT');
    } catch (error) { this.db.exec('ROLLBACK'); throw error; }
  }
  pending(account) {
    return this.db.prepare('SELECT key,envelope FROM messages WHERE account=? AND envelope IS NOT NULL ORDER BY rowid LIMIT 100').all(account);
  }
  published(key, receipt) { this.db.prepare('UPDATE messages SET envelope=NULL,receipt=? WHERE key=?').run(receipt, key); }
  status() {
    return this.db.prepare(`SELECT a.id,a.cutoff,a.cursor IS NOT NULL AS initialized,
      count(m.envelope) AS pending FROM accounts a LEFT JOIN messages m ON m.account=a.id GROUP BY a.id`).all();
  }
}

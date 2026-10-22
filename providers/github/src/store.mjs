import { mkdirSync, chmodSync } from 'node:fs';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { SetupError } from './errors.mjs';

// One process owns a state directory: the database is opened in exclusive
// locking mode, so a second provider fails instead of publishing twice.
export class Store {
  constructor(dir, { readOnly = false } = {}) {
    if (!readOnly) { mkdirSync(dir, { recursive: true, mode: 0o700 }); chmodSync(dir, 0o700); }
    this.db = new DatabaseSync(join(dir, 'github.sqlite'), { readOnly });
    if (readOnly) return;
    chmodSync(join(dir, 'github.sqlite'), 0o600);
    try { this.db.exec('PRAGMA busy_timeout=0; PRAGMA locking_mode=EXCLUSIVE; BEGIN EXCLUSIVE; COMMIT'); }
    catch { this.db.close(); throw new SetupError('Another provider process owns this state directory'); }
    this.db.exec(`PRAGMA journal_mode=DELETE;
      CREATE TABLE IF NOT EXISTS repositories (name TEXT PRIMARY KEY, since TEXT);
      CREATE TABLE IF NOT EXISTS pulls (repository TEXT, number INTEGER, head TEXT NOT NULL, state TEXT NOT NULL, PRIMARY KEY (repository, number));
      CREATE TABLE IF NOT EXISTS outbox (key TEXT PRIMARY KEY, repository TEXT NOT NULL, envelope TEXT, receipt TEXT);
      CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);`);
  }
  close() { this.db.close(); }
  get(name) { return this.db.prepare('SELECT value FROM settings WHERE key=?').get(name)?.value; }
  set(name, value) { this.db.prepare('INSERT OR REPLACE INTO settings VALUES (?,?)').run(name, value); }
  since(repository) { return this.db.prepare('SELECT since FROM repositories WHERE name=?').get(repository)?.since || null; }
  pull(repository, number) { return this.db.prepare('SELECT head,state FROM pulls WHERE repository=? AND number=?').get(repository, number) || null; }
  seen(key) { return !!this.db.prepare('SELECT key FROM outbox WHERE key=?').get(key); }
  // A poll commits its cursor, observed pull states and new envelopes together.
  stage(repository, since, pulls, envelopes) {
    this.db.exec('BEGIN IMMEDIATE');
    try {
      const pull = this.db.prepare('INSERT OR REPLACE INTO pulls VALUES (?,?,?,?)');
      for (const item of pulls) pull.run(repository, item.number, item.head, item.state);
      const insert = this.db.prepare('INSERT OR IGNORE INTO outbox (key,repository,envelope,receipt) VALUES (?,?,?,?)');
      for (const { baseline, ...envelope } of envelopes) {
        if (baseline) insert.run(envelope.key, repository, null, 'baseline');
        else insert.run(envelope.key, repository, JSON.stringify(envelope), null);
      }
      const backlog = this.db.prepare('SELECT count(*) AS count FROM outbox WHERE envelope IS NOT NULL').get().count;
      if (backlog > 2000) throw new SetupError('Outbox is full; restore the Steve connection before polling');
      this.db.prepare('INSERT OR REPLACE INTO repositories VALUES (?,?)').run(repository, since);
      this.db.exec('COMMIT');
    } catch (error) { this.db.exec('ROLLBACK'); throw error; }
  }
  pending() { return this.db.prepare('SELECT key,envelope FROM outbox WHERE envelope IS NOT NULL ORDER BY rowid LIMIT 100').all(); }
  published(key, receipt) { this.db.prepare('UPDATE outbox SET envelope=NULL,receipt=? WHERE key=?').run(receipt, key); }
  status() {
    return this.db.prepare(`SELECT r.name AS repository, r.since, count(o.envelope) AS pending
      FROM repositories r LEFT JOIN outbox o ON o.repository=r.name GROUP BY r.name`).all();
  }
}

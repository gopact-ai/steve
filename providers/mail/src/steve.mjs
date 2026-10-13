import { SetupError } from './errors.mjs';
import { contract } from './message.mjs';
import { requiredEnv } from './config.mjs';

export class Steve {
  constructor(config, store, fetcher = fetch) { this.config = config; this.store = store; this.fetch = fetcher; }
  async request(path, body) {
    const response = await this.fetch(`${this.config.steve_url}${path}`, {
      method: body === undefined ? 'GET' : 'POST', redirect: 'error', signal: AbortSignal.timeout(30000),
      headers: { Authorization: `Bearer ${requiredEnv(this.config.steve_token_env)}`, 'Content-Type': 'application/json' },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    if (!response.ok) { const error = new SetupError('Steve rejected the provider request'); error.statusCode = response.status; throw error; }
    return response.json();
  }
  async register() {
    let old;
    try { old = await this.request('/services/provider'); }
    catch (error) { if (error.statusCode !== 400) throw error; }
    if (old && old.plugin_id !== 'steve.mail') throw new SetupError('Use a dedicated Steve mail provider connection');
    const registration = await this.request('/services/provider/register', {
      generation: old?.generation || 0, plugin_id: 'steve.mail', version: '0.1.0', events: [contract],
    });
    const previous = this.store.get('steve-grant');
    if (previous && previous !== registration.id) throw new SetupError('The mail state belongs to a different Steve connection; retain its identity when rotating credentials');
    this.store.set('steve-grant', registration.id);
    this.generation = registration.generation;
  }
  async heartbeat() { await this.request('/services/provider/heartbeat', { generation: this.generation }); }
  async drain(account) {
    let count = 0;
    for (;;) {
      const rows = this.store.pending(account);
      if (!rows.length) return count;
      for (const row of rows) {
        const accepted = await this.request('/services/events', JSON.parse(row.envelope));
        if (!accepted.id) throw new SetupError('Steve returned no durable event identity');
        this.store.published(row.key, accepted.id);
        count++;
      }
    }
  }
}

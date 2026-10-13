import { mkdirSync, chmodSync } from 'node:fs';
import { parseArgs } from 'node:util';
import { resolve } from 'node:path';
import lockfile from 'proper-lockfile';
import { loadConfig, requiredEnv } from './config.mjs';
import { Store } from './store.mjs';
import { Steve } from './steve.mjs';
import { Runner, adapter, errorCode } from './runner.mjs';
import { SetupError } from './errors.mjs';
import { serveMail } from './daemon.mjs';

async function main() {
  const args = parseArgs({ allowPositionals: true, options: {
    config: { type: 'string', default: 'config.json' }, account: { type: 'string' }, help: { type: 'boolean' },
  } });
  const command = args.positionals[0] || 'run';
  if (args.values.help) {
    console.log('node src/cli.mjs [serve|login|run|once|status] --config config.json [--account account-id]');
    return;
  }
  if (!['serve', 'login', 'run', 'once', 'status'].includes(command) || args.positionals.length > 1) throw new SetupError('Unknown command; use --help');
  const config = loadConfig(resolve(args.values.config), { managed: ['serve', 'status'].includes(command) });
  if (command === 'status') {
    const store = new Store(config.state_dir, Buffer.alloc(32), { readOnly: true });
    try {
      console.log(JSON.stringify(store.status().map(row => ({ ...row, health: JSON.parse(store.get(`health:${row.id}`) || 'null') })), null, 2));
    } finally { store.close(); }
    return;
  }
  const key = Buffer.from(requiredEnv(config.secrets_key_env), 'base64');
  mkdirSync(config.state_dir, { recursive: true, mode: 0o700 });
  chmodSync(config.state_dir, 0o700);
  const release = await lockfile.lock(config.state_dir, { stale: 30000, update: 10000, retries: 0 });
  const store = new Store(config.state_dir, key);
  let timer;
  let heartbeat;
  try {
    if (command === 'login') {
      const account = config.accounts.find(a => a.id === args.values.account);
      if (!account) throw new SetupError('Select a configured account with --account');
      // Bind the stable account before SDK credentials are stored. A new login
      // cannot silently point old message receipts at another mailbox.
      store.account(account, false);
      const client = await adapter(account, store, true);
      if (account.provider === 'icloud') await client.poll(null, new Date().toISOString());
      store.set(`authorized:${account.id}`, '1');
      console.log(JSON.stringify({ account: account.id, state: 'authorized' }));
      return;
    }
    const steve = new Steve(config, store);
    await steve.register();
    const runner = new Runner(config, store, steve);
    const stop = new AbortController();
    const onStop = () => stop.abort();
    process.once('SIGINT', onStop);
    process.once('SIGTERM', onStop);
    timer = setInterval(() => {
      if (heartbeat) return;
      heartbeat = steve.heartbeat().catch(error => {
        console.error(JSON.stringify({ service: 'steve', state: 'error', code: errorCode(error) }));
        if ([401, 403, 409].includes(error.statusCode)) { process.exitCode = 1; stop.abort(); }
      }).finally(() => { heartbeat = null; });
    }, 15000);
    if (command === 'serve') await serveMail(config, store, steve, stop.signal);
    else if (command === 'once') process.exitCode = await runner.cycle() ? 0 : 1;
    else await runner.run(stop.signal);
    process.removeListener('SIGINT', onStop);
    process.removeListener('SIGTERM', onStop);
  } finally {
    clearInterval(timer);
    if (heartbeat) await heartbeat;
    store.close();
    await release();
  }
}

main().catch(error => {
  console.error(JSON.stringify({ state: 'error', code: errorCode(error),
    hint: error instanceof SetupError ? error.message : 'Check configuration and environment; use login for expired or missing authorization' }));
  process.exitCode = 1;
});

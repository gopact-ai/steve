import { parseArgs } from 'node:util';
import { resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { Octokit } from '@octokit/rest';
import { loadConfig, requiredEnv } from './config.mjs';
import { Store } from './store.mjs';
import { Steve } from './steve.mjs';
import { Poller } from './poller.mjs';
import { SetupError, errorCode } from './errors.mjs';

async function main() {
  const args = parseArgs({ allowPositionals: true, options: { config: { type: 'string', default: 'config.json' }, help: { type: 'boolean' } } });
  const command = args.positionals[0] || 'run';
  if (args.values.help) { console.log('node src/cli.mjs [run|once|status] --config config.json'); return; }
  if (!['run', 'once', 'status'].includes(command) || args.positionals.length > 1) throw new SetupError('Unknown command; use --help');
  const config = loadConfig(resolve(args.values.config));
  if (command === 'status') {
    const store = new Store(config.state_dir, { readOnly: true });
    try { console.log(JSON.stringify(store.status(), null, 2)); } finally { store.close(); }
    return;
  }
  const store = new Store(config.state_dir);
  const github = new Octokit({ auth: requiredEnv(config.github_token_env), baseUrl: config.api_url, userAgent: 'steve-github-provider/0.1.0', request: { timeout: 30000 } });
  const steve = new Steve(config, store);
  const stop = new AbortController();
  let timer, heartbeat;
  try {
    await steve.register();
    const poller = new Poller(config, store, steve, github);
    process.once('SIGINT', () => stop.abort());
    process.once('SIGTERM', () => stop.abort());
    timer = setInterval(() => {
      if (heartbeat) return;
      heartbeat = steve.heartbeat().catch((error) => {
        console.error(JSON.stringify({ service: 'steve', state: 'error', code: errorCode(error) }));
        if ([401, 403, 409].includes(error.statusCode)) { process.exitCode = 1; stop.abort(); }
      }).finally(() => { heartbeat = null; });
    }, 15000);
    if (command === 'once') { process.exitCode = (await poller.cycle()) ? 0 : 1; return; }
    while (!stop.signal.aborted) {
      await poller.cycle();
      await delay(config.poll_seconds * 1000, undefined, { signal: stop.signal }).catch(() => {});
    }
  } finally {
    clearInterval(timer);
    if (heartbeat) await heartbeat;
    store.close();
  }
}

main().catch((error) => {
  console.error(JSON.stringify({ state: 'error', code: errorCode(error), hint: error instanceof SetupError ? error.message : 'Check configuration, tokens and network' }));
  process.exitCode = 1;
});

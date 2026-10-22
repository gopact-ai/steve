import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { SetupError } from './errors.mjs';

const REPOSITORY = /^[A-Za-z0-9_.-]{1,100}\/[A-Za-z0-9_.-]{1,100}$/;

export function loadConfig(path) {
  const config = JSON.parse(readFileSync(path, 'utf8'));
  if (!Array.isArray(config.repositories) || !config.repositories.length) throw new SetupError('Configure at least one repository as owner/name');
  const seen = new Set();
  for (const name of config.repositories) {
    if (typeof name !== 'string' || !REPOSITORY.test(name)) throw new SetupError('Repositories use the owner/name form');
    if (seen.has(name.toLowerCase())) throw new SetupError('Each repository is listed once');
    seen.add(name.toLowerCase());
  }
  const steve = new URL(config.steve_url);
  if (steve.protocol !== 'https:' && !(steve.protocol === 'http:' && ['127.0.0.1', 'localhost', '[::1]'].includes(steve.hostname))) {
    throw new SetupError('Use HTTPS or a local SSH tunnel for the Steve connection');
  }
  if (steve.username || steve.password || steve.search || steve.hash || steve.pathname !== '/') throw new SetupError('steve_url must be an origin');
  config.steve_url = steve.origin;
  const api = new URL(config.api_url || 'https://api.github.com');
  if (api.protocol !== 'https:' || api.username || api.password || api.search || api.hash) throw new SetupError('api_url must be an HTTPS GitHub API endpoint');
  config.api_url = api.href.replace(/\/$/, '');
  config.state_dir = resolve(dirname(path), config.state_dir || 'state');
  config.poll_seconds ??= 60;
  if (!Number.isFinite(config.poll_seconds) || config.poll_seconds < 30) throw new SetupError('poll_seconds must be at least 30');
  config.steve_token_env ||= 'STEVE_GITHUB_CONNECTION_TOKEN';
  config.github_token_env ||= 'STEVE_GITHUB_TOKEN';
  return config;
}

export function requiredEnv(name) {
  const value = process.env[name];
  if (!value) throw new SetupError(`Set ${name} in the provider environment`);
  return value;
}

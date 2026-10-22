import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Store } from '../src/store.mjs';
import { Poller } from '../src/poller.mjs';
import { Steve } from '../src/steve.mjs';
import { checksEvent, workflowEvent } from '../src/events.mjs';

const REPO = 'acme/widget';

// A GitHub stand-in with just the endpoints the poller uses.
function fakeGitHub() {
  const state = { pulls: [], reviews: {}, checks: {}, runs: [] };
  const page = (items, { per_page = 30, page = 1 } = {}) => items.slice((page - 1) * per_page, page * per_page);
  const rest = {
    pulls: {
      list: async (args) => {
        const items = state.pulls.filter((pull) => args.state === 'all' || pull.state === args.state).sort((a, b) => b.updated_at.localeCompare(a.updated_at));
        return { data: page(items, args) };
      },
      listReviews: async ({ pull_number }) => ({ data: state.reviews[pull_number] || [] }),
    },
    checks: { listForRef: async ({ ref }) => ({ data: { check_runs: state.checks[ref] || [] } }) },
    repos: { get: async () => ({ data: { default_branch: 'main' } }) },
    actions: { listWorkflowRunsForRepo: async () => ({ data: { workflow_runs: state.runs } }) },
  };
  return { state, github: { rest } };
}

const iso = (offsetMinutes) => new Date(Date.now() + offsetMinutes * 60000).toISOString().replace(/\.\d+Z$/, 'Z');
const pull = (number, fields = {}) => ({ number, state: 'open', title: `Change ${number}`, html_url: `https://github.com/${REPO}/pull/${number}`,
  user: { login: 'dev' }, head: { sha: `sha-${number}-a` }, base: { ref: 'main' }, draft: false, merged_at: null,
  created_at: iso(-600), updated_at: iso(-600), ...fields });

function fixture(t) {
  const dir = mkdtempSync(join(tmpdir(), 'steve-github-'));
  const store = new Store(dir);
  t.after(() => { store.close(); rmSync(dir, { recursive: true, force: true }); });
  const published = [];
  const steve = new Steve({ steve_url: 'http://localhost', steve_token_env: 'STEVE_GITHUB_TEST_TOKEN' }, store, async (url, options) => {
    const body = JSON.parse(options.body);
    published.push(body);
    return { ok: true, json: async () => ({ id: `event-${published.length}` }) };
  });
  process.env.STEVE_GITHUB_TEST_TOKEN = 'connection-fixture';
  t.after(() => delete process.env.STEVE_GITHUB_TEST_TOKEN);
  const { state, github } = fakeGitHub();
  const reports = [];
  const poller = new Poller({ repositories: [REPO] }, store, steve, github, (row) => reports.push(row));
  return { dir, store, published, state, poller, reports };
}

test('connecting a repository records a baseline without replaying its history', async (t) => {
  const { published, state, poller, reports } = fixture(t);
  state.pulls.push(pull(1), pull(2, { state: 'closed', merged_at: iso(-500) }));
  state.checks['sha-1-a'] = [{ name: 'test', status: 'completed', conclusion: 'success' }];
  state.runs.push({ id: 7, name: 'test', status: 'completed', conclusion: 'failure', head_branch: 'main', updated_at: iso(-30) });
  assert.equal(await poller.cycle(), true);
  assert.deepEqual(published, []);
  assert.equal(reports.at(-1).state, 'baseline');
  assert.equal(await poller.cycle(), true);
  assert.deepEqual(published, [], 'existing results were published after the baseline');
});

test('new pull requests, pushes, reviews, checks and default-branch failures are published once', async (t) => {
  const { published, state, poller } = fixture(t);
  state.pulls.push(pull(1));
  await poller.cycle();
  state.pulls.push(pull(3, { created_at: iso(1), updated_at: iso(1) }));
  Object.assign(state.pulls[0], { head: { sha: 'sha-1-b' }, updated_at: iso(1) });
  state.reviews[1] = [{ id: 11, state: 'CHANGES_REQUESTED', body: 'Please add a test', user: { login: 'reviewer' }, submitted_at: iso(1) }];
  state.checks['sha-1-b'] = [{ name: 'test', status: 'completed', conclusion: 'failure', html_url: 'u' }, { name: 'lint', status: 'completed', conclusion: 'success' }];
  state.checks['sha-3-a'] = [{ name: 'test', status: 'in_progress', conclusion: null }];
  state.runs.push({ id: 8, name: 'release', status: 'completed', conclusion: 'failure', head_branch: 'main', updated_at: iso(1), html_url: 'r' });
  await poller.cycle();
  const types = published.map((item) => `${item.type} ${item.subject}`).sort();
  assert.deepEqual(types, [
    `github.checks.failed ${REPO}#1`,
    `github.pull_request.opened ${REPO}#3`,
    `github.pull_request.updated ${REPO}#1`,
    `github.review.submitted ${REPO}#1`,
    `github.workflow.failed ${REPO} release`,
  ]);
  assert.equal(published.find((item) => item.type === 'github.review.submitted').payload.body, 'Please add a test');
  const count = published.length;
  await poller.cycle();
  assert.equal(published.length, count, 'a repeated poll published duplicates');
  // Checks finishing later are still reported for that head.
  state.checks['sha-3-a'] = [{ name: 'test', status: 'completed', conclusion: 'success' }];
  await poller.cycle();
  assert.equal(published.at(-1).type, 'github.checks.succeeded');
  assert.equal(published.at(-1).key, `github/${REPO}/checks/sha-3-a`);
  // Merge.
  Object.assign(state.pulls.find((item) => item.number === 3), { state: 'closed', merged_at: iso(2), updated_at: iso(2) });
  await poller.cycle();
  assert.equal(published.at(-1).type, 'github.pull_request.closed');
  assert.equal(published.at(-1).payload.merged, true);
});

test('an older pull request first seen after the baseline is recorded, not announced', async (t) => {
  const { published, state, poller } = fixture(t);
  state.pulls.push(pull(1));
  await poller.cycle();
  // Updated now but created and closed long ago, and beyond the baseline page.
  state.pulls.push(pull(9, { updated_at: iso(1) }), pull(10, { state: 'closed', closed_at: iso(-300), merged_at: iso(-300), updated_at: iso(1) }));
  assert.equal(await poller.cycle(), true);
  assert.deepEqual(published.map((item) => item.type), []);
  Object.assign(state.pulls.find((item) => item.number === 9), { head: { sha: 'sha-9-b' }, updated_at: iso(2) });
  await poller.cycle();
  assert.deepEqual(published.map((item) => item.type), ['github.pull_request.updated']);
});

test('a GitHub failure leaves the cursor for the next poll', async (t) => {
  const { store, published, state, poller, reports } = fixture(t);
  state.pulls.push(pull(1));
  await poller.cycle();
  const before = store.since(REPO);
  state.pulls.push(pull(4, { created_at: iso(1), updated_at: iso(1) }));
  const list = poller.github.rest.pulls.listReviews;
  poller.github.rest.pulls.listReviews = async () => { const error = new Error('rate limited'); error.status = 403; throw error; };
  assert.equal(await poller.cycle(), false);
  assert.equal(reports.at(-1).code, '403');
  assert.equal(store.since(REPO), before);
  assert.deepEqual(published, []);
  poller.github.rest.pulls.listReviews = list;
  await poller.cycle();
  assert.deepEqual(published.map((item) => item.type), ['github.pull_request.opened']);
});

test('checks are reported only after every run completes', () => {
  const head = pull(5);
  assert.equal(checksEvent(REPO, head, []), null);
  assert.equal(checksEvent(REPO, head, [{ status: 'queued' }, { status: 'completed', conclusion: 'success' }]), null);
  assert.equal(checksEvent(REPO, head, [{ status: 'completed', conclusion: 'skipped' }]).type, 'github.checks.succeeded');
  assert.equal(checksEvent(REPO, head, [{ status: 'completed', conclusion: 'cancelled' }]).type, 'github.checks.failed');
  assert.equal(workflowEvent(REPO, { status: 'completed', conclusion: 'success' }), null);
});

test('a second process cannot open the same state', (t) => {
  const { dir } = fixture(t);
  assert.throws(() => new Store(dir), /owns this state directory/);
});

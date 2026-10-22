import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Store, lockState } from '../src/store.mjs';
import { Poller } from '../src/poller.mjs';
import { Steve } from '../src/steve.mjs';
import { checksEvent, workflowEvent } from '../src/events.mjs';

const REPO = 'acme/widget';

// A GitHub stand-in with the endpoints the poller uses. paginate follows
// pages of `size` items and honors the early-stop callback like Octokit's.
function fakeGitHub(size = 2) {
  const state = { pulls: [], reviews: {}, checks: {}, runs: [], calls: 0 };
  const list = (items, page, per) => items.slice((page - 1) * per, page * per);
  const rest = {
    pulls: {
      list: (args) => state.pulls.filter((pull) => args.state === 'all' || pull.state === args.state).sort((a, b) => b.updated_at.localeCompare(a.updated_at)),
      listReviews: ({ pull_number }) => state.reviews[pull_number] || [],
    },
    checks: { listForRef: ({ ref }) => state.checks[ref] || [] },
    repos: { get: async () => ({ data: { default_branch: 'main' } }) },
    actions: { listWorkflowRunsForRepo: ({ created }) => state.runs.filter((run) => run.created_at >= created.slice(2)) },
  };
  const paginate = async (method, args, map = (response) => response.data) => {
    const all = method(args), out = [];
    let stopped = false;
    for (let page = 1; !stopped; page++) {
      state.calls++;
      const data = list(all, page, size);
      if (!data.length) break;
      out.push(...map({ data }, () => { stopped = true; }));
      if (data.length < size) break;
    }
    return out;
  };
  return { state, github: { rest, paginate } };
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
  state.reviews[1] = [{ id: 5, state: 'COMMENTED', body: 'just before connecting', submitted_at: iso(-1) }];
  state.runs.push({ id: 7, name: 'test', status: 'completed', conclusion: 'failure', head_branch: 'main', created_at: iso(-30), updated_at: iso(-30) });
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
  state.runs.push({ id: 8, name: 'release', status: 'completed', conclusion: 'failure', head_branch: 'main', created_at: iso(1), updated_at: iso(1), html_url: 'r' });
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
  poller.github.rest.pulls.listReviews = () => { const error = new Error('rate limited'); error.status = 403; throw error; };
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

test('a second process cannot poll the same state, but status can read it', async (t) => {
  const { dir } = fixture(t);
  const release = await lockState(dir);
  try {
    await assert.rejects(lockState(dir), /owns this state directory/);
    const reader = new Store(dir, { readOnly: true });
    assert.deepEqual(reader.status(), []);
    reader.close();
  } finally { await release(); }
});

test('catch-up reads every page since the cursor', async (t) => {
  const { published, state, poller } = fixture(t);
  state.pulls.push(pull(1));
  await poller.cycle();
  for (let n = 100; n < 113; n++) state.pulls.push(pull(n, { created_at: iso(1), updated_at: iso(1 + n / 1000) }));
  await poller.cycle();
  assert.equal(published.filter((item) => item.type === 'github.pull_request.opened').length, 13);
});

test('every open head is seeded at baseline, so a first push on an old pull request is announced', async (t) => {
  const { published, state, poller } = fixture(t);
  for (let n = 1; n <= 7; n++) state.pulls.push(pull(n));
  await poller.cycle();
  Object.assign(state.pulls.find((item) => item.number === 1), { head: { sha: 'sha-1-b' }, updated_at: iso(1) });
  await poller.cycle();
  assert.deepEqual(published.map((item) => `${item.type} ${item.payload.number}`), ['github.pull_request.updated 1']);
});

test('a review submitted just before connecting is not published by later polls', async (t) => {
  const { published, state, poller } = fixture(t);
  state.pulls.push(pull(1, { updated_at: iso(-1) }));
  state.reviews[1] = [{ id: 21, state: 'APPROVED', submitted_at: iso(-1) }];
  await poller.cycle();
  await poller.cycle();
  await poller.cycle();
  assert.deepEqual(published, []);
});

test('reviews and check runs on later pages count', async (t) => {
  const { published, state, poller } = fixture(t);
  state.pulls.push(pull(1));
  state.reviews[1] = [1, 2, 3].map((id) => ({ id, state: 'COMMENTED', submitted_at: iso(-200) }));
  await poller.cycle();
  state.reviews[1].push({ id: 4, state: 'COMMENTED', body: 'page two', submitted_at: iso(1) });
  state.pulls[0].updated_at = iso(1);
  // Two passing runs on page one, a running one on page two: not finished yet.
  state.checks['sha-1-a'] = [{ name: 'a', status: 'completed', conclusion: 'success' }, { name: 'b', status: 'completed', conclusion: 'success' }, { name: 'c', status: 'in_progress' }];
  await poller.cycle();
  assert.deepEqual(published.map((item) => item.type), ['github.review.submitted']);
  state.checks['sha-1-a'][2] = { name: 'c', status: 'completed', conclusion: 'failure' };
  await poller.cycle();
  assert.equal(published.at(-1).type, 'github.checks.failed');
});

test('a long workflow that finishes late and a rerun are both reported', async (t) => {
  const { published, state, poller } = fixture(t);
  state.pulls.push(pull(1));
  await poller.cycle();
  // Created three hours ago, finished now; then rerun and failed again.
  state.runs.push({ id: 30, run_attempt: 1, name: 'nightly', status: 'completed', conclusion: 'failure', head_branch: 'main', created_at: iso(-180), updated_at: iso(1) });
  await poller.cycle();
  Object.assign(state.runs[0], { run_attempt: 2, updated_at: iso(5) });
  await poller.cycle();
  assert.deepEqual(published.map((item) => item.key), [`github/${REPO}/run/30/1`, `github/${REPO}/run/30/2`]);
});

import { pullEvents, reviewEvent, checksEvent, workflowEvent } from './events.mjs';
import { errorCode } from './errors.mjs';

const PAGE = 100, SKEW = 120000, RUN_WINDOW = 24 * 3600 * 1000;
const stamp = (ms) => new Date(ms).toISOString().replace(/\.\d+Z$/, 'Z');

// Polls each repository through the GitHub REST API.
//
// The first poll is a baseline: it records every open pull request's head and
// marks existing reviews, check results and workflow failures as delivered, so
// connecting a repository does not replay history. Later polls read everything
// updated since the committed cursor, however many pages that is. A failure
// leaves the cursor unchanged; the next poll repeats the window and outbox keys
// absorb the overlap.
export class Poller {
  constructor(config, store, steve, github, report = (row) => console.log(JSON.stringify(row))) {
    Object.assign(this, { config, store, steve, github, report });
  }
  async cycle() {
    let ok = true;
    for (const name of this.config.repositories) ok = (await this.repository(name)) && ok;
    return ok;
  }
  // Octokit's paginate follows every page and unwraps list responses.
  all(method, args) { return this.github.paginate(method, { ...args, per_page: PAGE }); }
  async repository(name) {
    const [owner, repo] = name.split('/');
    try {
      let published = await this.steve.drain();
      const now = Date.now(), started = stamp(now - SKEW), since = this.store.since(name), baseline = !since;
      const pulls = new Map(), envelopes = new Map();
      const add = (envelope) => {
        if (!envelope || this.store.seen(envelope.key) || envelopes.has(envelope.key)) return;
        envelopes.set(envelope.key, baseline ? { ...envelope, baseline: true } : envelope);
      };
      const record = (pull) => pulls.set(pull.number, { number: pull.number, head: pull.head.sha, state: pull.state });
      const reviews = async (pull) => {
        for (const review of await this.all(this.github.rest.pulls.listReviews, { owner, repo, pull_number: pull.number })) {
          if (review.state !== 'PENDING') add(reviewEvent(name, pull, review));
        }
      };
      // Every open pull request: heads for push detection and pending checks.
      // Check completion does not touch a pull request's updated_at.
      const open = await this.all(this.github.rest.pulls.list, { owner, repo, state: 'open', sort: 'updated', direction: 'desc' });
      // Everything updated since the cursor, newest first, until crossing it.
      // The baseline reads its own overlap window and records it as delivered.
      const window = since || started;
      const changed = await this.github.paginate(this.github.rest.pulls.list,
        { owner, repo, state: 'all', sort: 'updated', direction: 'desc', per_page: PAGE },
        (response, done) => {
          const fresh = response.data.filter((pull) => pull.updated_at >= window);
          if (fresh.length < response.data.length) done();
          return fresh;
        });
      for (const pull of changed) {
        if (!baseline) for (const envelope of pullEvents(name, pull, this.store.pull(name, pull.number), since)) add(envelope);
        record(pull);
        await reviews(pull);
      }
      if (baseline) for (const pull of open) if (!pulls.has(pull.number)) { record(pull); await reviews(pull); }
      for (const pull of open) {
        if (this.store.seen(`github/${name}/checks/${pull.head.sha}`)) continue;
        const runs = await this.all(this.github.rest.checks.listForRef, { owner, repo, ref: pull.head.sha });
        add(checksEvent(name, pull, runs));
      }
      // Default-branch failures. Runs are listed by creation time, so look a
      // day further back than the cursor to catch long runs that finished late.
      const { data: repository } = await this.github.rest.repos.get({ owner, repo });
      const from = stamp((baseline ? now : Date.parse(since)) - RUN_WINDOW);
      const runs = await this.all(this.github.rest.actions.listWorkflowRunsForRepo,
        { owner, repo, branch: repository.default_branch, status: 'completed', created: `>=${from}` });
      for (const run of runs) add(workflowEvent(name, run));
      this.store.stage(name, started, [...pulls.values()], [...envelopes.values()]);
      published += await this.steve.drain();
      this.report({ repository: name, state: baseline ? 'baseline' : 'ready', published, at: started });
      return true;
    } catch (error) {
      this.report({ repository: name, state: 'error', code: errorCode(error), at: new Date().toISOString() });
      return false;
    }
  }
}

import { pullEvents, reviewEvent, checksEvent, workflowEvent } from './events.mjs';
import { errorCode } from './errors.mjs';

const PAGE = 50, PAGES = 5, SKEW = 120000;

// Polls each repository through the GitHub REST API. The first poll records a
// baseline without publishing, so connecting a repository does not replay its
// history. A failure leaves the cursor unchanged; the next poll repeats it and
// the outbox keys absorb the overlap.
export class Poller {
  constructor(config, store, steve, github, report = (row) => console.log(JSON.stringify(row))) {
    Object.assign(this, { config, store, steve, github, report });
  }
  async cycle() {
    let ok = true;
    for (const name of this.config.repositories) ok = (await this.repository(name)) && ok;
    return ok;
  }
  async repository(name) {
    const [owner, repo] = name.split('/');
    try {
      let published = await this.steve.drain();
      const started = new Date(Date.now() - SKEW).toISOString().replace(/\.\d+Z$/, 'Z'), since = this.store.since(name), baseline = !since;
      const pulls = [], envelopes = [];
      const seen = (envelope) => envelope && !this.store.seen(envelope.key) && !envelopes.some((item) => item.key === envelope.key);
      // The baseline records existing results as already delivered.
      const add = (envelope) => { if (seen(envelope)) envelopes.push(baseline ? { ...envelope, baseline: true } : envelope); };
      // Recently updated pull requests, newest first, until the cursor.
      const recent = [];
      for (let page = 1; page <= PAGES; page++) {
        const { data } = await this.github.rest.pulls.list({ owner, repo, state: 'all', sort: 'updated', direction: 'desc', per_page: PAGE, page });
        recent.push(...data);
        if (data.length < PAGE || baseline || data.at(-1).updated_at < since) break;
      }
      const changed = recent.filter((pull) => !since || pull.updated_at >= since);
      for (const pull of changed) {
        const previous = this.store.pull(name, pull.number);
        if (!baseline) for (const envelope of pullEvents(name, pull, previous, since)) add(envelope);
        pulls.push({ number: pull.number, head: pull.head.sha, state: pull.state });
        if (!baseline) {
          const { data: reviews } = await this.github.rest.pulls.listReviews({ owner, repo, pull_number: pull.number, per_page: 100 });
          for (const review of reviews) if (review.state !== 'PENDING' && (!review.submitted_at || review.submitted_at >= since)) add(reviewEvent(name, pull, review));
        }
      }
      // Check completion does not touch a pull request's updated_at, so every
      // open head is checked until its result has been published.
      const { data: open } = await this.github.rest.pulls.list({ owner, repo, state: 'open', sort: 'updated', direction: 'desc', per_page: 100 });
      for (const pull of open) {
        if (this.store.seen(`github/${name}/checks/${pull.head.sha}`)) continue;
        const { data } = await this.github.rest.checks.listForRef({ owner, repo, ref: pull.head.sha, per_page: 100 });
        add(checksEvent(name, pull, data.check_runs));
      }
      const { data: repository } = await this.github.rest.repos.get({ owner, repo });
      const { data: runs } = await this.github.rest.actions.listWorkflowRunsForRepo({ owner, repo, branch: repository.default_branch, status: 'completed', per_page: 20 });
      for (const run of runs.workflow_runs) if (!since || run.updated_at >= since) add(workflowEvent(name, run));
      this.store.stage(name, started, pulls, envelopes);
      published += await this.steve.drain();
      this.report({ repository: name, state: baseline ? 'baseline' : 'ready', published, at: started });
      return true;
    } catch (error) {
      this.report({ repository: name, state: 'error', code: errorCode(error), at: new Date().toISOString() });
      return false;
    }
  }
}

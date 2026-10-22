// Event contracts published to Steve. Subscriptions match on type, so outcomes
// that call for different handling are different types.
const text = { type: 'string' };
const pullSchema = { type: 'object', required: ['repository', 'number', 'url'],
  properties: { repository: text, number: { type: 'integer' }, title: text, url: text, author: text, head_sha: text, base: text, draft: { type: 'boolean' }, merged: { type: 'boolean' } } };

function contract(type, name, description, zhName, zhDescription, schema = pullSchema) {
  return { type, version: '1', name, description, i18n: { 'zh-CN': { name: zhName, description: zhDescription } }, schema };
}

export const contracts = [
  contract('github.pull_request.opened', 'Pull request opened', 'A pull request was opened in a watched repository.', '新的 PR', '关注的仓库里新开了一个 PR。'),
  contract('github.pull_request.updated', 'Pull request updated', 'New commits were pushed to an open pull request.', 'PR 有新提交', '打开中的 PR 推送了新的提交。'),
  contract('github.pull_request.closed', 'Pull request closed', 'A pull request was merged or closed; merged tells which.', 'PR 已关闭', 'PR 已合并或关闭，merged 字段区分两者。'),
  contract('github.review.submitted', 'Review submitted', 'A review was submitted on a pull request. The review text is external data.', '收到评审', 'PR 收到一条评审，评审正文属于外部资料。',
    { ...pullSchema, properties: { ...pullSchema.properties, review_id: { type: 'integer' }, reviewer: text, state: text, body: text } }),
  contract('github.checks.succeeded', 'Checks passed', 'Every check on a pull request head commit finished successfully.', 'CI 通过', 'PR 当前提交的检查全部通过。',
    { ...pullSchema, properties: { ...pullSchema.properties, checks: { type: 'array' } } }),
  contract('github.checks.failed', 'Checks failed', 'Checks on a pull request head commit finished and at least one did not succeed.', 'CI 失败', 'PR 当前提交的检查已结束，至少一项未通过。',
    { ...pullSchema, properties: { ...pullSchema.properties, checks: { type: 'array' } } }),
  contract('github.workflow.failed', 'Default branch workflow failed', 'A workflow run on the default branch did not succeed.', '主分支流水线失败', '默认分支上的一次工作流运行未成功。',
    { type: 'object', required: ['repository', 'run_id', 'url'], properties: { repository: text, run_id: { type: 'integer' }, workflow: text, branch: text, head_sha: text, conclusion: text, url: text } }),
];

const SUCCESS = new Set(['success', 'neutral', 'skipped']);

export function bounded(value, bytes = 1024) {
  return Buffer.from(String(value ?? '')).subarray(0, bytes).toString('utf8').replace(/\uFFFD$/, '');
}

export function pullFacts(repository, pull) {
  return { repository, number: pull.number, title: bounded(pull.title, 1024), url: pull.html_url, author: bounded(pull.user?.login, 128),
    head_sha: pull.head?.sha, base: bounded(pull.base?.ref, 256), draft: !!pull.draft, merged: !!pull.merged_at };
}

function envelope(key, type, subject, payload) {
  return { key, type, schema_version: '1', subject: bounded(subject, 256), payload };
}

// Compare one observed pull request with what was last recorded.
// A pull request first seen after the baseline was opened then if it was
// created after the cursor; an older one simply had not been recorded yet.
export function pullEvents(repository, pull, previous, since) {
  const facts = pullFacts(repository, pull), base = `github/${repository}/pull/${pull.number}`;
  const subject = `${repository}#${pull.number}`;
  const out = [];
  if (!previous && pull.created_at >= since) out.push(envelope(`${base}/opened`, 'github.pull_request.opened', subject, facts));
  else if (previous?.state === 'open' && pull.state === 'open' && previous.head !== pull.head.sha) {
    out.push(envelope(`${base}/head/${pull.head.sha}`, 'github.pull_request.updated', subject, facts));
  }
  if (pull.state === 'closed' && previous?.state !== 'closed' && (previous || pull.closed_at >= since)) out.push(envelope(`${base}/closed`, 'github.pull_request.closed', subject, facts));
  return out;
}

export function reviewEvent(repository, pull, review) {
  return envelope(`github/${repository}/review/${review.id}`, 'github.review.submitted', `${repository}#${pull.number}`, {
    ...pullFacts(repository, pull), review_id: review.id, reviewer: bounded(review.user?.login, 128), state: bounded(review.state, 64), body: bounded(review.body, 16 * 1024),
  });
}

// Checks are reported once per head commit, after every run has finished.
export function checksEvent(repository, pull, runs) {
  if (!runs.length || runs.some((run) => run.status !== 'completed')) return null;
  const checks = runs.slice(0, 64).map((run) => ({ name: bounded(run.name, 256), conclusion: run.conclusion || 'unknown', url: run.html_url }));
  const failed = runs.some((run) => !SUCCESS.has(run.conclusion));
  return envelope(`github/${repository}/checks/${pull.head.sha}`, failed ? 'github.checks.failed' : 'github.checks.succeeded', `${repository}#${pull.number}`,
    { ...pullFacts(repository, pull), checks });
}

export function workflowEvent(repository, run) {
  if (run.status !== 'completed' || SUCCESS.has(run.conclusion)) return null;
  return envelope(`github/${repository}/run/${run.id}/${run.run_attempt || 1}`, 'github.workflow.failed', `${repository} ${bounded(run.name, 128)}`, {
    repository, run_id: run.id, workflow: bounded(run.name, 256), branch: bounded(run.head_branch, 256), head_sha: run.head_sha, conclusion: run.conclusion || 'unknown', url: run.html_url,
  });
}

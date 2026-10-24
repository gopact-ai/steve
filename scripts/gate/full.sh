#!/bin/bash
# The required CI jobs of .github/workflows/test.yml, run locally in order:
# test everywhere, macos on macOS; the console job returns with the v1 Web (P2)
# with Docker. Keep the steps in step with that workflow. Every job runs;
# the exit status is non-zero if any failed. Set GATE_JOBS="test console"
# to pick jobs.
set -uo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"
jobs="${GATE_JOBS:-test macos}"
failed=()
skipped=()

step() { # name, command...
  local name="$1"; shift
  local start=$SECONDS
  echo "== $name"
  if "$@"; then
    echo "-- $name PASS ($((SECONDS - start))s)"
  else
    echo "-- $name FAIL ($((SECONDS - start))s)"
    failed+=("$name")
  fi
}

gofmt_check() { test -z "$(git ls-files -z '*.go' | xargs -0 gofmt -l)"; }
dist_clean() { test -z "$(git status --porcelain -- internal/readmodel/web/dist)"; }

job_test() {
  step "test: gofmt" gofmt_check
  step "test: go vet" go vet ./cmd/... ./internal/... ./scripts/...
  step "test: make test" make test
}

job_console() {
  step "console: npm ci" npm --prefix web/console ci
  step "console: playwright chromium" npx --prefix web/console playwright install chromium
  step "console: make test-console" make test-console
  step "console: embedded console matches source" dist_clean
}

job_macos() {
  step "macos: agent process groups" go test -race -count=1 ./internal/procgroup/
  step "macos: stop capability" go test -race -count=1 ./internal/peerstop/
  step "macos: MCP broker socket fixtures" go test -race -count=1 -run '^(TestARefusedLaunch|TestLaunchingAReleased|TestAStoppedBroker|TestMCPBindingKeepsSecrets|TestExternalBrokerHolds|TestWorkspaceChangeApplies|TestBroker)' ./internal/node/
  step "macos: agent process group stops" go test -race -count=1 -run '^(TestKillProcessGroupReapsDescendants|TestHostConfirmsStopOnlyAfterTheAgentsGroupIsGone|TestHostDoesNotConfirmAStopWhileAHiddenMemberRuns|TestHostRestartsWhileAnExitedAgentsGroupCannotBeEnded|TestHostCloseKillsAnAgentWhoseExitCannotBeWatched|TestLocalTransportDoesNotPassOnAnInheritedMark|TestHostStopWaitsOnlyBrieflyForAnExitedAgentsGroup|TestHostEndsAnExitedAgentsGroupWhileAMemberHoldsItsOutput|TestHostEndsAnAgentWhoseProcessGroupCannotBeIdentified|TestLocalProcessReapsAnAgentWhoseExitCannotBeWatchedOnlyBetweenKills|TestLocalProcessTellsTheExitCodeOfAnAgentWhoseExitCannotBeWatched)$' ./internal/acphost/
  step "macos: owned ACP terminals and recovery" env TMPDIR=/tmp go test -race -count=1 -run '^(TestTerminal|TestHostTerminal|TestStandaloneTerminal|TestWarmRuntime)' ./internal/acphost/ ./internal/node/
}

os="$(uname -s)"
started=$SECONDS
for job in $jobs; do
  case "$job" in
    macos) [[ "$os" == Darwin ]] || { skipped+=("$job (needs macOS)"); continue; } ;;
    test|console) ;;
    *) echo "unknown job $job"; exit 2 ;;
  esac
  "job_$job"
done

echo
echo "gate-full: $((SECONDS - started))s"
for s in "${skipped[@]+"${skipped[@]}"}"; do echo "not run here: $s; CI runs it"; done
if ((${#failed[@]})); then
  printf 'FAIL %s\n' "${failed[@]}"
  exit 1
fi
echo PASS

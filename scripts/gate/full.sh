#!/bin/bash
# The required CI jobs of .github/workflows/test.yml, run locally in order:
# test and console everywhere, desktop and macos on macOS, fleet on Linux
# with Docker. Keep the steps in step with that workflow. Every job runs;
# the exit status is non-zero if any failed. Set GATE_JOBS="test console"
# to pick jobs.
set -uo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"
jobs="${GATE_JOBS:-test console desktop macos fleet}"
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
  step "test: go vet" go vet ./cmd/... ./internal/... ./e2e/...
  step "test: raft local patch regressions" ./scripts/test-raft.sh -race -run '^(TestRaft_AppendEntriesSnapshotBoundary|TestCommitment_|TestNetworkTransport_AppendEntriesScaledTimeout|TestRaft_AppendEntriesScaledTimeout)' -count=1
  step "test: make test" make test
}

job_console() {
  step "console: npm ci" npm --prefix web/console ci
  step "console: playwright chromium" npx --prefix web/console playwright install chromium
  step "console: make test-console" make test-console
  step "console: embedded console matches source" dist_clean
}

job_desktop() {
  step "desktop: make test-desktop" make test-desktop
  step "desktop: package app icon" bash scripts/build-app-icon.sh desktop/macos/Assets/AppIcon.png "${TMPDIR:-/tmp}/AppIcon.icns"
}

job_macos() {
  step "macos: agent process groups" go test -race -count=1 ./internal/procgroup/
  step "macos: peer discovery and stop capability" go test -race -count=1 ./internal/nodebootstrap/ ./internal/peerstop/
  step "macos: MCP broker socket fixtures" go test -race -count=1 -run '^(TestARefusedLaunch|TestLaunchingAReleased|TestAStoppedBroker|TestMCPBindingKeepsSecrets|TestExternalBrokerHolds|TestWorkspaceChangeApplies|TestBroker)' ./internal/node/
  step "macos: agent process group stops" go test -race -count=1 -run '^(TestKillProcessGroupReapsDescendants|TestHostConfirmsStopOnlyAfterTheAgentsGroupIsGone|TestHostDoesNotConfirmAStopWhileAHiddenMemberRuns|TestHostRestartsWhileAnExitedAgentsGroupCannotBeEnded|TestHostCloseKillsAnAgentWhoseExitCannotBeWatched|TestLocalTransportDoesNotPassOnAnInheritedMark|TestHostStopWaitsOnlyBrieflyForAnExitedAgentsGroup|TestHostEndsAnExitedAgentsGroupWhileAMemberHoldsItsOutput|TestHostEndsAnAgentWhoseProcessGroupCannotBeIdentified|TestLocalProcessReapsAnAgentWhoseExitCannotBeWatchedOnlyBetweenKills|TestLocalProcessTellsTheExitCodeOfAnAgentWhoseExitCannotBeWatched)$' ./internal/acphost/
  step "macos: owned ACP terminals and recovery" env TMPDIR=/tmp go test -race -count=1 -run '^(TestTerminal|TestHostTerminal|TestStandaloneTerminal|TestWarmRuntime)' ./internal/acphost/ ./internal/node/
}

job_fleet() {
  step "fleet: disposable hub gates" env STEVE_HUBLAB_E2E=1 TMPDIR=/tmp go test -race -count=1 -timeout 12m ./e2e/fleetlab/...
  step "fleet: disposable fleet acceptance" make e2e-fleet
}

os="$(uname -s)"
started=$SECONDS
for job in $jobs; do
  case "$job" in
    desktop|macos) [[ "$os" == Darwin ]] || { skipped+=("$job (needs macOS)"); continue; } ;;
    fleet) { [[ "$os" == Linux ]] && docker info >/dev/null 2>&1; } || { skipped+=("$job (needs Linux and Docker)"); continue; } ;;
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

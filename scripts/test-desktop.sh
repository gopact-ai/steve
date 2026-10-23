#!/bin/bash
set -euo pipefail

if [[ "$(uname -s)" != Darwin ]]; then
  echo "Desktop navigation tests require macOS."
  exit 1
fi

task_root="$(cd "$(dirname "$0")/.." && pwd)"
task_build="$(mktemp -d "${TMPDIR:-/tmp}/steve-desktop-test.XXXXXX")"
trap 'rm -rf "$task_build"' EXIT
xcrun clang -fobjc-arc -fmodules -Wall -Wextra -Wno-unused-parameter \
  -mmacosx-version-min=13.0 -framework Cocoa -framework WebKit \
  "$task_root/desktop/macos/main_test.m" -o "$task_build/navigation-test"
"$task_build/navigation-test"
xcrun clang -fobjc-arc -fmodules -Wall -Wextra -Wno-unused-parameter \
  -mmacosx-version-min=13.0 -framework Cocoa -framework WebKit \
  "$task_root/desktop/macos/preview_test.m" -o "$task_build/preview-test"
python3 "$task_root/desktop/macos/preview_test.py" "$task_build/preview-test"

# Release version stamping: Info.plist carries the release, and the app's
# binaries are built with exactly the -ldflags GoReleaser uses, so both
# print the same `steve version`.
source "$task_root/scripts/desktop-version.sh"
task_fail() { echo "FAIL: $*" >&2; exit 1; }
for task_case in "v0.2.0 0.2.0 0.2.0" "0.2.0 0.2.0 0.2.0" "v0.3.0-rc.1 0.3.0-rc.1 0.3.0"; do
  read -r task_tag task_want task_numeric <<<"$task_case"
  task_got="$(STEVE_VERSION="$task_tag" desktop_version "$task_root")"
  [[ "$task_got" == "$task_want" ]] || task_fail "STEVE_VERSION=$task_tag gave $task_got, want $task_want"
  cp "$task_root/desktop/macos/Info.plist" "$task_build/Info.plist"
  stamp_info_plist "$task_build/Info.plist" "$task_got"
  plutil -lint "$task_build/Info.plist" >/dev/null
  for task_key in CFBundleShortVersionString CFBundleVersion; do
    task_value="$(plutil -extract "$task_key" raw "$task_build/Info.plist")"
    [[ "$task_value" == "$task_numeric" ]] || task_fail "$task_key=$task_value for $task_tag, want $task_numeric"
  done
  [[ "$(plutil -extract SteveVersion raw "$task_build/Info.plist")" == "$task_want" ]] || task_fail "SteveVersion for $task_tag"
done
for task_bad in "release-1" "v1" "v1.2" "1.2.3 4"; do
  if STEVE_VERSION="$task_bad" desktop_version "$task_root" 2>/dev/null; then task_fail "accepted STEVE_VERSION=$task_bad"; fi
done
task_local="$(unset STEVE_VERSION; desktop_version "$task_root")"
[[ "$task_local" =~ ^[0-9]+\.[0-9]+\.[0-9]+ ]] || task_fail "local version $task_local"
[[ "$(STEVE_VERSION= desktop_steve_ldflags "$task_local")" != *ReleaseVersion* ]] || task_fail "a source build injects a release version"
echo "PASS Info.plist version stamping (release tags, rc, invalid tags, local $task_local)"

task_goreleaser="$(awk '/^  - id: steve$/{s=1;next} s&&/^  - id:/{exit} s&&/ldflags:/{l=1;next} l&&/^ *- /{sub(/^ *- /,"");print;exit}' "$task_root/.goreleaser.yaml")"
[[ "$task_goreleaser" == *'{{ .Version }}'* ]] || task_fail "could not read cmd/steve ldflags from .goreleaser.yaml: $task_goreleaser"
task_goreleaser="${task_goreleaser//\{\{ .Version \}\}/0.2.0}"
task_desktop="$(STEVE_VERSION=v0.2.0 desktop_steve_ldflags 0.2.0)"
[[ "$task_desktop" == "$task_goreleaser" ]] || task_fail "desktop ldflags '$task_desktop' differ from GoReleaser '$task_goreleaser'"
task_node_goreleaser="$(awk '/^  - id: steve-node$/{s=1;next} s&&/^  - id:/{exit} s&&/ldflags:/{sub(/.*ldflags: *\[/,"");sub(/\].*/,"");gsub(/, */," ");print;exit}' "$task_root/.goreleaser.yaml")"
[[ "$(desktop_node_ldflags)" == "$task_node_goreleaser" ]] || task_fail "steve-node ldflags '$(desktop_node_ldflags)' differ from GoReleaser '$task_node_goreleaser'"
echo "PASS desktop -ldflags match .goreleaser.yaml for steve and steve-node"
# Comparing the printed versions needs Go. CI must run it (the desktop job
# sets Go up); only a local machine without Go may skip, and says so.
if command -v "${GO:-go}" >/dev/null 2>&1; then
  (cd "$task_root" && CGO_ENABLED=0 "${GO:-go}" build -trimpath -ldflags="$task_goreleaser" -o "$task_build/steve-goreleaser" ./cmd/steve)
  (cd "$task_root" && CGO_ENABLED=0 "${GO:-go}" build -trimpath -ldflags="$task_desktop" -o "$task_build/steve-desktop" ./cmd/steve)
  task_a="$("$task_build/steve-goreleaser" version)"
  task_b="$("$task_build/steve-desktop" version)"
  [[ "$task_a" == "$task_b" ]] || task_fail "steve version differs: GoReleaser '$task_a', desktop '$task_b'"
  [[ "$task_b" == "steve 0.2.0 "* ]] || task_fail "release binary prints '$task_b', want 'steve 0.2.0 ...'"
  echo "PASS desktop and GoReleaser binaries print the same version: $task_b"
elif [[ "${CI:-}" == true ]]; then
  task_fail "Go is required in CI to compare the desktop and GoReleaser 'steve version' output"
else
  echo "SKIP (no go) desktop and GoReleaser 'steve version' comparison"
fi

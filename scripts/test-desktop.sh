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

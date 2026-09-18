#!/bin/bash
set -euo pipefail

if [[ "$(uname -s)" != Darwin ]]; then
  echo "The desktop app currently builds on macOS." >&2
  exit 1
fi

task_root="$(cd "$(dirname "$0")/.." && pwd)"
task_output="${1:-${TMPDIR:-/tmp}/steve-desktop-build}"
mkdir -p "$task_output"
task_output="$(cd "$task_output" && pwd)"
task_app="$task_output/Steve.app"
task_arch="$(uname -m)"
case "$task_arch" in
  arm64) task_goarch=arm64 ;;
  x86_64) task_goarch=amd64 ;;
  *) echo "Unsupported desktop architecture: $task_arch" >&2; exit 1 ;;
esac

# Node packages travel to a machine over SSH byte for byte, and a slow home
# link can spend minutes on one. Packing the Linux builds halves what goes
# over the wire for a few hundred milliseconds of decompression on each
# start. macOS builds are left alone: upx refuses Mach-O ("macOS is
# currently not supported"), and packing one would break the ad-hoc
# signature applied below.
task_upx="$(command -v upx || true)"
if [[ "${STEVE_DESKTOP_SKIP_UPX:-0}" == 1 ]]; then
  task_upx=""
elif [[ -z "$task_upx" ]]; then
  echo "warning: upx was not found, so Linux node packages ship uncompressed and take about twice as long to upload. Install it with: brew install upx" >&2
fi

# pack compresses one Linux build in place and reads it back, so a package
# that cannot be unpacked fails the build here rather than on a machine
# halfway through an install.
pack() {
  [[ -n "$task_upx" ]] || return 0
  "$task_upx" -q "$1" >/dev/null
  "$task_upx" -qt "$1" >/dev/null
}

if [[ -e "$task_app" ]]; then
  echo "Output already exists: $task_app. Choose another output directory." >&2
  exit 1
fi

# Build through a staging directory so an unsuccessful compile never publishes
# a partially assembled application. No source-tree output is needed.
task_stage="$(mktemp -d "$task_output/.steve-build.XXXXXX")"
trap 'rm -rf "$task_stage"' EXIT
task_bundle="$task_stage/Steve.app"
mkdir -p "$task_bundle/Contents/MacOS" "$task_bundle/Contents/Resources"

if [[ "${STEVE_DESKTOP_SKIP_CONSOLE_BUILD:-0}" != 1 ]]; then
  # The lockfile pins the host each package came from. A local npm mirror
  # would otherwise be substituted for it, and a package the mirror does not
  # carry fails the install after node_modules has already been emptied.
  (cd "$task_root/web/console" && npm ci --no-audit --no-fund --replace-registry-host=never && npm run build)
fi

(cd "$task_root" && GOOS=darwin GOARCH="$task_goarch" CGO_ENABLED=0 "${GO:-go}" build -trimpath -o "$task_bundle/Contents/Resources/steve" ./cmd/steve)
for task_platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  task_nodeos="${task_platform%/*}"
  task_nodearch="${task_platform#*/}"
  task_nodepath="$task_bundle/Contents/Resources/node-binaries/$task_nodeos-$task_nodearch/steve-node"
  mkdir -p "$(dirname "$task_nodepath")"
  (cd "$task_root" && GOOS="$task_nodeos" GOARCH="$task_nodearch" CGO_ENABLED=0 "${GO:-go}" build -trimpath -o "$task_nodepath" ./cmd/steve-node)
  task_peerpath="$task_bundle/Contents/Resources/peer-binaries/$task_nodeos-$task_nodearch/steve"
  mkdir -p "$(dirname "$task_peerpath")"
  (cd "$task_root" && GOOS="$task_nodeos" GOARCH="$task_nodearch" CGO_ENABLED=0 "${GO:-go}" build -trimpath -o "$task_peerpath" ./cmd/steve)
  if [[ "$task_nodeos" == darwin ]]; then
    codesign --force --sign - "$task_nodepath"
    codesign --force --sign - "$task_peerpath"
  else
    pack "$task_nodepath"
    pack "$task_peerpath"
  fi
done
xcrun clang -fobjc-arc -fmodules -Wall -Wextra -Wno-unused-parameter \
  -arch "$task_arch" -mmacosx-version-min=13.0 -framework Cocoa -framework WebKit \
  "$task_root/desktop/macos/main.m" -o "$task_bundle/Contents/MacOS/Steve"
cp "$task_root/desktop/macos/Info.plist" "$task_bundle/Contents/Info.plist"
bash "$task_root/scripts/build-app-icon.sh" "$task_root/desktop/macos/Assets/AppIcon.png" "$task_bundle/Contents/Resources/AppIcon.icns"
plutil -lint "$task_bundle/Contents/Info.plist"
codesign --force --sign - "$task_bundle/Contents/Resources/steve"
codesign --force --sign - "$task_bundle"
codesign --verify --deep --strict "$task_bundle"
mv "$task_bundle" "$task_app"
printf '%s\n' "$task_app"

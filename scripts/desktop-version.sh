#!/bin/bash
# Version stamping shared by build-desktop.sh and test-desktop.sh. Source it.
#
# The version comes from STEVE_VERSION when the release workflow sets it
# (the tag, with or without its leading "v"), otherwise from
# `git describe` of the newest v<major>.<minor>.<patch> tag, otherwise the
# dev marker 0.0.0-dev.

# desktop_version prints the version without a leading "v": "0.2.0",
# "0.3.0-rc.1", or for a source build "0.2.0-10-g79b152d" (+"-dirty").
desktop_version() {
  local root="$1" version="${STEVE_VERSION:-}"
  if [[ -z "$version" ]]; then
    version="$(git -C "$root" describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' --dirty 2>/dev/null || true)"
    [[ -n "$version" ]] || version="0.0.0-dev"
  fi
  version="${version#v}"
  if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]*)?$ ]]; then
    echo "Not a release version: ${STEVE_VERSION:-$version} (expected v<major>.<minor>.<patch>[-suffix])" >&2
    return 1
  fi
  printf '%s\n' "$version"
}

# The -ldflags GoReleaser gives each binary (.goreleaser.yaml), so a release
# app's copies print the same `steve version` as the release archives. A
# source build sets no release and reports its module build information, as
# `go build` does; injecting a `git describe` string there would print the
# commit twice.
desktop_steve_ldflags() {
  if [[ -n "${STEVE_VERSION:-}" ]]; then
    printf -- '-s -w -X github.com/gopact-ai/steve/internal/app.ReleaseVersion=%s\n' "$1"
  else
    printf -- '-s -w\n'
  fi
}
desktop_node_ldflags() { printf -- '-s -w\n'; }

# stamp_info_plist writes the version into a copied Info.plist. Apple wants
# both keys as period-separated integers, so they carry the numeric
# release (0.3.0 for 0.3.0-rc.1); SteveVersion keeps the full string.
stamp_info_plist() {
  local plist="$1" version="$2" numeric
  numeric="${version%%[-+]*}"
  plutil -replace CFBundleShortVersionString -string "$numeric" "$plist"
  plutil -replace CFBundleVersion -string "$numeric" "$plist"
  plutil -replace SteveVersion -string "$version" "$plist"
}

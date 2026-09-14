#!/bin/bash
set -euo pipefail

task_source="$1"
task_output="$2"
task_temp="$(mktemp -d "${TMPDIR:-/tmp}/steve-icon.XXXXXX")"
trap 'rm -rf "$task_temp"' EXIT
task_iconset="$task_temp/AppIcon.iconset"
mkdir -p "$task_iconset"
for task_size in 16 32 128 256 512; do
  sips -z "$task_size" "$task_size" "$task_source" --out "$task_iconset/icon_${task_size}x${task_size}.png" >/dev/null
  task_retina=$((task_size * 2))
  sips -z "$task_retina" "$task_retina" "$task_source" --out "$task_iconset/icon_${task_size}x${task_size}@2x.png" >/dev/null
done
iconutil --convert icns --output "$task_output" "$task_iconset"

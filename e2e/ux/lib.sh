#!/usr/bin/env bash
# Shared helpers for real-Feishu UX scenarios. Sourced, not executed.
set -u
# Personal identifiers stay out of the repo: they live in e2e/ux/.env
# (gitignored). Copy .env.example and fill in your own chat and bot.
_here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "$_here/.env" ]; then
  . "$_here/.env"
fi
CHAT="${STEVE_UX_CHAT:-}"
BOT="${STEVE_UX_BOT:-}"
if [ -z "$CHAT" ] || [ -z "$BOT" ]; then
  echo "e2e/ux: STEVE_UX_CHAT and STEVE_UX_BOT are required (copy e2e/ux/.env.example to e2e/ux/.env)" >&2
  exit 1
fi
AT="<at user_id=\"$BOT\"></at>"

send() { # send <text> -> message_id
  lark-cli im +messages-send --chat-id "$CHAT" --text "$1" 2>/dev/null | python3 -c "
import json,sys
raw=sys.stdin.read()
print(json.loads(raw[raw.find('{'):])['data']['message_id'])"
}

list_desc() { lark-cli im +chat-messages-list --chat-id "$CHAT" --order desc 2>/dev/null; }

# poll <max_iters> <checker.py>: the checker reads the message list on stdin,
# env carries M1..; prints WAIT / DONE... / FAILED...; poll echoes each verdict.
poll() {
  local iters=$1 checker=$2 verdict=""
  for i in $(seq 1 "$iters"); do
    sleep 10
    verdict=$(list_desc | python3 "$checker")
    echo "poll $i: $verdict"
    case "$verdict" in DONE*|FAILED*) break;; esac
  done
  LAST_VERDICT="$verdict"
}

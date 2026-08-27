#!/usr/bin/env bash
# Shared helpers for real-Feishu UX scenarios. Sourced, not executed.
set -u
CHAT="${STEVE_UX_CHAT:-oc_70ccf0fa8750c6fcdc343b394cf76ae8}"
BOT="${STEVE_UX_BOT:-ou_c831d01122062d06c7511278da1063dd}"
AT="<at user_id=\"$BOT\"></at>"

send() { # send <text> -> message_id
  lark-cli im +messages-send --chat-id "$CHAT" --text "$1" 2>/dev/null | python3 -c "
import json,sys
raw=sys.stdin.read()
print(json.loads(raw[raw.find('{'):])['data']['message_id'])"
}

list_desc() { lark-cli im +chat-messages-list --chat-id "$CHAT" --order desc 2>/dev/null; }

# poll <max_iters> <python_checker>: checker reads the message list on stdin,
# env carries M1..; prints WAIT / DONE.../ FAILED...; poll echoes each verdict.
poll() {
  local iters=$1 checker=$2 verdict=""
  for i in $(seq 1 "$iters"); do
    sleep 10
    verdict=$(list_desc | python3 -c "$checker")
    echo "poll $i: $verdict"
    case "$verdict" in DONE*|FAILED*) break;; esac
  done
  LAST_VERDICT="$verdict"
}

#!/usr/bin/env bash
# 定时任务：/at 建一次性任务并真的到点开跑；/every 进列表、能取消。
cd "$(dirname "$0")" && . ./lib.sh
export MA=$(send "$AT /at 2m 只回复一行：SCHED-ONCE-OK")
echo "MA=$MA"
poll 6 checks/schedmade.py
case "$LAST_VERDICT" in DONE*) echo SCHEDULE-CREATE-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

export ME=$(send "$AT /every 每天 09:00 汇报昨天的进展")
sleep 6
export MS=$(send "$AT /schedules")
echo "ME=$ME MS=$MS"
poll 6 checks/schedlist.py
case "$LAST_VERDICT" in DONE*) echo SCHEDULE-LIST-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac
EVERY_ID=$(echo "$LAST_VERDICT" | sed -n 's/.*every_id=\([0-9]*\).*/\1/p')
echo "EVERY_ID=$EVERY_ID"

# 到点开跑：一次性任务必须自己发出「⏰ 开跑」并跑出真实回答。
poll 30 checks/schedfire.py
case "$LAST_VERDICT" in DONE*) echo SCHEDULE-FIRE-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

export MX=$(send "$AT /schedules cancel $EVERY_ID")
echo "MX=$MX"
poll 6 checks/schedcancel.py
case "$LAST_VERDICT" in DONE*) echo SCHEDULE-CANCEL-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

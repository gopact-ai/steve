#!/usr/bin/env bash
# 顺序（最终卡落在里程碑下方、旧占位卡撤回）、几分之几角标、同族尾标。
cd "$(dirname "$0")" && . ./lib.sh
export M1=$(send "$AT 任务分两阶段。开始时立刻用 channel_send 发一张进度卡并带 progress 参数 \"1/2\"；阶段A 完成后用 channel_update 更新同一张卡，progress 改为 \"2/2\"。最后给出合并最终答案。")
echo "M1=$M1"
poll 36 checks/order.py
case "$LAST_VERDICT" in "DONE tail"*) echo ORDER-BADGE-TAIL-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

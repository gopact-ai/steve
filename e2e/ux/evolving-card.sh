#!/usr/bin/env bash
# 单卡演进：channel_send 一张进度卡，此后只 channel_update，不刷屏。
cd "$(dirname "$0")" && . ./lib.sh
export M1=$(send "$AT 任务分两阶段。开始时先用 channel_send 发一张进度卡（内容：阶段A 进行中）；阶段A 完成后不要发新卡，用 channel_update 把同一张卡更新为「阶段A ✅ / 阶段B 进行中」；阶段B 完成后再次更新为双阶段 ✅。最后给出合并最终答案。")
echo "M1=$M1"
poll 36 checks/evolve.py
case "$LAST_VERDICT" in "DONE single_card"*) echo EVOLVING-CARD-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

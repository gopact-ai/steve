#!/usr/bin/env bash
# 多阶段任务：中途里程碑卡（不 @ 人）→ 最终卡 @ 提问人 → feishu_recall 真撤回。
cd "$(dirname "$0")" && . ./lib.sh
export M1=$(send "$AT 一个两阶段任务：阶段1——用两三句话总结 Go context 包的取消传播机制，完成后立刻把结论作为里程碑用 feishu_send 发出来；阶段2——写一个不超过20行的示例。最终回答合并两个阶段。")
echo "M1=$M1"
poll 36 checks/milestone.py
case "$LAST_VERDICT" in "DONE final"*) echo MILESTONE-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

export M2=$(send "$AT 测试撤回：用 feishu_send 发一条内容为「这条会被撤回」的卡片，拿到 message_id 后立刻用 feishu_recall 撤回它，最后报告撤回结果与该 message_id。")
echo "M2=$M2"
poll 30 checks/recall.py
case "$LAST_VERDICT" in "DONE recalled=True") echo RECALL-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

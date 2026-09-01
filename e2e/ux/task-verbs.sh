#!/usr/bin/env bash
# /tasks 管理动词：暂停停下当前轮、恢复真的续跑、取消是终局、详情带尝试轨迹。
cd "$(dirname "$0")" && . ./lib.sh
export M1=$(send "$AT 分五步详细解释 Go 调度器的 GMP 模型，每一步都认真展开写")
sleep 8
export MP=$(send "$AT /tasks pause")
echo "M1=$M1 MP=$MP"
poll 12 checks/taskpause.py
case "$LAST_VERDICT" in DONE*) echo TASK-PAUSE-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

export MR=$(send "$AT /tasks resume")
echo "MR=$MR"
poll 30 checks/taskresume.py
case "$LAST_VERDICT" in DONE*) echo TASK-RESUME-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

export MC=$(send "$AT /tasks cancel")
echo "MC=$MC"
poll 12 checks/taskcancel.py
case "$LAST_VERDICT" in DONE*) echo TASK-CANCEL-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

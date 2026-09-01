#!/usr/bin/env bash
# 离线送达：跑得久且期间没人说话时，答案卡之外补一条 @ 提问人的纯文本提醒。
# 前置：网关 offline_reminder_after 设得比这轮耗时短（测试用 1m）。
cd "$(dirname "$0")" && . ./lib.sh
export M1=$(send "$AT 分十步详细展开解释 Go 运行时的调度、内存分配与 GC，每一步至少写 200 字")
echo "M1=$M1  （期间不要再发消息）"
poll 40 checks/offline.py
case "$LAST_VERDICT" in DONE*) echo OFFLINE-REMINDER-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

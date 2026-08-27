#!/usr/bin/env bash
# 排队为默认（裸消息排队不杀当前轮），随后「!」打断替换。
cd "$(dirname "$0")" && . ./lib.sh
export M1=$(send "$AT 写一份简短的 goroutine 泄漏排查清单，5 条以内")
sleep 6
export M2=$(send "$AT 再补一条：channel 关闭时的注意事项")
echo "M1=$M1 M2=$M2"
poll 30 checks/queue.py
case "$LAST_VERDICT" in DONE*) echo QUEUE-TEST-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

export M3=$(send "$AT 分五步详细解释 Go 调度器的 GMP 模型，每一步都认真展开写")
sleep 7
export M4=$(send "$AT !不用展开了，只用一句话概括 GMP 即可")
echo "M3=$M3 M4=$M4"
poll 30 checks/interrupt.py
case "$LAST_VERDICT" in DONE*) echo INTERRUPT-TEST-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

#!/usr/bin/env bash
# 多阶段任务：中途里程碑卡（不 @ 人）→ 最终卡 @ 提问人 → feishu_recall 真撤回。
cd "$(dirname "$0")" && . ./lib.sh
export M1=$(send "$AT 一个两阶段任务：阶段1——用两三句话总结 Go context 包的取消传播机制，完成后立刻把结论作为里程碑用 feishu_send 发出来；阶段2——写一个不超过20行的示例。最终回答合并两个阶段。")
echo "M1=$M1"
poll 36 '\nimport json,sys,os\nraw=sys.stdin.read()\nd=json.loads(raw[raw.find("{"):])["data"]["messages"]\nfinal=milestone=None\nfor m in d:\n    if m.get("reply_to")!=os.environ["M1"] or m["msg_type"]!="interactive": continue\n    names=[x["name"] for x in (m.get("mentions") or [])]\n    if "完成 · 发送给" in m["content"]: final=(m["message_id"],names)\n    elif "已取消" in m["content"] or "失败" in m["content"]: final=("BAD:"+m["content"][:120],names)\n    elif not names: milestone=m["message_id"]\nif final and str(final[0]).startswith("BAD"): print("FAILED", final)\nelif final and milestone: print("DONE final_mentions=%s milestone=%s"%(final[1],milestone))\nelif final: print("DONE-NO-MILESTONE final_mentions=%s"%(final[1],))\nelse: print("WAIT")\n'
case "$LAST_VERDICT" in "DONE final"*) echo MILESTONE-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

export M2=$(send "$AT 测试撤回：用 feishu_send 发一条内容为「这条会被撤回」的卡片，拿到 message_id 后立刻用 feishu_recall 撤回它，最后报告撤回结果与该 message_id。")
echo "M2=$M2"
poll 30 '\nimport json,sys,os\nraw=sys.stdin.read()\nd=json.loads(raw[raw.find("{"):])["data"]["messages"]\nfinal=None; recalled=False\nfor m in d[:10]:\n    if m.get("reply_to")==os.environ["M2"] and m["msg_type"]=="interactive" and "完成 · 发送给" in m["content"]:\n        final=m["message_id"]\n    if m.get("deleted") and m["msg_type"]=="nonsupport":\n        recalled=True\nprint("DONE recalled=%s"%recalled if final else "WAIT")\n'
case "$LAST_VERDICT" in "DONE recalled=True") echo RECALL-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

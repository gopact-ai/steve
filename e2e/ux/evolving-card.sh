#!/usr/bin/env bash
# 单卡演进：feishu_send 一张进度卡，此后只 feishu_update，不刷屏。
cd "$(dirname "$0")" && . ./lib.sh
export M1=$(send "$AT 任务分两阶段。开始时先用 feishu_send 发一张进度卡（内容：阶段A 进行中）；阶段A 完成后不要发新卡，用 feishu_update 把同一张卡更新为「阶段A ✅ / 阶段B 进行中」；阶段B 完成后再次更新为双阶段 ✅。最后给出合并最终答案。")
echo "M1=$M1"
poll 36 '\nimport json,sys,os\nraw=sys.stdin.read()\nd=json.loads(raw[raw.find("{"):])["data"]["messages"]\nfinal=None; milestones=[]\nfor m in d:\n    if m.get("reply_to")!=os.environ["M1"] or m["msg_type"]!="interactive": continue\n    names=[x["name"] for x in (m.get("mentions") or [])]\n    if "完成 · 发送给" in m["content"]: final=names\n    elif "已取消" in m["content"] or "失败" in m["content"]: final=["BAD"]\n    elif not names: milestones.append((m.get("updated",False), m["message_id"]))\nif final==["BAD"]: print("FAILED")\nelif final is None: print("WAIT n_milestones=%d"%len(milestones))\nelif len(milestones)==1 and milestones[0][0]:\n    print("DONE single_card=%s updated=true"%milestones[0][1])\nelse:\n    print("DONE-BUT milestones=%s"%(milestones,))\n'
case "$LAST_VERDICT" in "DONE single_card"*) echo EVOLVING-CARD-PASS;; *) echo CHECK: "$LAST_VERDICT"; exit 1;; esac

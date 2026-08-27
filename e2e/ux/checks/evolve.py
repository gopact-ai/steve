import json,sys,os
raw=sys.stdin.read()
d=json.loads(raw[raw.find("{"):])["data"]["messages"]
final=None; milestones=[]
for m in d:
    if m.get("reply_to")!=os.environ["M1"] or m["msg_type"]!="interactive": continue
    names=[x["name"] for x in (m.get("mentions") or [])]
    if "完成 · 发送给" in m["content"]: final=names
    elif "已取消" in m["content"] or "失败" in m["content"]: final=["BAD"]
    elif not names: milestones.append((m.get("updated",False), m["message_id"]))
if final==["BAD"]: print("FAILED")
elif final is None: print("WAIT n_milestones=%d"%len(milestones))
elif len(milestones)==1 and milestones[0][0]:
    print("DONE single_card=%s updated=true"%milestones[0][1])
else:
    print("DONE-BUT milestones=%s"%(milestones,))

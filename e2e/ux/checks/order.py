import json,sys,os
raw=sys.stdin.read()
d=json.loads(raw[raw.find("{"):])["data"]["messages"]
final=milestone=opener=None
for m in d:
    if m.get("reply_to")!=os.environ["M1"]: continue
    if m.get("deleted"): opener=m; continue
    if m["msg_type"]!="interactive": continue
    names=[x["name"] for x in (m.get("mentions") or [])]
    c=m["content"]
    if "完成 · 发送给" in c: final=m
    elif "已取消" in c or "失败" in c: final={"BAD":c[:100]}
    elif not names: milestone=m
if final and "BAD" in final: print("FAILED", final["BAD"])
elif not final: print("WAIT")
else:
    checks=[]
    if milestone is None: checks.append("no-milestone")
    else:
        mc=milestone["content"]
        if "里程碑" not in mc: checks.append("no-badge")
        if int(milestone["message_position"])>=int(final["message_position"]): checks.append("order-wrong")
    if opener is None: checks.append("opener-not-recalled")
    if not checks:
        tail=[l for l in milestone["content"].split(chr(10)) if "里程碑" in l]
        print("DONE tail=%r final_pos=%s milestone_pos=%s"%(tail, final["message_position"], milestone["message_position"]))
    else:
        print("DONE-BUT "+",".join(checks))

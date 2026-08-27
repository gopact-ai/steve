import json,sys,os
raw=sys.stdin.read()
d=json.loads(raw[raw.find("{"):])["data"]["messages"]
final=milestone=None
for m in d:
    if m.get("reply_to")!=os.environ["M1"] or m["msg_type"]!="interactive": continue
    names=[x["name"] for x in (m.get("mentions") or [])]
    if "完成 · 发送给" in m["content"]: final=(m["message_id"],names)
    elif "已取消" in m["content"] or "失败" in m["content"]: final=("BAD:"+m["content"][:120],names)
    elif not names: milestone=m["message_id"]
if final and str(final[0]).startswith("BAD"): print("FAILED", final)
elif final and milestone: print("DONE final_mentions=%s milestone=%s"%(final[1],milestone))
elif final: print("DONE-NO-MILESTONE final_mentions=%s"%(final[1],))
else: print("WAIT")

import json,sys,os
raw=sys.stdin.read()
d=json.loads(raw[raw.find("{"):])["data"]["messages"]
notice=None
for m in d:
    if m["msg_type"]=="text" and m["content"].startswith("⏰ 定时任务 #"): notice=m
if notice is None:
    print("WAIT notice=no"); sys.exit()
final=None
for m in d:
    if m.get("reply_to")==notice["message_id"] and m["msg_type"]=="interactive":
        if "完成 · 发送给" in m["content"]: final=m
        elif "已取消" in m["content"] or "失败" in m["content"]: final={"BAD":m["content"][:120]}
if final is None:
    print("WAIT notice=%s run=pending"%notice["message_id"]); sys.exit()
if "BAD" in final:
    print("FAILED scheduled run did not complete: %s"%final["BAD"]); sys.exit()
print("DONE notice=%s ran=%s answered=%s"%(
    notice["message_id"], final["message_id"], "SCHED-ONCE-OK" in final["content"]))

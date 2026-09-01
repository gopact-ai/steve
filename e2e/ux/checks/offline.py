import json,sys,os
raw=sys.stdin.read()
d=json.loads(raw[raw.find("{"):])["data"]["messages"]
final=None; ping=None
for m in d:
    if m.get("reply_to")!=os.environ["M1"]: continue
    if m["msg_type"]=="interactive" and "完成 · 发送给" in m["content"]: final=m
    if m["msg_type"]=="text" and "跑了" in m["content"] and "任务 #" in m["content"]: ping=m
if final is None:
    print("WAIT final=pending ping=%s"%("yes" if ping else "no")); sys.exit()
if ping is None:
    print("WAIT final=done ping=pending"); sys.exit()
names=[x["name"] for x in (ping.get("mentions") or [])]
print("DONE final=%s ping=%s ping_mentions=%s text=%r"%(
    final["message_id"], ping["message_id"], names, ping["content"][:80]))

import json,sys,os
raw=sys.stdin.read()
d=json.loads(raw[raw.find("{"):])["data"]["messages"]
c={}
for m in d:
    if m.get("reply_to")==os.environ["M1"]: c["m1"]=m["content"]
    if m.get("reply_to")==os.environ["M2"]: c["m2"]=m["content"]
ok1="完成 · 发送给" in c.get("m1","")
ok2="完成 · 发送给" in c.get("m2","")
print(("DONE" if ok1 and ok2 else "WAIT")+" m1="+("done" if ok1 else "pending")+" m2="+("done" if ok2 else "pending"))

import json,sys,os
raw=sys.stdin.read()
d=json.loads(raw[raw.find("{"):])["data"]["messages"]
c={}
for m in d:
    if m.get("reply_to")==os.environ["M3"]: c["m3"]=c.get("m3","")+m["content"]
    if m.get("reply_to")==os.environ["M4"]: c["m4"]=c.get("m4","")+m["content"]
ok3="已取消" in c.get("m3","")
ok4="完成 · 发送给" in c.get("m4","")
print(("DONE" if ok3 and ok4 else "WAIT")+" m3="+("cancelled" if ok3 else "pending")+" m4="+("done" if ok4 else "pending"))

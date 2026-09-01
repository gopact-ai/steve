import json,sys,os,re
raw=sys.stdin.read()
d=json.loads(raw[raw.find("{"):])["data"]["messages"]
card=""
for m in d:
    if m.get("reply_to")==os.environ["MC"]: card+=m["content"]
if "已取消，不会再被恢复" not in card:
    print("WAIT cancel_card=%s"%("yes" if card else "no")); sys.exit()
tid=re.search(r"任务 #(\d+) 已取消", card)
print("DONE task=%s state=%s"%(tid.group(1) if tid else "?", "cancelled" if "cancelled" in card else "MISSING"))

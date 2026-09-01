import json,sys,os
raw=sys.stdin.read()
d=json.loads(raw[raw.find("{"):])["data"]["messages"]
card=""
for m in d:
    if m.get("reply_to")==os.environ["MX"]: card+=m["content"]
if "已取消" not in card:
    print("WAIT card=%s"%("yes" if card else "no")); sys.exit()
print("DONE cancelled=%s"%("汇报昨天的进展" in card))

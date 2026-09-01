import json,sys,os,re
raw=sys.stdin.read()
d=json.loads(raw[raw.find("{"):])["data"]["messages"]
card=""
for m in d:
    if m.get("reply_to")==os.environ["MA"]: card+=m["content"]
if "已建" not in card:
    print("WAIT card=%s"%("yes" if card else "no")); sys.exit()
sid=re.search(r"定时任务 #(\d+) 已建", card)
nxt=re.search(r"下次 ([0-9:\- ]+)", card)
ok_prompt="SCHED-ONCE-OK" in card
print("DONE id=%s next=%r prompt_kept=%s"%(sid.group(1) if sid else "?", nxt.group(1).strip() if nxt else "?", ok_prompt))

import json,sys,os,re
raw=sys.stdin.read()
d=json.loads(raw[raw.find("{"):])["data"]["messages"]
card=""
for m in d:
    if m.get("reply_to")==os.environ["MS"]: card+=m["content"]
if not card:
    print("WAIT no_listing"); sys.exit()
once="SCHED-ONCE-OK" in card
daily="汇报昨天的进展" in card
every=re.search(r"#(\d+)[^\n]*汇报昨天的进展", card, re.S)
if not every:
    every=None
    for block in re.finditer(r"\*\*#(\d+)\*\*(.*?)(?=\*\*#|\Z)", card, re.S):
        if "汇报昨天的进展" in block.group(2): every=block
if once and daily and every:
    print("DONE every_id=%s both_listed=true"%every.group(1))
else:
    print("WAIT once=%s daily=%s"%(once, daily))

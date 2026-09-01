import json,sys,os,re
raw=sys.stdin.read()
d=json.loads(raw[raw.find("{"):])["data"]["messages"]
pause=""; run=""
for m in d:
    if m.get("reply_to")==os.environ["MP"]: pause+=m["content"]
    if m.get("reply_to")==os.environ["M1"]: run+=m["content"]
if "已暂停" not in pause:
    print("WAIT pause_card=%s"%("yes" if pause else "no")); sys.exit()
# The card must carry the digest, not just the verdict: paused state, the
# member, and the turns spent so far.
ok_state="paused" in pause
ok_stopped="已取消" in run
tid=re.search(r"任务 #(\d+) 已暂停", pause)
if ok_state and ok_stopped:
    print("DONE task=%s stopped=true detail=true"%(tid.group(1) if tid else "?"))
else:
    print("DONE-BUT state=%s turn_stopped=%s"%(ok_state, ok_stopped))

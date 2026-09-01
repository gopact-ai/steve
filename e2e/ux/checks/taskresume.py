import json,sys,os,re
raw=sys.stdin.read()
d=json.loads(raw[raw.find("{"):])["data"]["messages"]
card=""; notice=None
for m in d:
    if m.get("reply_to")==os.environ["MR"]: card+=m["content"]
    if m["msg_type"]=="text" and m["content"].startswith("⟳ 继续任务 #"): notice=m
if not card:
    print("WAIT no_command_card"); sys.exit()
if notice is None:
    print("WAIT notice=no"); sys.exit()
# The continuation has to be a real turn: a card replying to the notice that
# finished and addressed the asker.
final=None
for m in d:
    if m.get("reply_to")==notice["message_id"] and m["msg_type"]=="interactive":
        if "完成 · 发送给" in m["content"]: final=m
        elif "已取消" in m["content"] or "失败" in m["content"]: final={"BAD":m["content"][:120]}
if final is None:
    print("WAIT notice=%s turn=pending"%notice["message_id"]); sys.exit()
if "BAD" in final:
    print("FAILED resumed turn did not complete: %s"%final["BAD"]); sys.exit()
names=[x["name"] for x in (final.get("mentions") or [])]
print("DONE notice=%s final=%s mentions=%s"%(notice["message_id"], final["message_id"], names))

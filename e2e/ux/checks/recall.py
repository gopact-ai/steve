import json,sys,os
raw=sys.stdin.read()
d=json.loads(raw[raw.find("{"):])["data"]["messages"]
final=None; recalled=False
for m in d[:10]:
    if m.get("reply_to")==os.environ["M2"] and m["msg_type"]=="interactive" and "完成 · 发送给" in m["content"]:
        final=m["message_id"]
    if m.get("deleted") and m["msg_type"]=="nonsupport":
        recalled=True
print("DONE recalled=%s"%recalled if final else "WAIT")

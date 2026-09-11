#!/usr/bin/env python
# 取前端脚本源码（修正：Debugger.enable 会重放 scriptParsed，需全程收集）
import json, os, sys, urllib.request, websocket, time

PORT = 9333
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "js_src")
os.makedirs(OUT, exist_ok=True)

tl = json.loads(urllib.request.urlopen(f"http://127.0.0.1:{PORT}/json/list", timeout=3).read().decode())
page = [t for t in tl if t.get("type") == "page"][0]
ws = websocket.create_connection(page["webSocketDebuggerUrl"], timeout=30)
ws.settimeout(2)

ids = [0]


def send(method, params=None):
    ids[0] += 1
    ws.send(json.dumps({"id": ids[0], "method": method, "params": params or {}}))
    return ids[0]


scripts = {}
pend = set()
pend.add(send("Debugger.enable"))

deadline = time.time() + 8
while time.time() < deadline:
    try:
        m = json.loads(ws.recv())
    except Exception:
        continue
    if m.get("method") == "Debugger.scriptParsed":
        p = m["params"]
        scripts[p["scriptId"]] = (p.get("url", ""), p.get("length", 0))
    if m.get("id") in pend:
        pend.discard(m["id"])

print("脚本总数:", len(scripts))
big = sorted(((ln, sid, u) for sid, (u, ln) in scripts.items() if ln > 30000), reverse=True)
for ln, sid, u in big:
    print(f"  {ln:>10}  {u[:120]}")

# 取源码
for ln, sid, u in big:
    i = send("Debugger.getScriptSource", {"scriptId": sid})
    ws.settimeout(120)
    src = None
    while True:
        m = json.loads(ws.recv())
        if m.get("id") == i:
            src = m.get("result", {}).get("scriptSource")
            break
    ws.settimeout(2)
    if not src:
        continue
    name = (u.rsplit("/", 1)[-1] or ("inline_" + sid)).split("?")[0] or ("inline_" + sid)
    p = os.path.join(OUT, name)
    open(p, "w", encoding="utf-8").write(src)
    print(f"  saved {len(src):>9}  {name}")

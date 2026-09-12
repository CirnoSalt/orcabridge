#!/usr/bin/env python
"""直连上游 dump 原始响应（诊断用）。

代理侧解析失败时，用这个脚本拿上游的**原始字节**，避免靠猜。
请求结构与代理 chatBody 保持一致（tools 开关可控）。

用法：
    .venv-cdp/Scripts/python.exe tools/reverse/raw_dump.py "把你的问题写在这里"
    .venv-cdp/Scripts/python.exe tools/reverse/raw_dump.py --tools "调用 fetch 抓取 https://example.com"
"""
import base64
import json
import os
import sys
import urllib.request
import uuid

COOKIES = os.path.join(os.environ["LOCALAPPDATA"], "com.orcaterm-desktop.app", ".cookies")
UP = "https://lightai.cloud.tencent.com"
UA = ("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "
      "(KHTML, like Gecko) OrcaTerm/1.0.0 Chrome/120.0.0.0 Safari/537.36")
CHAT = "/assistant/chat?name=orcaterm&mode=0&csrfCode=&sseresume=true"


def creds():
    arr = json.load(open(COOKIES, encoding="utf-8"))
    sid = ot = None
    for it in arr:
        d = it.get("domain") or {}
        dom = d.get("HostOnly") or d.get("Suffix") or ""
        raw = (it.get("raw_cookie") or "").split(";")[0].strip()
        if "=" not in raw:
            continue
        k, v = raw.split("=", 1)
        if "lightai.cloud.tencent.com" in dom and k == "sid":
            sid = v
        if "orcaterm.cloud.tencent.com" in dom and k == "ot_session":
            ot = v
    return sid, ot


def jwt_uid(jwt):
    seg = jwt.split(".")[1]
    seg += "=" * (-len(seg) % 4)
    return json.loads(base64.urlsafe_b64decode(seg)).get("userId")


def acp(text):
    return json.dumps({"_format": "acp-prompt", "version": 1,
                       "prompt": [{"type": "text", "text": text}]}, ensure_ascii=False)


def main():
    args = [a for a in sys.argv[1:]]
    tools = "--tools" in args
    args = [a for a in args if a != "--tools"]
    prompt = args[0] if args else "1+1等于几？只回答数字。"

    sid, ot = creds()
    uid = str(jwt_uid(ot))
    cid = "cid-" + str(uuid.uuid4()) + "-orcaterm"
    body = {
        "conversationId": cid,
        "user": {"id": uid, "setting": {
            "mcpServers": ["mcp-server-orcaterm-oauth"] if tools else [],
            "uiServers": ["ui-tools-orcaterm-explorer"] if tools else [],
            "tools": [], "approvedTools": [], "approvedMCPTools": [],
            "enableAutoSubtaskExecution": False, "env": [],
            "model": "TokenHub/deepseek-v4-flash", "modelDesc": {},
            "hideToolMessage": True, "isNormalToolMessageIgnored": True,
            "shouldHideDirectOutputToolReview": True,
        }},
        "input": {"type": "start", "input": acp(prompt),
                  "id": str(uuid.uuid4()), "emphasisPrompt": "", "files": []},
        "stream": False,
    }
    req = urllib.request.Request(UP + CHAT, data=json.dumps(body, ensure_ascii=False).encode(), method="POST")
    for k, v in [
        ("Content-Type", "application/json"),
        ("Accept", "text/event-stream"),
        ("Authorization", "Bearer " + ot),
        ("Cookie", f"ot_session={ot}; userAccountLoginMethod=oauth; userAccountType=wechat; userId={uid}"),
        ("X-Product", "orcaterm"),
        ("X-Seq-Id", str(uuid.uuid4())),
        ("X-Csrfcode", ""),
        ("X-Referer", "http://tauri.localhost/"),
        ("Origin", "http://tauri.localhost"),
        ("User-Agent", UA),
    ]:
        req.add_header(k, v)
    with urllib.request.urlopen(req, timeout=180) as r:
        raw = r.read().decode("utf-8", "replace")

    print("prompt:", prompt)
    print("tools :", tools)
    print("bytes :", len(raw))
    print("--- raw head ---")
    print(raw[:600])
    print("--- decoded types ---")
    try:
        obj = json.loads(raw)
    except Exception as exc:  # noqa: BLE001
        print("非 JSON:", exc)
        return
    print("top-level keys:", list(obj))
    for k, v in obj.items():
        if k == "data":
            print(f"  data.type = dict, keys={list(v) if isinstance(v, dict) else type(v)}")
            continue
        print(f"  {k} = {v!r} ({type(v).__name__})")


if __name__ == "__main__":
    main()

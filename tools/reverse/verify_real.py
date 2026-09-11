#!/usr/bin/env python
# 用抓到的真实请求结构验证：input 为嵌套对象 + user.id + sseresume
import json, os, time, urllib.request, urllib.error, uuid, base64

COOKIES = os.path.join(os.environ["LOCALAPPDATA"], "com.orcaterm-desktop.app", ".cookies")
UP = "https://lightai.cloud.tencent.com"
UA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) OrcaTerm/1.0.0 Chrome/120.0.0.0 Safari/537.36"


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


def post(sid, ot, path, body):
    data = json.dumps(body, ensure_ascii=False).encode()
    req = urllib.request.Request(UP + path, data=data, method="POST")
    for k, v in [
        ("Content-Type", "application/json"),
        ("Accept", "text/event-stream"),
        ("Authorization", "Bearer " + ot),
        ("Cookie", f"ot_session={ot}; userAccountLoginMethod=oauth; userAccountType=wechat; userId={jwt_uid(ot)}"),
        ("X-Product", "orcaterm"),
        ("X-Seq-Id", str(uuid.uuid4())),
        ("X-Csrfcode", ""),
        ("X-Referer", "http://tauri.localhost/"),
        ("Origin", "http://tauri.localhost"),
        ("User-Agent", UA),
    ]:
        req.add_header(k, v)
    r = urllib.request.urlopen(req, timeout=120)
    return r.read().decode("utf-8", "replace")


def acp(t):
    return json.dumps({"_format": "acp-prompt", "version": 1,
                       "prompt": [{"type": "text", "text": t}]}, ensure_ascii=False)


def main():
    sid, ot = creds()
    uid = str(jwt_uid(ot))
    print("userId:", uid)
    for q in ["1+1等于几？只回答数字。", "用一句话介绍杭州。"]:
        cid = "cid-" + str(uuid.uuid4()) + "-orcaterm"
        body = {
            "conversationId": cid,
            "user": {"id": uid, "setting": {
                "mcpServers": ["mcp-server-orcaterm-oauth"],
                "uiServers": ["ui-tools-orcaterm-explorer"],
                "tools": [], "approvedTools": [], "approvedMCPTools": [],
                "enableAutoSubtaskExecution": False, "env": [],
                "model": "TokenHub/deepseek-v4-flash", "modelDesc": {},
            }},
            "input": {"type": "start", "input": acp(q),
                      "id": str(uuid.uuid4()), "emphasisPrompt": "", "files": []},
            "stream": False,
        }
        raw = post(sid, ot, "/assistant/chat?name=orcaterm&mode=0&csrfCode=&sseresume=true", body)
        try:
            d = json.loads(raw).get("data") or {}
        except Exception:
            print("非JSON:", raw[:300]); continue
        print(f"\n=== Q={q!r}")
        print("  action:", d.get("action"))
        print("  thinking:", (d.get("thinking") or "")[:200])
        print("  taskCompletion:", (d.get("taskCompletion") or "")[:300])


if __name__ == "__main__":
    main()

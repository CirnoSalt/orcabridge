#!/usr/bin/env python
# 用正确协议验证：多轮上下文、消息读回、usage、多模态、tools
import json, os, time, urllib.request, urllib.error, uuid, base64

COOKIES = os.path.join(os.environ["LOCALAPPDATA"], "com.orcaterm-desktop.app", ".cookies")
UP = "https://lightai.cloud.tencent.com"
UA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) OrcaTerm/1.0.0 Chrome/120.0.0.0 Safari/537.36"
MODEL = "TokenHub/deepseek-v4-flash"


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
    s = jwt.split(".")[1]
    s += "=" * (-len(s) % 4)
    return json.loads(base64.urlsafe_b64decode(s)).get("userId")


def req(sid, ot, method, path, body=None):
    data = json.dumps(body, ensure_ascii=False).encode() if body is not None else None
    r = urllib.request.Request(UP + path, data=data, method=method)
    uid = str(jwt_uid(ot))
    for k, v in [
        ("Content-Type", "application/json"),
        ("Accept", "text/event-stream"),
        ("Authorization", "Bearer " + ot),
        ("Cookie", f"ot_session={ot}; userAccountLoginMethod=oauth; userAccountType=wechat; userId={uid}"),
        ("X-Product", "orcaterm"), ("X-Seq-Id", str(uuid.uuid4())),
        ("X-Csrfcode", ""), ("X-Referer", "http://tauri.localhost/"),
        ("Origin", "http://tauri.localhost"), ("User-Agent", UA),
    ]:
        r.add_header(k, v)
    try:
        resp = urllib.request.urlopen(r, timeout=120)
        return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")
    except Exception as e:
        return -1, repr(e)[:150]


def acp(t):
    return json.dumps({"_format": "acp-prompt", "version": 1,
                       "prompt": [{"type": "text", "text": t}]}, ensure_ascii=False)


def chat(sid, ot, cid, text, uid, stream=False):
    body = {
        "conversationId": cid,
        "user": {"id": uid, "setting": {
            "mcpServers": ["mcp-server-orcaterm-oauth"],
            "uiServers": ["ui-tools-orcaterm-explorer"],
            "tools": [], "approvedTools": [], "approvedMCPTools": [],
            "enableAutoSubtaskExecution": False, "env": [],
            "model": MODEL, "modelDesc": {}}},
        "input": {"type": "start", "input": acp(text),
                  "id": str(uuid.uuid4()), "emphasisPrompt": "", "files": []},
        "stream": stream,
    }
    return req(sid, ot, "POST", "/assistant/chat?name=orcaterm&mode=0&csrfCode=&sseresume=true", body)


def main():
    sid, ot = creds()
    uid = str(jwt_uid(ot))
    cid = "cid-" + str(uuid.uuid4()) + "-orcaterm"
    print("会话:", cid)

    print("\n########## 1. 多轮上下文 ##########")
    st, raw = chat(sid, ot, cid, "我叫小明，请记住我的名字，只回复“记住了”。", uid)
    d = json.loads(raw).get("data") or {}
    print("  轮1 ->", (d.get("taskCompletion") or "")[:120])
    print("  完整响应键:", list(json.loads(raw).keys()), "| data键:", list(d.keys()))

    st, raw2 = chat(sid, ot, cid, "我叫什么名字？只回答名字。", uid)
    d2 = json.loads(raw2).get("data") or {}
    print("  轮2 ->", (d2.get("taskCompletion") or "")[:200])

    print("\n########## 2. 消息读回 ##########")
    st3, raw3 = req(sid, ot, "GET", f"/assistant/messages/{cid}?name=orcaterm&mode=0&csrfCode=")
    print("  status:", st3, "| len:", len(raw3))
    print("  ", raw3[:600])

    print("\n########## 3. usage / token 信息 ##########")
    full = json.loads(raw2)
    print("  顶层:", json.dumps({k: v for k, v in full.items() if k != "data"}, ensure_ascii=False)[:300])
    print("  data 全部键:", list((full.get("data") or {}).keys()))
    for k in ("usage", "tokens", "tokenUsage", "promptTokens", "completionTokens"):
        if k in full or k in (full.get("data") or {}):
            print("  发现用量字段:", k)


if __name__ == "__main__":
    main()

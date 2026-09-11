#!/usr/bin/env python
# 触发工具调用：以真实客户端 setting 发请求，打印上游完整响应（含 tool 字段）
import json, os, sys, time, urllib.request, urllib.error, uuid, base64

COOKIES = os.path.join(os.environ["LOCALAPPDATA"], "com.orcaterm-desktop.app", ".cookies")
UP = "https://lightai.cloud.tencent.com"
UA = ("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "
      "(KHTML, like Gecko) OrcaTerm/1.0.0 Chrome/120.0.0.0 Safari/537.36")
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


def call(sid, ot, path, body, stream=False):
    data = json.dumps(body, ensure_ascii=False).encode()
    r = urllib.request.Request(UP + path, data=data, method="POST")
    uid = str(jwt_uid(ot))
    for k, v in [
        ("Content-Type", "application/json"), ("Accept", "text/event-stream"),
        ("Authorization", "Bearer " + ot),
        ("Cookie", f"ot_session={ot}; userAccountLoginMethod=oauth; userAccountType=wechat; userId={uid}"),
        ("X-Product", "orcaterm"), ("X-Seq-Id", str(uuid.uuid4())),
        ("X-Csrfcode", ""), ("X-Referer", "http://tauri.localhost/"),
        ("Origin", "http://tauri.localhost"), ("User-Agent", UA),
    ]:
        r.add_header(k, v)
    try:
        resp = urllib.request.urlopen(r, timeout=180)
        if stream:
            chunks = []
            while True:
                b = resp.read(4096)
                if not b:
                    break
                chunks.append(b)
            return resp.status, b"".join(chunks).decode("utf-8", "replace")
        return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")
    except Exception as e:
        return -1, repr(e)[:200]


def chat(sid, ot, cid, text, setting_extra=None, stream=False, parts=None):
    setting = {
        "mcpServers": ["mcp-server-orcaterm-oauth"],
        "uiServers": ["ui-tools-orcaterm-explorer"],
        "tools": [], "approvedTools": [], "approvedMCPTools": [],
        "enableAutoSubtaskExecution": False, "env": [],
        "model": MODEL, "modelDesc": {},
    }
    if setting_extra:
        setting.update(setting_extra)
    env = {"_format": "acp-prompt", "version": 1,
           "prompt": parts or [{"type": "text", "text": text}]}
    body = {
        "conversationId": cid, "user": {"id": str(jwt_uid(ot)), "setting": setting},
        "input": {"type": "start", "input": json.dumps(env, ensure_ascii=False),
                  "id": str(uuid.uuid4()), "emphasisPrompt": "", "files": []},
        "stream": stream,
    }
    return call(sid, ot, "/assistant/chat?name=orcaterm&mode=0&csrfCode=&sseresume=true", body, stream)


def main():
    sid, ot = creds()
    uid = str(jwt_uid(ot))
    print("userId:", uid)
    q = sys.argv[1] if len(sys.argv) > 1 else "请调用 fetch 工具抓取 https://example.com 的内容"
    use_stream = "--stream" in sys.argv
    cid = "cid-" + str(uuid.uuid4()) + "-orcaterm"
    print("Q:", q, "| stream:", use_stream, "| cid:", cid)
    st, raw = chat(sid, ot, cid, q, stream=use_stream)
    print("status:", st, "len:", len(raw))
    if use_stream:
        print("=" * 70)
        print(raw[:12000])
        return
    try:
        j = json.loads(raw)
    except Exception:
        print("RAW:", raw[:3000])
        return
    print("=" * 70)
    print("顶层键:", list(j.keys()))
    print(json.dumps(j, ensure_ascii=False, indent=2)[:6000])


if __name__ == "__main__":
    main()

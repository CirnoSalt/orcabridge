#!/usr/bin/env python
# 端到端验证：触发工具调用 -> 按客户端格式回传工具结果 -> 检查模型是否消费结果
import json, os, uuid, base64, urllib.request, urllib.error, sys

COOKIES = os.path.join(os.environ["LOCALAPPDATA"], "com.orcaterm-desktop.app", ".cookies")
UP = "https://lightai.cloud.tencent.com"
UA = ("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "
      "(KHTML, like Gecko) OrcaTerm/1.0.0 Chrome/120.0.0.0 Safari/537.36")
MODEL = "TokenHub/deepseek-v4-flash"
MARKER = "MARKER-8899-TITLE-IS-青柠测试页"

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
    s = jwt.split(".")[1]; s += "=" * (-len(s) % 4)
    return json.loads(base64.urlsafe_b64decode(s)).get("userId")

def post(path, body):
    r = urllib.request.Request(UP + path, data=json.dumps(body, ensure_ascii=False).encode(), method="POST")
    sid, ot = creds()
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
        return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")

def chat(cid, text, extra_setting=None):
    sid, ot = creds()
    setting = {
        "mcpServers": ["mcp-server-orcaterm-oauth"],
        "uiServers": ["ui-tools-orcaterm-explorer"],
        "tools": [], "approvedTools": [], "approvedMCPTools": [],
        "enableAutoSubtaskExecution": False, "env": [],
        "model": MODEL, "modelDesc": {},
    }
    if extra_setting:
        setting.update(extra_setting)
    env = {"_format": "acp-prompt", "version": 1, "prompt": [{"type": "text", "text": text}]}
    return post("/assistant/chat?name=orcaterm&mode=0&csrfCode=&sseresume=true", {
        "conversationId": cid, "user": {"id": str(jwt_uid(ot)), "setting": setting},
        "input": {"type": "start", "input": json.dumps(env, ensure_ascii=False),
                  "id": str(uuid.uuid4()), "emphasisPrompt": "", "files": []},
        "stream": False,
    })

def show(tag, raw):
    try:
        j = json.loads(raw); d = j.get("data") or {}
    except Exception:
        print(f"  [{tag}] 非JSON: {raw[:300]}"); return {}
    print(f"  [{tag}] action={d.get('action')} tool={(d.get('tool') or {}).get('name')}")
    print("     thinking:", (d.get("thinking") or "")[:150])
    print("     tc:", (d.get("taskCompletion") or "")[:400])
    return d

cid = "cid-" + str(uuid.uuid4()) + "-orcaterm"
print("会话:", cid)
print("\n=== 轮1：请求调用 fetch ===")
_, raw = chat(cid, "请调用 fetch 工具抓取 https://example.com 的内容")
d1 = show("轮1", raw)
tool = d1.get("tool") or {}
if not tool:
    print("!! 未触发工具调用，无法继续"); sys.exit(1)

# 客户端格式：<TOOL_META>返回结果</TOOL_META>\n<输出>
result = f"{MARKER}\n页面正文：这是一个用于示例的页面。"
payload = "<TOOL_META>返回结果</TOOL_META>\n" + result
print("\n=== 轮2：回传工具结果（客户端格式）===")
print("  发送文本:", payload.replace("\n", "\\n")[:200])
_, raw2 = chat(cid, payload)
show("轮2", raw2)
print("\n  模型是否消费了结果（出现 MARKER/青柠）:",
      ("青柠" in raw2 or MARKER in raw2))

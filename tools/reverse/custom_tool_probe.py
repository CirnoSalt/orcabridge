#!/usr/bin/env python
# 探测：自定义工具能否被上游模型调用（setting.tools 各种 schema / 提示词注入原生 review 格式）
import json, os, uuid, base64, urllib.request, urllib.error

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
    s = jwt.split(".")[1]; s += "=" * (-len(s) % 4)
    return json.loads(base64.urlsafe_b64decode(s)).get("userId")


def chat(text, setting_extra=None):
    sid, ot = creds()
    uid = str(jwt_uid(ot))
    setting = {
        "mcpServers": ["mcp-server-orcaterm-oauth"],
        "uiServers": ["ui-tools-orcaterm-explorer"],
        "tools": [], "approvedTools": [], "approvedMCPTools": [],
        "enableAutoSubtaskExecution": False, "env": [],
        "model": MODEL, "modelDesc": {},
    }
    if setting_extra:
        setting.update(setting_extra)
    env = {"_format": "acp-prompt", "version": 1, "prompt": [{"type": "text", "text": text}]}
    body = {
        "conversationId": "cid-" + str(uuid.uuid4()) + "-orcaterm",
        "user": {"id": uid, "setting": setting},
        "input": {"type": "start", "input": json.dumps(env, ensure_ascii=False),
                  "id": str(uuid.uuid4()), "emphasisPrompt": "", "files": []},
        "stream": False,
    }
    r = urllib.request.Request(UP + "/assistant/chat?name=orcaterm&mode=0&csrfCode=&sseresume=true",
                              data=json.dumps(body, ensure_ascii=False).encode(), method="POST")
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
        j = json.loads(resp.read().decode("utf-8", "replace"))
        return j.get("data") or {}
    except urllib.error.HTTPError as e:
        return {"error": e.read().decode("utf-8", "replace")[:200]}
    except Exception as e:
        return {"error": repr(e)[:200]}


SCHEMA = {"type": "object",
          "properties": {"city": {"type": "string", "description": "城市名"}},
          "required": ["city"]}

VARIANTS = [
    ("A. tools 内联定义",
     {"tools": [{"name": "get_weather", "description": "查询指定城市天气", "parameters": SCHEMA}]}),
    ("B. tools OpenAI 格式",
     {"tools": [{"type": "function", "function": {"name": "get_weather",
                                                  "description": "查询指定城市天气", "parameters": SCHEMA}}]}),
    ("C. tools 仅名字数组", {"tools": ["get_weather"]}),
    ("D. selectedUITools", {"selectedUITools": ["get_weather"]}),
    ("E. 无 tools（基线）", None),
]

Q = "请调用 get_weather 工具查询北京的天气"

for name, extra in VARIANTS:
    d = chat(Q, extra)
    tool = d.get("tool") or {}
    tc = (d.get("taskCompletion") or "")
    print(f"\n=== {name} ===")
    print(f"  action={d.get('action')} tool={tool.get('name')} err={d.get('error','')}")
    print("  args:", json.dumps(tool.get("args"), ensure_ascii=False)[:160])
    print("  tc:", tc[:220].replace("\n", " "))

# 提示词注入：让模型按原生 review 格式请求自定义工具
INJECT = """[工具调用说明]
你可以调用下列外部工具。当需要调用某个工具时，请严格只输出一个 json 代码块，格式如下（不要输出其它内容）：
```json
{"action":"review","thinking":"简要理由","tool":{"type":"function","name":"工具名","args":{参数对象}}}
```
可用工具：
- get_weather: 查询指定城市天气。参数 {"city": string} 城市名。
若无需调用工具，正常回答即可。"""

print("\n=== F. 提示词注入原生 review 格式 ===")
d = chat(Q + "\n\n" + INJECT)
tool = d.get("tool") or {}
print(f"  action={d.get('action')} tool={tool.get('name')}")
print("  thinking:", (d.get("thinking") or "")[:200])
print("  tc:", (d.get("taskCompletion") or "")[:400].replace("\n", " "))
